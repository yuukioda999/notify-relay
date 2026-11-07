package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// These tests need a MySQL database. CI sets RELAY_TEST_DSN; locally they are skipped without it.
func openTestDB(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{"DROP TABLE IF EXISTS webhook_events", "DROP TABLE IF EXISTS delivery_recipients", "DROP TABLE IF EXISTS deliveries", "DROP TABLE IF EXISTS schema_migrations"} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Store{DB: db, RefKey: testRefKey}
}

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

var testRefKey = domain.EmailRefKey("test-email-ref-key-0000000000001")

func newDelivery(key string, emails ...string) usecase.NewDelivery {
	req := domain.Request{
		DedupeKey: key,
		TenantRef: domain.SystemTenantRef,
		Message:   domain.Message{Subject: "件名", Text: "本文", URL: "http://localhost:5173/x"},
	}
	for i, e := range emails {
		req.Recipients = append(req.Recipients, domain.Recipient{Ref: fmt.Sprintf("r%d", i), Email: e})
	}
	id, _ := domain.NewDeliveryID(t0, nil)
	return usecase.NewDelivery{
		Accepted: usecase.Accepted{ID: id, DedupeKey: key, ContentHash: req.ContentHash(), RecipientCount: len(emails), AcceptedAt: t0},
		Request:  req,
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestDB(t)
	if err := Migrate(context.Background(), s.DB); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestCreateAndFind(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, s *Store)
	}{
		{name: "AC-R001-01 the delivery and every recipient are stored", run: func(t *testing.T, s *Store) {
			d := newDelivery("key-00000001", "a@example.invalid", "b@example.invalid")
			if err := s.Create(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			got, err := s.FindByDedupeKey(context.Background(), "key-00000001")
			if err != nil || got.ID != d.ID || got.ContentHash != d.ContentHash || got.RecipientCount != 2 {
				t.Fatalf("FindByDedupeKey = %+v, %v", got, err)
			}
			if n := count(t, s.DB, `SELECT COUNT(*) FROM delivery_recipients WHERE delivery_id = ? AND status = 'pending'`, string(d.ID)); n != 2 {
				t.Fatalf("pending recipients = %d, want 2", n)
			}
		}},
		{name: "AC-R001-03 a second delivery with the same key is rejected as a duplicate", run: func(t *testing.T, s *Store) {
			if err := s.Create(context.Background(), newDelivery("key-00000002", "a@example.invalid")); err != nil {
				t.Fatal(err)
			}
			err := s.Create(context.Background(), newDelivery("key-00000002", "a@example.invalid"))
			if !errors.Is(err, usecase.ErrDuplicateDedupeKey) {
				t.Fatalf("err = %v, want ErrDuplicateDedupeKey", err)
			}
			if n := count(t, s.DB, `SELECT COUNT(*) FROM delivery_recipients`); n != 1 {
				t.Fatalf("recipients = %d, want 1 (nothing saved for the duplicate)", n)
			}
		}},
		{name: "an unknown key is not found", run: func(t *testing.T, s *Store) {
			if _, err := s.FindByDedupeKey(context.Background(), "missing-key"); !errors.Is(err, usecase.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, openTestDB(t)) })
	}
}

func TestClaimAndRecord(t *testing.T) {
	ctx := context.Background()

	t.Run("AC-R001-15 a delivered recipient loses its address and the delivery completes", func(t *testing.T) {
		s := openTestDB(t)
		d := newDelivery("key-00000010", "a@example.invalid")
		mustCreate(t, s, d)

		jobs, err := s.Claim(ctx, t0, 10)
		if err != nil || len(jobs) != 1 || jobs[0].Email != "a@example.invalid" || jobs[0].Message.URL == "" {
			t.Fatalf("Claim = %+v, %v", jobs, err)
		}
		if again, _ := s.Claim(ctx, t0, 10); len(again) != 0 {
			t.Fatalf("a claimed recipient was handed out again: %+v", again)
		}
		if err := s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0); err != nil {
			t.Fatal(err)
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM delivery_recipients WHERE email IS NOT NULL`); n != 0 {
			t.Fatalf("%d addresses remain after the final status", n)
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM deliveries WHERE completed_at IS NOT NULL`); n != 1 {
			t.Fatal("delivery was not marked complete")
		}
	})

	t.Run("AC-R001-10 a retry keeps the address and waits until the next attempt time", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000011", "a@example.invalid", "b@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 1)
		next := t0.Add(30 * time.Second)
		err := s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusPending, Code: domain.ErrProviderUnavailable, Attempts: 1, NextAttempt: next}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM delivery_recipients WHERE email IS NOT NULL`); n != 2 {
			t.Fatalf("addresses = %d, want 2 while retrying", n)
		}
		// The other recipient is still due; the retried one is not due before next.
		if due, _ := s.Claim(ctx, t0.Add(time.Second), 10); len(due) != 1 || due[0].RecipientID == jobs[0].RecipientID {
			t.Fatalf("Claim before retry time = %+v", due)
		}
		if due, _ := s.Claim(ctx, next, 10); len(due) != 1 || due[0].RecipientID != jobs[0].RecipientID || due[0].Attempts != 1 {
			t.Fatalf("Claim at retry time = %+v", due)
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM deliveries WHERE completed_at IS NOT NULL`); n != 0 {
			t.Fatal("delivery completed while recipients are left")
		}
	})

	t.Run("AC-R001-13 recipients interrupted while sending are requeued and nothing final is", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000012", "a@example.invalid", "b@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 10)
		_ = s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)
		// jobs[1] is left "sending", as if the process was killed.

		n, err := s.RequeueInterrupted(ctx)
		if err != nil || n != 1 {
			t.Fatalf("RequeueInterrupted = %d, %v; want 1", n, err)
		}
		due, _ := s.Claim(ctx, t0, 10)
		if len(due) != 1 || due[0].RecipientID != jobs[1].RecipientID {
			t.Fatalf("after restart Claim = %+v, want only the interrupted recipient", due)
		}
	})

	t.Run("recording a recipient that is not being sent fails", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000013", "a@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 10)
		_ = s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)
		if err := s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusFailed, Attempts: 2}, t0); err == nil {
			t.Fatal("a final recipient was changed again")
		}
	})

	t.Run("AC-R001-08 concurrent claims never hand out the same recipient twice", func(t *testing.T) {
		s := openTestDB(t)
		emails := make([]string, 40)
		for i := range emails {
			emails[i] = "a@example.invalid"
		}
		mustCreate(t, s, newDelivery("key-00000014", emails...))

		var mu sync.Mutex
		seen := map[int64]int{}
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					jobs, err := s.Claim(ctx, t0, 3)
					if err != nil {
						t.Error(err)
						return
					}
					if len(jobs) == 0 {
						return
					}
					mu.Lock()
					for _, j := range jobs {
						seen[j.RecipientID]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(seen) != 40 {
			t.Fatalf("claimed %d distinct recipients, want 40", len(seen))
		}
		for id, n := range seen {
			if n != 1 {
				t.Fatalf("recipient %d claimed %d times", id, n)
			}
		}
	})
}

func mustCreate(t *testing.T, s *Store, d usecase.NewDelivery) {
	t.Helper()
	if err := s.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpaqueValuesAreCaseSensitive(t *testing.T) {
	s := openTestDB(t)
	d := newDelivery("key-00000020", "a@example.invalid", "b@example.invalid")
	d.Request.Recipients[0].Ref = "Member_1"
	d.Request.Recipients[1].Ref = "member_1"
	mustCreate(t, s, d)
	mustCreate(t, s, newDelivery("KEY-00000020", "a@example.invalid"))

	if _, err := s.FindByDedupeKey(context.Background(), "key-00000020"); err != nil {
		t.Fatalf("dedupe keys differing only in case must be distinct: %v", err)
	}
}

func TestRecordConcurrently(t *testing.T) {
	t.Run("AC-R001-15 recording the last recipients of a delivery at the same time completes it", func(t *testing.T) {
		s := openTestDB(t)
		ctx := context.Background()
		for round := range 10 {
			d := newDelivery(fmt.Sprintf("key-concurrent-%02d", round), "a@example.invalid", "b@example.invalid", "c@example.invalid")
			mustCreate(t, s, d)
			jobs, err := s.Claim(ctx, t0, 10)
			if err != nil || len(jobs) != 3 {
				t.Fatalf("Claim = %d jobs, %v", len(jobs), err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, len(jobs))
			for _, j := range jobs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- s.Record(ctx, j.RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("round %d: Record failed: %v", round, err)
				}
			}
			if n := count(t, s.DB, `SELECT COUNT(*) FROM deliveries WHERE id = ? AND completed_at IS NOT NULL`, string(d.ID)); n != 1 {
				t.Fatalf("round %d: delivery was not completed", round)
			}
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM delivery_recipients WHERE status = 'sending'`); n != 0 {
			t.Fatalf("%d recipients were left sending", n)
		}
	})
}

func TestRecordsKeepOnlyWhatIsNeeded(t *testing.T) {
	ctx := context.Background()

	t.Run("ADR-0003 a hash of the address remains after the address is deleted", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000030", "coach@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 10)
		_ = s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)

		var ref string
		var email sql.NullString
		if err := s.DB.QueryRowContext(ctx, `SELECT email_ref, email FROM delivery_recipients`).Scan(&ref, &email); err != nil {
			t.Fatal(err)
		}
		if email.Valid || ref != testRefKey.Ref("coach@example.invalid") {
			t.Fatalf("email = %v, email_ref = %q", email, ref)
		}
	})

	t.Run("the body and URL, which may hold a login link, are deleted when the delivery completes", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000031", "a@example.invalid", "b@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 10)

		_ = s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)
		if n := count(t, s.DB, `SELECT COUNT(*) FROM deliveries WHERE body_text IS NOT NULL AND url IS NOT NULL`); n != 1 {
			t.Fatal("the body was deleted while a recipient is still being sent")
		}
		_ = s.Record(ctx, jobs[1].RecipientID, usecase.Outcome{Status: domain.StatusFailed, Code: domain.ErrInvalidAddress, Attempts: 1}, t0)
		if n := count(t, s.DB, `SELECT COUNT(*) FROM deliveries WHERE body_text IS NULL AND url IS NULL AND completed_at IS NOT NULL`); n != 1 {
			t.Fatal("the body and URL remain after completion")
		}
	})

	t.Run("AC-R001-14 released recipients can be claimed again", func(t *testing.T) {
		s := openTestDB(t)
		mustCreate(t, s, newDelivery("key-00000032", "a@example.invalid", "b@example.invalid"))
		jobs, _ := s.Claim(ctx, t0, 10)
		if err := s.Release(ctx, []int64{jobs[1].RecipientID}); err != nil {
			t.Fatal(err)
		}
		again, _ := s.Claim(ctx, t0, 10)
		if len(again) != 1 || again[0].RecipientID != jobs[1].RecipientID {
			t.Fatalf("Claim after Release = %+v", again)
		}
	})
}

func TestDeleteAcceptedBefore(t *testing.T) {
	tests := []struct {
		name        string
		acceptedAgo []time.Duration // how long before the cutoff (negative: after)
		limit       int
		wantDeleted int64
		wantLeft    int
		wantLeftKey string // the dedupe key of the one delivery left, when checked
	}{
		{name: "AC-R001-22 only deliveries accepted before the cutoff are deleted", acceptedAgo: []time.Duration{time.Hour, time.Second, -time.Second}, limit: 1000, wantDeleted: 2, wantLeft: 1},
		{name: "AC-R001-22 at most limit deliveries per call, oldest first", acceptedAgo: []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour}, limit: 2, wantDeleted: 2, wantLeft: 1, wantLeftKey: "purge-2"},
		{name: "AC-R001-22 nothing old", acceptedAgo: []time.Duration{-time.Hour}, limit: 1000, wantDeleted: 0, wantLeft: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestDB(t)
			ctx := context.Background()
			cutoff := t0
			for i, ago := range tt.acceptedAgo {
				d := newDelivery(fmt.Sprintf("purge-%d", i), "a@example.invalid", "b@example.invalid")
				d.AcceptedAt = cutoff.Add(-ago)
				id, _ := domain.NewDeliveryID(d.AcceptedAt, nil)
				d.ID = id
				if err := s.Create(ctx, d); err != nil {
					t.Fatal(err)
				}
			}

			deleted, err := s.DeleteAcceptedBefore(ctx, cutoff, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %d, want %d", deleted, tt.wantDeleted)
			}
			var deliveries, recipients int
			_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries").Scan(&deliveries)
			_ = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM delivery_recipients").Scan(&recipients)
			if deliveries != tt.wantLeft || recipients != 2*tt.wantLeft {
				t.Errorf("left %d deliveries and %d recipients, want %d and %d", deliveries, recipients, tt.wantLeft, 2*tt.wantLeft)
			}

			if tt.wantLeftKey != "" {
				var key string
				_ = s.DB.QueryRowContext(ctx, "SELECT dedupe_key FROM deliveries").Scan(&key)
				if key != tt.wantLeftKey {
					t.Errorf("left %q, want the newest %q", key, tt.wantLeftKey)
				}
			}

			// The dedupe key goes with the delivery: the same key is accepted again as new
			if tt.wantDeleted > 0 {
				if _, err := s.FindByDedupeKey(ctx, "purge-0"); !errors.Is(err, usecase.ErrNotFound) {
					t.Errorf("dedupe key of a deleted delivery still found: %v", err)
				}
			}
		})
	}
}

func TestRecordAfterPurge(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()
	d := newDelivery("purge-while-sending", "a@example.invalid")
	mustCreate(t, s, d)
	jobs, err := s.Claim(ctx, t0, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim = %+v, %v", jobs, err)
	}
	if _, err := s.DeleteAcceptedBefore(ctx, t0.Add(time.Hour), 1000); err != nil {
		t.Fatal(err)
	}

	err = s.Record(ctx, jobs[0].RecipientID, usecase.Outcome{Status: domain.StatusDelivered, Attempts: 1}, t0)
	if !errors.Is(err, usecase.ErrRecipientGone) {
		t.Fatalf("AC-R001-22 Record after the purge = %v, want ErrRecipientGone", err)
	}
}
