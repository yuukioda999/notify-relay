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
	// Release returns claimed recipients that were not sent to "pending".
	Release(ctx context.Context, recipientIDs []int64) error
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

// recordRetryDelays bound how long a finished send waits for the database before giving up.
var recordRetryDelays = []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second}

// Sender sends due recipients with a fixed number of workers (AC-R001-08).
type Sender struct {
	Queue       SendQueue
	Mailer      Mailer
	Concurrency int
	BatchSize   int
	// DemoMode skips any address outside .invalid right before sending, in addition to the
	// check at acceptance, so a demo environment can never mail a real person (AC-R001-06).
	DemoMode bool
	Logger   *slog.Logger
	Now      func() time.Time
}

// SendDue claims one batch and sends it. When ctx is canceled no new mail is started: mail
// already being sent is finished and recorded, and the rest of the batch is released (AC-R001-14).
// It returns how many recipients were sent or skipped.
func (s *Sender) SendDue(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, nil
	}
	jobs, err := s.Queue.Claim(ctx, s.now(), s.batchSize())
	if err != nil || len(jobs) == 0 {
		return 0, err
	}

	// Sending and recording must not be cut off halfway by the stop signal.
	workCtx := context.WithoutCancel(ctx)
	work := make(chan Job)
	var wg sync.WaitGroup
	for range min(s.concurrency(), len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				s.process(workCtx, j)
			}
		}()
	}

	started := 0
dispatch:
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			break dispatch
		case work <- j:
			started++
		}
	}
	close(work)
	wg.Wait()

	if rest := jobs[started:]; len(rest) > 0 {
		ids := make([]int64, len(rest))
		for i, j := range rest {
			ids[i] = j.RecipientID
		}
		if err := s.Queue.Release(workCtx, ids); err != nil {
			// They stay "sending" and are requeued on the next start; nothing was sent to them.
			s.logger().Error("release unsent recipients", "error", err, "count", len(ids))
		}
	}
	return started, nil
}

// Run sends due recipients until ctx is canceled, then returns once the mail being sent is done.
func (s *Sender) Run(ctx context.Context, idle time.Duration) {
	if n, err := s.Queue.RequeueInterrupted(ctx); err != nil {
		s.logger().Error("requeue interrupted recipients", "error", err)
	} else if n > 0 {
		s.logger().Warn("requeued recipients interrupted by a previous stop", "count", n)
	}

	for {
		n, err := s.SendDue(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger().Error("claim due recipients", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		if n > 0 && err == nil {
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
	switch {
	case j.ExpiresAt != nil && !now.Before(*j.ExpiresAt):
		o = Outcome{Status: domain.StatusSkipped, Code: domain.ErrExpired, Attempts: j.Attempts} // AC-R001-12
	case s.DemoMode && !domain.IsDemoAddress(j.Email):
		o = Outcome{Status: domain.StatusSkipped, Attempts: j.Attempts}
		log.Warn("skipped a non-demo address in demo mode")
	default:
		o = outcomeOf(s.Mailer.Send(ctx, Mail{To: j.Email, Message: j.Message}), attempts, now)
	}

	// The mail may already be out, so the result is worth a few tries: a recipient left
	// "sending" would be sent again after a restart.
	var err error
	for _, delay := range append([]time.Duration{0}, recordRetryDelays...) {
		time.Sleep(delay)
		if err = s.Queue.Record(ctx, j.RecipientID, o, s.now()); err == nil {
			log.Info("send attempt finished", "status", o.Status, "error_code", o.Code, "attempts", o.Attempts)
			return
		}
	}
	log.Error("record send result", "error", err, "status", o.Status)
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
