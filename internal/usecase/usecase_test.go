package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// memStore is an in-memory DeliveryStore.
type memStore struct {
	mu        sync.Mutex
	byKey     map[string]Accepted
	createErr error
	// raceWinner, if set, is stored just before Create runs, to simulate a concurrent request.
	raceWinner *Accepted
	created    []NewDelivery
}

func newMemStore() *memStore { return &memStore{byKey: map[string]Accepted{}} }

func (m *memStore) Create(_ context.Context, d NewDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raceWinner != nil {
		m.byKey[m.raceWinner.DedupeKey] = *m.raceWinner
		m.raceWinner = nil
	}
	if m.createErr != nil {
		return m.createErr
	}
	if _, ok := m.byKey[d.DedupeKey]; ok {
		return ErrDuplicateDedupeKey
	}
	m.byKey[d.DedupeKey] = d.Accepted
	m.created = append(m.created, d)
	return nil
}

func (m *memStore) FindByDedupeKey(_ context.Context, key string) (Accepted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byKey[key]
	if !ok {
		return Accepted{}, ErrNotFound
	}
	return a, nil
}

func request() domain.Request {
	return domain.Request{
		DedupeKey:  "login-link:1",
		TenantRef:  domain.SystemTenantRef,
		Message:    domain.Message{Subject: "件名", Text: "本文"},
		Recipients: []domain.Recipient{{Ref: "user_1", Email: "coach@example.invalid"}},
	}
}

var fixedNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func TestAccept(t *testing.T) {
	changed := request()
	changed.Message.Text = "別の本文"

	tests := []struct {
		name        string
		setup       func(*memStore, *Acceptor)
		req         domain.Request
		wantCreated bool
		wantErr     error
		wantInvalid bool
		wantStored  int
	}{
		{name: "AC-R001-01 a new request is stored and reported as created", req: request(), wantCreated: true, wantStored: 1},
		{
			name: "AC-R001-03 the same key and content returns the first delivery",
			setup: func(m *memStore, a *Acceptor) {
				_, _ = a.Accept(context.Background(), request(), "")
			},
			req: request(), wantCreated: false, wantStored: 1,
		},
		{
			name: "AC-R001-04 the same key with different content is a conflict",
			setup: func(m *memStore, a *Acceptor) {
				_, _ = a.Accept(context.Background(), request(), "")
			},
			req: changed, wantErr: ErrDedupeConflict, wantStored: 1,
		},
		{
			name: "AC-R001-03 a concurrent request with the same content is treated as a repeat",
			setup: func(m *memStore, _ *Acceptor) {
				m.raceWinner = &Accepted{ID: "dlv_WINNER", DedupeKey: "login-link:1", ContentHash: request().ContentHash(), RecipientCount: 1}
			},
			req: request(), wantCreated: false, wantStored: 0,
		},
		{
			name: "AC-R001-04 a concurrent request with other content is a conflict",
			setup: func(m *memStore, _ *Acceptor) {
				m.raceWinner = &Accepted{ID: "dlv_WINNER", DedupeKey: "login-link:1", ContentHash: "other"}
			},
			req: request(), wantErr: ErrDedupeConflict, wantStored: 0,
		},
		{
			name:  "AC-R001-01 a storage failure is not reported as accepted",
			setup: func(m *memStore, _ *Acceptor) { m.createErr = errors.New("db down") },
			req:   request(), wantErr: errors.New("db down"), wantStored: 0,
		},
		{
			name:  "AC-R001-06 demo mode rejects real domains before storing anything",
			setup: func(_ *memStore, a *Acceptor) { a.DemoMode = true },
			req: func() domain.Request {
				r := request()
				r.Recipients[0].Email = "parent@example.com"
				return r
			}(),
			wantInvalid: true, wantStored: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemStore()
			acc := &Acceptor{Store: store, Now: func() time.Time { return fixedNow }}
			if tt.setup != nil {
				tt.setup(store, acc)
			}

			res, err := acc.Accept(context.Background(), tt.req, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

			switch {
			case tt.wantInvalid:
				var ve *domain.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want validation error", err)
				}
			case tt.wantErr != nil:
				if err == nil || (!errors.Is(err, tt.wantErr) && err.Error() != tt.wantErr.Error()) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if res.Created != tt.wantCreated {
					t.Fatalf("Created = %v, want %v", res.Created, tt.wantCreated)
				}
				if res.ID == "" || res.RecipientCount != 1 {
					t.Fatalf("unexpected result %+v", res)
				}
			}
			if len(store.created) != tt.wantStored {
				t.Fatalf("store holds %d created deliveries, want %d", len(store.created), tt.wantStored)
			}
		})
	}
}

// fakeQueue is an in-memory SendQueue.
type fakeQueue struct {
	mu          sync.Mutex
	jobs        []Job
	recorded    map[int64]Outcome
	released    []int64
	requeued    int
	recordFails int // the first recordFails calls to Record fail
}

func (q *fakeQueue) Claim(_ context.Context, _ time.Time, limit int) ([]Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(limit, len(q.jobs))
	out := q.jobs[:n]
	q.jobs = q.jobs[n:]
	return out, nil
}

func (q *fakeQueue) Record(_ context.Context, id int64, o Outcome, _ time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.recordFails > 0 {
		q.recordFails--
		return errors.New("database unavailable")
	}
	if q.recorded == nil {
		q.recorded = map[int64]Outcome{}
	}
	q.recorded[id] = o
	return nil
}

func (q *fakeQueue) Release(_ context.Context, ids []int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.released = append(q.released, ids...)
	return nil
}

func (q *fakeQueue) RequeueInterrupted(context.Context) (int64, error) {
	q.requeued++
	return 0, nil
}

type fakeMailer struct {
	mu      sync.Mutex
	err     error
	sent    []Mail
	active  int
	peak    int
	delay   time.Duration
	onStart func() // called when a send starts
}

func (m *fakeMailer) Send(_ context.Context, mail Mail) error {
	if m.onStart != nil {
		m.onStart()
	}
	m.mu.Lock()
	m.active++
	m.peak = max(m.peak, m.active)
	m.mu.Unlock()
	time.Sleep(m.delay)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active--
	m.sent = append(m.sent, mail)
	return m.err
}

func TestSendDueOutcomes(t *testing.T) {
	past := fixedNow.Add(-time.Minute)
	future := fixedNow.Add(time.Hour)

	tests := []struct {
		name     string
		job      Job
		mailErr  error
		demo     bool
		want     Outcome
		wantSent bool
	}{
		{
			name: "AC-R001-08 a successful send is delivered", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			want: Outcome{Status: domain.StatusDelivered, Attempts: 1}, wantSent: true,
		},
		{
			name: "AC-R001-10 a temporary failure is retried after 30 seconds", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			mailErr: &domain.SendError{Code: domain.ErrProviderUnavailable, Temporary: true},
			want:    Outcome{Status: domain.StatusPending, Code: domain.ErrProviderUnavailable, Attempts: 1, NextAttempt: fixedNow.Add(30 * time.Second)}, wantSent: true,
		},
		{
			name: "AC-R001-10 an unclassified error is treated as temporary", job: Job{RecipientID: 1, Email: "a@example.invalid", Attempts: 1},
			mailErr: errors.New("connection reset"),
			want:    Outcome{Status: domain.StatusPending, Code: domain.ErrProviderUnavailable, Attempts: 2, NextAttempt: fixedNow.Add(2 * time.Minute)}, wantSent: true,
		},
		{
			name: "AC-R001-10 the sixth failed attempt gives up", job: Job{RecipientID: 1, Email: "a@example.invalid", Attempts: 5},
			mailErr: &domain.SendError{Code: domain.ErrRateLimited, Temporary: true},
			want:    Outcome{Status: domain.StatusFailed, Code: domain.ErrRateLimited, Attempts: 6}, wantSent: true,
		},
		{
			name: "AC-R001-11 a permanent failure is not retried", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			mailErr: &domain.SendError{Code: domain.ErrInvalidAddress},
			want:    Outcome{Status: domain.StatusFailed, Code: domain.ErrInvalidAddress, Attempts: 1}, wantSent: true,
		},
		{
			name: "AC-R001-12 an expired recipient is skipped without sending", job: Job{RecipientID: 1, Email: "a@example.invalid", ExpiresAt: &past},
			want: Outcome{Status: domain.StatusSkipped, Code: domain.ErrExpired, Attempts: 0}, wantSent: false,
		},
		{
			name: "AC-R001-12 a recipient before its expiry is sent", job: Job{RecipientID: 1, Email: "a@example.invalid", ExpiresAt: &future},
			want: Outcome{Status: domain.StatusDelivered, Attempts: 1}, wantSent: true,
		},
		{
			name: "AC-R001-06 demo mode skips a real address right before sending", demo: true, job: Job{RecipientID: 1, Email: "parent@example.com"},
			want: Outcome{Status: domain.StatusSkipped, Attempts: 0}, wantSent: false,
		},
		{
			name: "AC-R001-06 demo mode sends to .invalid", demo: true, job: Job{RecipientID: 1, Email: "a@example.invalid"},
			want: Outcome{Status: domain.StatusDelivered, Attempts: 1}, wantSent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{jobs: []Job{tt.job}}
			m := &fakeMailer{err: tt.mailErr}
			s := &Sender{Queue: q, Mailer: m, DemoMode: tt.demo, Now: func() time.Time { return fixedNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

			n, err := s.SendDue(context.Background())
			if err != nil || n != 1 {
				t.Fatalf("SendDue() = %d, %v", n, err)
			}
			if got := q.recorded[1]; got != tt.want {
				t.Fatalf("outcome = %+v, want %+v", got, tt.want)
			}
			if sent := len(m.sent) == 1; sent != tt.wantSent {
				t.Fatalf("sent = %v, want %v", sent, tt.wantSent)
			}
		})
	}
}

func TestSendDueRespectsConcurrency(t *testing.T) {
	tests := []struct {
		name        string
		jobs        int
		concurrency int
	}{
		{name: "AC-R001-08 at most 3 sends at once", jobs: 20, concurrency: 3},
		{name: "AC-R001-08 fewer jobs than workers", jobs: 2, concurrency: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{}
			for i := range tt.jobs {
				q.jobs = append(q.jobs, Job{RecipientID: int64(i + 1), Email: "a@example.invalid"})
			}
			m := &fakeMailer{delay: 5 * time.Millisecond}
			s := &Sender{Queue: q, Mailer: m, Concurrency: tt.concurrency, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

			if _, err := s.SendDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(m.sent) != tt.jobs {
				t.Fatalf("sent %d, want %d", len(m.sent), tt.jobs)
			}
			if m.peak > tt.concurrency {
				t.Fatalf("peak concurrency %d exceeds %d", m.peak, tt.concurrency)
			}
		})
	}
}

func TestRun(t *testing.T) {
	t.Run("AC-R001-13 interrupted work is requeued at start and Run stops on cancel", testRunRequeuesAndStops)
}

func testRunRequeuesAndStops(t *testing.T) {
	q := &fakeQueue{jobs: []Job{{RecipientID: 1, Email: "a@example.invalid"}}}
	m := &fakeMailer{}
	s := &Sender{Queue: q, Mailer: m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, 10*time.Millisecond); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		q.mu.Lock()
		got := len(q.recorded)
		q.mu.Unlock()
		if got == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("job was not processed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	if q.requeued != 1 {
		t.Fatalf("RequeueInterrupted called %d times, want 1 (AC-R001-13)", q.requeued)
	}
}

func TestShutdown(t *testing.T) {
	t.Run("AC-R001-14 after the stop signal no new mail starts; started mail is recorded and the rest released", func(t *testing.T) {
		q := &fakeQueue{}
		for i := range 10 {
			q.jobs = append(q.jobs, Job{RecipientID: int64(i + 1), Email: "a@example.invalid"})
		}
		ctx, cancel := context.WithCancel(context.Background())
		var once sync.Once
		m := &fakeMailer{delay: 20 * time.Millisecond, onStart: func() { once.Do(cancel) }}
		s := &Sender{Queue: q, Mailer: m, Concurrency: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

		started, err := s.SendDue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if started < 1 || started > 2 {
			t.Fatalf("started %d sends after the stop signal, want 1 (or 2 if the next one was already handed over)", started)
		}
		if len(q.recorded) != started || len(m.sent) != started {
			t.Fatalf("recorded %d, sent %d, want %d each", len(q.recorded), len(m.sent), started)
		}
		if len(q.released) != 10-started {
			t.Fatalf("released %d, want %d", len(q.released), 10-started)
		}
	})

	t.Run("AC-R001-14 nothing is claimed once stopped", func(t *testing.T) {
		q := &fakeQueue{jobs: []Job{{RecipientID: 1, Email: "a@example.invalid"}}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &Sender{Queue: q, Mailer: &fakeMailer{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if n, _ := s.SendDue(ctx); n != 0 || len(q.jobs) != 1 {
			t.Fatalf("SendDue after stop claimed or sent %d", n)
		}
	})
}

func TestRecordIsRetried(t *testing.T) {
	saved := recordRetryDelays
	recordRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { recordRetryDelays = saved })

	tests := []struct {
		name         string
		fails        int
		wantRecorded bool
	}{
		{name: "AC-R001-13 a temporary database error does not leave the recipient sending", fails: 2, wantRecorded: true},
		{name: "AC-R001-13 a lasting database error is given up after 4 tries", fails: 10, wantRecorded: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &fakeQueue{jobs: []Job{{RecipientID: 1, Email: "a@example.invalid"}}, recordFails: tt.fails}
			s := &Sender{Queue: q, Mailer: &fakeMailer{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if _, err := s.SendDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := q.recorded[1]; ok != tt.wantRecorded {
				t.Fatalf("recorded = %v, want %v", ok, tt.wantRecorded)
			}
		})
	}
}
