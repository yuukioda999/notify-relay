package mysqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// finish claims every due recipient and records the given status at the given time.
func finish(t *testing.T, s *Store, at time.Time, status domain.RecipientStatus, code domain.ErrorCode) {
	t.Helper()
	ctx := context.Background()
	for {
		jobs, err := s.Claim(ctx, at, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			return
		}
		for _, j := range jobs {
			if err := s.Record(ctx, j.RecipientID, usecase.Outcome{Status: status, Code: code, Attempts: 1}, at); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type eventRow struct {
	Type    string
	Payload map[string]any
}

func events(t *testing.T, s *Store) []eventRow {
	t.Helper()
	rows, err := s.DB.QueryContext(context.Background(), `SELECT type, payload FROM webhook_events ORDER BY created_at, type DESC, LENGTH(payload) DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []eventRow
	for rows.Next() {
		var e eventRow
		var p string
		if err := rows.Scan(&e.Type, &p); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(p), &e.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func emails(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("u%d@example.invalid", i)
	}
	return out
}

func TestCreateResultEvents(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		recipients  int
		finishFirst int // how many recipients reach a final status (the rest stay pending)
		checkAfter  time.Duration
		wantEvents  []int // results per delivery.results event
	}{
		{name: "AC-R001-16 fewer than 500 waiting less than 5 seconds are held", recipients: 3, finishFirst: 2, checkAfter: 4 * time.Second, wantEvents: nil},
		{name: "AC-R001-16 after 5 seconds they are sent", recipients: 3, finishFirst: 2, checkAfter: 5 * time.Second, wantEvents: []int{2}},
		{name: "AC-R001-16 500 waiting are sent at once", recipients: 700, finishFirst: 700, checkAfter: 0, wantEvents: []int{500, 200}},
		{name: "AC-R001-16 a complete delivery is sent without waiting", recipients: 3, finishFirst: 3, checkAfter: 0, wantEvents: []int{3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestDB(t)
			d := newDelivery("results-"+fmt.Sprint(tt.recipients), emails(tt.recipients)...)
			mustCreate(t, s, d)
			finishedAt := t0.Add(time.Minute)
			jobs, err := s.Claim(ctx, finishedAt, tt.finishFirst)
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range jobs {
				code := domain.ErrorCode("")
				status := domain.StatusDelivered
				if j.RecipientID%2 == 0 {
					status, code = domain.StatusFailed, domain.ErrInvalidAddress
				}
				if err := s.Record(ctx, j.RecipientID, usecase.Outcome{Status: status, Code: code, Attempts: 1}, finishedAt); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := s.CreateResultEvents(ctx, finishedAt.Add(tt.checkAfter), usecase.ResultBatchMax, usecase.ResultBatchWait); err != nil {
				t.Fatal(err)
			}
			got := events(t, s)
			var sizes []int
			seen := map[string]bool{}
			for _, e := range got {
				if e.Type != "delivery.results" {
					t.Fatalf("unexpected %s", e.Type)
				}
				results := e.Payload["results"].([]any)
				sizes = append(sizes, len(results))
				for _, r := range results {
					m := r.(map[string]any)
					ref := m["recipient_ref"].(string)
					if seen[ref] {
						t.Fatalf("%s was reported twice", ref)
					}
					seen[ref] = true
					if _, ok := m["email"]; ok {
						t.Fatal("an event carries the address")
					}
					if m["status"] == "failed" && m["error_code"] != "invalid_address" {
						t.Fatalf("error_code = %v", m["error_code"])
					}
				}
				if e.Payload["dedupe_key"] != d.DedupeKey || e.Payload["delivery_id"] != string(d.ID) {
					t.Fatalf("payload = %v", e.Payload)
				}
			}
			if fmt.Sprint(sizes) != fmt.Sprint(tt.wantEvents) {
				t.Fatalf("events of sizes %v, want %v", sizes, tt.wantEvents)
			}

			// Asking again makes nothing new: each result goes into one event only
			if _, err := s.CreateResultEvents(ctx, finishedAt.Add(time.Hour), usecase.ResultBatchMax, usecase.ResultBatchWait); err != nil {
				t.Fatal(err)
			}
			if again := len(events(t, s)); tt.wantEvents != nil && again != len(tt.wantEvents) {
				t.Fatalf("%d events after asking again, want %d", again, len(tt.wantEvents))
			}
		})
	}
}

func TestCreateCompletedEvents(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	d := newDelivery("completed-1", "a@example.invalid", "b@example.invalid")
	d.Traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	mustCreate(t, s, d)
	at := t0.Add(time.Minute)
	finish(t, s, at, domain.StatusDelivered, "")

	// The completed event waits until the results have been put into events
	if n, err := s.CreateCompletedEvents(ctx, at); err != nil || n != 0 {
		t.Fatalf("AC-R001-17 completed before the results: %d, %v", n, err)
	}
	if _, err := s.CreateResultEvents(ctx, at, usecase.ResultBatchMax, usecase.ResultBatchWait); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.CreateCompletedEvents(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	got := events(t, s)
	if len(got) != 2 || got[1].Type != "delivery.completed" {
		t.Fatalf("AC-R001-17 events = %+v, want results then one completed", got)
	}
	counts := got[1].Payload["counts"].(map[string]any)
	if counts["delivered"] != float64(2) || counts["pending"] != float64(0) {
		t.Fatalf("counts = %v", counts)
	}
	var tp string
	if err := s.DB.QueryRowContext(ctx, `SELECT traceparent FROM webhook_events LIMIT 1`).Scan(&tp); err != nil || tp != d.Traceparent {
		t.Fatalf("AC-R001-18 traceparent = %q, %v", tp, err)
	}
}

func TestClaimDueEvents(t *testing.T) {
	ctx := context.Background()
	s := openTestDB(t)
	mustCreate(t, s, newDelivery("claim-events", "a@example.invalid"))
	at := t0.Add(time.Minute)
	finish(t, s, at, domain.StatusDelivered, "")
	if _, err := s.CreateResultEvents(ctx, at, usecase.ResultBatchMax, usecase.ResultBatchWait); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		at   time.Time
		want int
		then func(e usecase.WebhookEvent)
	}{
		{name: "AC-R001-16 a new event is due", at: at, want: 1, then: func(e usecase.WebhookEvent) {
			if err := s.RescheduleEvent(ctx, e.ID, 1, at.Add(10*time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "AC-R001-19 not before the next attempt", at: at.Add(5 * time.Second), want: 0},
		{name: "AC-R001-19 due again at the next attempt, then held for the lease", at: at.Add(10 * time.Second), want: 1},
		{name: "AC-R001-19 a claimed event is not handed out again during the lease", at: at.Add(11 * time.Second), want: 0},
		{name: "AC-R001-19 after the lease it is due again", at: at.Add(71 * time.Second), want: 1, then: func(e usecase.WebhookEvent) {
			if err := s.MarkEventSent(ctx, e.ID, at.Add(71*time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "AC-R001-16 a sent event is never due", at: at.Add(time.Hour), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ClaimDueEvents(ctx, tt.at, 10, time.Minute)
			if err != nil || len(got) != tt.want {
				t.Fatalf("ClaimDueEvents = %d events, %v; want %d", len(got), err, tt.want)
			}
			if tt.then != nil {
				tt.then(got[0])
			}
		})
	}
}

func TestDeliveryStatus(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		finish int // recipients finished out of 2
		want   string
	}{
		{name: "AC-R001-21 accepted, nothing tried", finish: 0, want: usecase.StateAccepted},
		{name: "AC-R001-21 processing", finish: 1, want: usecase.StateProcessing},
		{name: "AC-R001-21 completed", finish: 2, want: usecase.StateCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestDB(t)
			d := newDelivery("status-1", "a@example.invalid", "b@example.invalid")
			mustCreate(t, s, d)
			if tt.finish > 0 {
				jobs, err := s.Claim(ctx, t0, tt.finish)
				if err != nil {
					t.Fatal(err)
				}
				for _, j := range jobs {
					if err := s.Record(ctx, j.RecipientID, usecase.Outcome{Status: domain.StatusFailed, Code: domain.ErrInvalidAddress, Attempts: 1}, t0); err != nil {
						t.Fatal(err)
					}
				}
			}
			st, err := s.DeliveryStatus(ctx, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tt.want || st.Counts.Failed != tt.finish || st.Counts.Pending != 2-tt.finish || st.DedupeKey != d.DedupeKey {
				t.Fatalf("status = %+v", st)
			}
			if (st.CompletedAt != nil) != (tt.want == usecase.StateCompleted) {
				t.Fatalf("completed_at = %v", st.CompletedAt)
			}
		})
	}

	t.Run("AC-R001-21 an unknown or purged delivery is not found", func(t *testing.T) {
		s := openTestDB(t)
		d := newDelivery("status-purged", "a@example.invalid")
		mustCreate(t, s, d)
		finish(t, s, t0, domain.StatusDelivered, "")
		if _, err := s.CreateResultEvents(ctx, t0, usecase.ResultBatchMax, usecase.ResultBatchWait); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteAcceptedBefore(ctx, t0.Add(time.Hour), 1000); err != nil {
			t.Fatalf("AC-R001-22 purge with events: %v", err)
		}
		if _, err := s.DeliveryStatus(ctx, d.ID); !errors.Is(err, usecase.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if n := count(t, s.DB, `SELECT COUNT(*) FROM webhook_events`); n != 0 {
			t.Fatalf("AC-R001-22 %d events left after the purge", n)
		}
	})
}
