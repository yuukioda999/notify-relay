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

type fakePurgeStore struct {
	mu        sync.Mutex
	remaining int64 // deliveries older than the cutoff
	calls     []time.Time
	failOn    int // 1-based call that fails; 0 never
}

func (f *fakePurgeStore) DeleteAcceptedBefore(_ context.Context, before time.Time, limit int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, before)
	if f.failOn == len(f.calls) {
		return 0, errors.New("database is down")
	}
	n := min(f.remaining, int64(limit))
	f.remaining -= n
	return n, nil
}

func (f *fakePurgeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestPurgeOnce(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		old       int64
		failOn    int
		wantTotal int64
		wantCalls int
		wantErr   bool
	}{
		{name: "AC-R001-22 nothing old: one query", old: 0, wantTotal: 0, wantCalls: 1},
		{name: "AC-R001-22 fewer than a batch", old: 10, wantTotal: 10, wantCalls: 1},
		{name: "AC-R001-22 exactly one batch needs a second query to confirm", old: PurgeBatch, wantTotal: PurgeBatch, wantCalls: 2},
		{name: "AC-R001-22 many batches", old: 2*PurgeBatch + 1, wantTotal: 2*PurgeBatch + 1, wantCalls: 3},
		{name: "AC-R001-22 an error stops and reports what was deleted", old: 3 * PurgeBatch, failOn: 2, wantTotal: PurgeBatch, wantCalls: 2, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakePurgeStore{remaining: tt.old, failOn: tt.failOn}
			p := &Purger{Store: store, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

			total, err := p.PurgeOnce(context.Background())

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if total != tt.wantTotal {
				t.Errorf("total = %d, want %d", total, tt.wantTotal)
			}
			if len(store.calls) != tt.wantCalls {
				t.Errorf("calls = %d, want %d", len(store.calls), tt.wantCalls)
			}
			for _, before := range store.calls {
				if want := now.Add(-7 * 24 * time.Hour); !before.Equal(want) {
					t.Errorf("cutoff = %v, want %v (7 days before now)", before, want)
				}
			}
		})
	}
}

func TestPurgerRunStopsWithContext(t *testing.T) {
	store := &fakePurgeStore{}
	p := &Purger{Store: store, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, time.Hour); close(done) }()

	// Run purges once right away, then waits for the next interval
	deadline := time.After(2 * time.Second)
	for store.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not purge at start")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
}
