package usecase

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// Job is one recipient that is due to be sent. It has been marked "sending" when claimed.
type Job struct {
	RecipientID int64
	DeliveryID  domain.DeliveryID
	Email       string
	Message     domain.Message
	ExpiresAt   *time.Time
	Attempts    int // attempts made before this one
}

// Outcome is the result of one attempt, recorded by the store.
type Outcome struct {
	Status      domain.RecipientStatus // pending (retry later) or a final status
	Code        domain.ErrorCode
	Attempts    int
	NextAttempt time.Time // set when Status is pending
}

// SendQueue hands out due recipients and records results.
type SendQueue interface {
	// Claim marks up to limit due recipients as "sending" and returns them. A recipient
	// is handed to one sender only.
	Claim(ctx context.Context, now time.Time, limit int) ([]Job, error)
	// Record stores the outcome. Reaching a final status deletes the address (AC-R001-15).
	Record(ctx context.Context, recipientID int64, o Outcome, now time.Time) error
	// RequeueInterrupted returns recipients left "sending" by a crash to "pending" (AC-R001-13).
	RequeueInterrupted(ctx context.Context) (int64, error)
}

// Mail is what the mailer sends to one address.
type Mail struct {
	To      string
	Message domain.Message
}

// Mailer sends one mail. A failure should be a *domain.SendError.
type Mailer interface {
	Send(ctx context.Context, m Mail) error
}

// Sender sends due recipients with a fixed number of workers (AC-R001-08).
type Sender struct {
	Queue       SendQueue
	Mailer      Mailer
	Concurrency int
	BatchSize   int
	Logger      *slog.Logger
	Now         func() time.Time
}

// SendDue claims one batch and sends it. It returns how many recipients were processed.
func (s *Sender) SendDue(ctx context.Context) (int, error) {
	jobs, err := s.Queue.Claim(ctx, s.now(), s.batchSize())
	if err != nil || len(jobs) == 0 {
		return 0, err
	}

	work := make(chan Job)
	var wg sync.WaitGroup
	for range min(s.concurrency(), len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				s.process(ctx, j)
			}
		}()
	}
	for _, j := range jobs {
		work <- j
	}
	close(work)
	wg.Wait()
	return len(jobs), nil
}

// Run sends due recipients until ctx is canceled. Work already claimed is finished first,
// using a context that is not canceled, so a shutdown does not leave mail half-sent (AC-R001-14).
func (s *Sender) Run(ctx context.Context, idle time.Duration) {
	if n, err := s.Queue.RequeueInterrupted(context.WithoutCancel(ctx)); err != nil {
		s.logger().Error("requeue interrupted recipients", "error", err)
	} else if n > 0 {
		s.logger().Warn("requeued recipients interrupted by a previous stop", "count", n)
	}

	for {
		n, err := s.SendDue(context.WithoutCancel(ctx))
		if err != nil {
			s.logger().Error("claim due recipients", "error", err)
		}
		if n > 0 && err == nil && ctx.Err() == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idle):
		}
	}
}

func (s *Sender) process(ctx context.Context, j Job) {
	now := s.now()
	attempts := j.Attempts + 1
	log := s.logger().With("delivery_id", j.DeliveryID, "recipient_id", j.RecipientID, "email_ref", domain.EmailRef(j.Email))

	var o Outcome
	if j.ExpiresAt != nil && !now.Before(*j.ExpiresAt) {
		o = Outcome{Status: domain.StatusSkipped, Code: domain.ErrExpired, Attempts: j.Attempts} // AC-R001-12
	} else {
		o = outcomeOf(s.Mailer.Send(ctx, Mail{To: j.Email, Message: j.Message}), attempts, now)
	}

	if err := s.Queue.Record(ctx, j.RecipientID, o, s.now()); err != nil {
		log.Error("record send result", "error", err)
		return
	}
	log.Info("send attempt finished", "status", o.Status, "error_code", o.Code, "attempts", o.Attempts)
}

// outcomeOf turns a mailer result into the next state (AC-R001-10, AC-R001-11).
func outcomeOf(err error, attempts int, now time.Time) Outcome {
	if err == nil {
		return Outcome{Status: domain.StatusDelivered, Attempts: attempts}
	}
	var se *domain.SendError
	if !errors.As(err, &se) {
		se = &domain.SendError{Code: domain.ErrProviderUnavailable, Temporary: true, Err: err}
	}
	if !se.Temporary {
		return Outcome{Status: domain.StatusFailed, Code: se.Code, Attempts: attempts}
	}
	if delay, ok := domain.RetryDelay(attempts); ok {
		return Outcome{Status: domain.StatusPending, Code: se.Code, Attempts: attempts, NextAttempt: now.Add(delay)}
	}
	return Outcome{Status: domain.StatusFailed, Code: se.Code, Attempts: attempts}
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Sender) concurrency() int {
	if s.Concurrency > 0 {
		return s.Concurrency
	}
	return 50
}

func (s *Sender) batchSize() int {
	if s.BatchSize > 0 {
		return s.BatchSize
	}
	return 500
}

func (s *Sender) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
