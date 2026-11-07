package usecase

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Batching of delivery.results (AC-R001-16): an event is sent when 500 results are waiting
// for a delivery, or when the oldest waiting result is 5 seconds old.
const (
	ResultBatchMax  = 500
	ResultBatchWait = 5 * time.Second
)

// WebhookGiveUpAfter is how long an event is retried before it is given up (AC-R001-19).
const WebhookGiveUpAfter = 24 * time.Hour

// webhookClaimLimit and webhookLease bound one dispatch round: a claimed event is not claimed
// again for webhookLease, so a crash in the middle only delays it.
const (
	webhookClaimLimit  = 50
	webhookLease       = time.Minute
	webhookConcurrency = 8
)

// ErrWebhookUnauthorized is returned by WebhookSender when the business app answered 401.
// The key is probably misconfigured, so the event is not retried (AC-R001-20).
var ErrWebhookUnauthorized = errors.New("webhook receiver rejected the signature")

// WebhookEvent is a saved event waiting to be sent. Payload is the JSON body without sent_at.
type WebhookEvent struct {
	ID          string
	Payload     []byte
	Traceparent string
	Attempts    int
	CreatedAt   time.Time
}

// WebhookStore saves events and tracks their delivery.
type WebhookStore interface {
	// CreateResultEvents puts final recipients that are not yet reported into delivery.results
	// events, at most maxBatch per event. A smaller event is made only when the oldest waiting
	// result is older than now-wait or the delivery is complete. It returns how many events it made.
	CreateResultEvents(ctx context.Context, now time.Time, maxBatch int, wait time.Duration) (int, error)
	// CreateCompletedEvents makes one delivery.completed event for each complete delivery whose
	// results have all been put into events (AC-R001-17).
	CreateCompletedEvents(ctx context.Context, now time.Time) (int, error)
	// ClaimDueEvents returns up to limit events due now and holds them for lease.
	ClaimDueEvents(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]WebhookEvent, error)
	MarkEventSent(ctx context.Context, id string, now time.Time) error
	RescheduleEvent(ctx context.Context, id string, attempts int, next time.Time) error
	GiveUpEvent(ctx context.Context, id string, attempts int, now time.Time) error
}

// WebhookSender posts one event to the business app, adding sent_at and the signature.
type WebhookSender interface {
	Send(ctx context.Context, e WebhookEvent, sentAt time.Time) error
}

// Notifier turns results into webhook events and sends them (AC-R001-16 to 20).
type Notifier struct {
	Store  WebhookStore
	Sender WebhookSender
	Now    func() time.Time
	Logger *slog.Logger
}

// WebhookRetryDelay is the wait after the given number of failed attempts: 10s, 20s, 40s and so
// on, at most one hour.
func WebhookRetryDelay(failedAttempts int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < failedAttempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// Once makes the events that are due and sends the events waiting to be sent.
func (n *Notifier) Once(ctx context.Context) error {
	now := n.now()
	if _, err := n.Store.CreateResultEvents(ctx, now, ResultBatchMax, ResultBatchWait); err != nil {
		return err
	}
	if _, err := n.Store.CreateCompletedEvents(ctx, now); err != nil {
		return err
	}
	events, err := n.Store.ClaimDueEvents(ctx, now, webhookClaimLimit, webhookLease)
	if err != nil {
		return err
	}

	sem := make(chan struct{}, webhookConcurrency)
	var wg sync.WaitGroup
	for _, e := range events {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			n.send(ctx, e)
		}()
	}
	wg.Wait()
	return nil
}

func (n *Notifier) send(ctx context.Context, e WebhookEvent) {
	log := n.logger().With("event_id", e.ID)
	attempts := e.Attempts + 1
	err := n.Sender.Send(ctx, e, n.now())
	now := n.now()
	var storeErr error
	switch {
	case err == nil:
		storeErr = n.Store.MarkEventSent(ctx, e.ID, now)
	case errors.Is(err, ErrWebhookUnauthorized):
		log.Error("webhook receiver rejected the signature; not retrying (check WEBHOOK_KEY_ID and the receiver's key)", "attempts", attempts)
		storeErr = n.Store.GiveUpEvent(ctx, e.ID, attempts, now)
	case now.Sub(e.CreatedAt) >= WebhookGiveUpAfter:
		log.Error("webhook was not accepted for 24 hours; giving up", "attempts", attempts, "error", err)
		storeErr = n.Store.GiveUpEvent(ctx, e.ID, attempts, now)
	default:
		log.Warn("webhook was not accepted; will retry", "attempts", attempts, "error", err)
		storeErr = n.Store.RescheduleEvent(ctx, e.ID, attempts, now.Add(WebhookRetryDelay(attempts)))
	}
	if storeErr != nil {
		// The lease runs out and the event is sent again; the receiver drops duplicates by event_id.
		log.Error("record webhook result", "error", storeErr)
	}
}

// Run calls Once every interval until ctx is done. Errors are logged and retried.
func (n *Notifier) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := n.Once(ctx); err != nil && ctx.Err() == nil {
			n.logger().Error("webhook round failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *Notifier) logger() *slog.Logger {
	if n.Logger != nil {
		return n.Logger
	}
	return slog.Default()
}
