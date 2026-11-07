package usecase

import (
	"context"
	"log/slog"
	"time"
)

// Retention is how long a delivery is kept after it was accepted (AC-R001-22, ADR-0003).
const Retention = 7 * 24 * time.Hour

// PurgeBatch is how many deliveries one delete statement removes, so the table is not locked for long.
const PurgeBatch = 1000

// PurgeStore deletes old deliveries.
type PurgeStore interface {
	// DeleteAcceptedBefore deletes up to limit deliveries accepted before the given time, with their
	// recipients and dedupe keys, and returns how many deliveries it deleted.
	DeleteAcceptedBefore(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Purger deletes deliveries once they are older than Retention, whatever their status
// (a delivery still unfinished after 7 days is stuck; retries end within about two hours).
type Purger struct {
	Store  PurgeStore
	Now    func() time.Time
	Logger *slog.Logger
}

// PurgeOnce deletes every delivery older than Retention, in batches, and returns the total.
func (p *Purger) PurgeOnce(ctx context.Context) (int64, error) {
	before := p.Now().Add(-Retention)
	var total int64
	for {
		n, err := p.Store.DeleteAcceptedBefore(ctx, before, PurgeBatch)
		total += n
		if err != nil {
			return total, err
		}
		if n < PurgeBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// Run purges now and then every interval until ctx is done. Errors are logged and retried
// at the next interval.
func (p *Purger) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := p.PurgeOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.Logger.Error("purging old deliveries failed", "error", err, "deleted", n)
		} else if n > 0 {
			p.Logger.Info("purged old deliveries", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
