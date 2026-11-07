package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeWebhookStore struct {
	mu          sync.Mutex
	due         []WebhookEvent
	sent        []string
	rescheduled map[string]time.Time
	gaveUp      map[string]int
	created     int
}

func (f *fakeWebhookStore) CreateResultEvents(context.Context, time.Time, int, time.Duration) (int, error) {
	f.created++
	return 0, nil
}
func (f *fakeWebhookStore) CreateCompletedEvents(context.Context, time.Time) (int, error) {
	return 0, nil
}
func (f *fakeWebhookStore) ClaimDueEvents(context.Context, time.Time, int, time.Duration) ([]WebhookEvent, error) {
	out := f.due
	f.due = nil
	return out, nil
}
func (f *fakeWebhookStore) MarkEventSent(_ context.Context, id string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, id)
	return nil
}
func (f *fakeWebhookStore) RescheduleEvent(_ context.Context, id string, _ int, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rescheduled == nil {
		f.rescheduled = map[string]time.Time{}
	}
	f.rescheduled[id] = next
	return nil
}
func (f *fakeWebhookStore) GiveUpEvent(_ context.Context, id string, attempts int, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gaveUp == nil {
		f.gaveUp = map[string]int{}
	}
	f.gaveUp[id] = attempts
	return nil
}

type fakeWebhookSender struct{ err error }

func (f *fakeWebhookSender) Send(context.Context, WebhookEvent, time.Time) error { return f.err }

func TestNotifierOutcomes(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		sendErr       error
		createdAgo    time.Duration
		attempts      int
		wantSent      bool
		wantGaveUp    bool
		wantNextAfter time.Duration // 0 when not rescheduled
	}{
		{name: "AC-R001-16 a 2xx answer marks the event sent", wantSent: true},
		{name: "AC-R001-19 a failure is retried with backoff", sendErr: errors.New("503"), attempts: 2, createdAgo: time.Hour, wantNextAfter: 40 * time.Second},
		{name: "AC-R001-19 after 24 hours the event is given up", sendErr: errors.New("timeout"), attempts: 20, createdAgo: 24 * time.Hour, wantGaveUp: true},
		{name: "AC-R001-20 a 401 is not retried", sendErr: ErrWebhookUnauthorized, createdAgo: time.Minute, wantGaveUp: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeWebhookStore{due: []WebhookEvent{{ID: "evt_1", Attempts: tt.attempts, CreatedAt: now.Add(-tt.createdAgo)}}}
			n := &Notifier{Store: store, Sender: &fakeWebhookSender{err: tt.sendErr}, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := n.Batch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if store.created != 1 {
				t.Fatal("results were not batched")
			}
			if err := n.Dispatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(store.sent) == 1; got != tt.wantSent {
				t.Fatalf("sent = %v, want %v", got, tt.wantSent)
			}
			if _, got := store.gaveUp["evt_1"]; got != tt.wantGaveUp {
				t.Fatalf("gave up = %v, want %v", got, tt.wantGaveUp)
			}
			next, rescheduled := store.rescheduled["evt_1"]
			if (tt.wantNextAfter != 0) != rescheduled {
				t.Fatalf("rescheduled = %v, want %v", rescheduled, tt.wantNextAfter != 0)
			}
			if rescheduled && next.Sub(now) != tt.wantNextAfter {
				t.Fatalf("next attempt after %v, want %v", next.Sub(now), tt.wantNextAfter)
			}
		})
	}
}

func TestWebhookRetryDelay(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{name: "AC-R001-19 first retry after 10 seconds", attempts: 1, want: 10 * time.Second},
		{name: "AC-R001-19 doubles", attempts: 4, want: 80 * time.Second},
		{name: "AC-R001-19 at most one hour", attempts: 30, want: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WebhookRetryDelay(tt.attempts); got != tt.want {
				t.Fatalf("WebhookRetryDelay(%d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}
