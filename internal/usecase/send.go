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

// ErrRecipientGone is returned by Record when the recipient no longer exists, for example
// because the delivery was purged after 7 days while it was being sent (AC-R001-22).
// Trying again cannot help.
var ErrRecipientGone = errors.New("recipient no longer exists")

// SendQueue hands out due recipients and records results.
type SendQueue interface {
	// Claim marks up to limit due recipients as "sending" and returns them. A recipient
	// is handed to one sender only.
	Claim(ctx context.Context, now time.Time, limit int) ([]Job, error)
	// Record stores the outcome. Reaching a final status deletes the address (AC-R001-15).
	// It returns ErrRecipientGone when the recipient was deleted.
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
	// RefKey makes the email_ref written to the logs instead of the address (ADR-0003).
	RefKey domain.EmailRefKey
	Logger *slog.Logger
	Now    func() time.Time
}

// SendDue claims one batch, sends it and waits for all of it. Run does not use it (Run keeps the
// workers busy without waiting for a whole batch); it is kept for sending once, as in the tests.
// When ctx is canceled no new mail is started: mail already being sent is finished and recorded,
// and the rest of the batch is released (AC-R001-14).
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
//
// The workers are kept busy without waiting for a whole batch: whenever there is room in the
// hand-over queue (as many places as workers) is half free, more due recipients are claimed
// (ADR-0003). A slow
// mail therefore holds up only its own worker, not the next claim. On cancel no new mail starts:
// mail being sent is finished and recorded, and claimed recipients not yet started are released
// (AC-R001-14).
func (s *Sender) Run(ctx context.Context, idle time.Duration) {
	if n, err := s.Queue.RequeueInterrupted(ctx); err != nil {
		s.logger().Error("requeue interrupted recipients", "error", err)
	} else if n > 0 {
		s.logger().Warn("requeued recipients interrupted by a previous stop", "count", n)
	}

	workCtx := context.WithoutCancel(ctx) // sending and recording must not be cut off halfway
	queue := make(chan Job, s.concurrency())
	taken := make(chan struct{}, 1) // a worker took a job: there may be room to claim more

	var mu sync.Mutex
	var unsent []int64 // claimed but not started because of the stop signal
	var wg sync.WaitGroup
	for range s.concurrency() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				select {
				case taken <- struct{}{}:
				default:
				}
				if ctx.Err() != nil {
					mu.Lock()
					unsent = append(unsent, j.RecipientID)
					mu.Unlock()
					continue
				}
				s.process(workCtx, j)
			}
		}()
	}

	// Claim once at least half of the queue is free, so that one claim (one database round trip)
	// covers many mails while the other half keeps the workers busy
	threshold := max(1, cap(queue)/2)
	for ctx.Err() == nil {
		room := cap(queue) - len(queue)
		if room < threshold {
			select {
			case <-ctx.Done():
			case <-taken:
			}
			continue
		}
		jobs, err := s.Queue.Claim(ctx, s.now(), min(room, s.batchSize()))
		if err != nil && ctx.Err() == nil {
			s.logger().Error("claim due recipients", "error", err)
		}
		for _, j := range jobs {
			queue <- j // never blocks: only this loop adds, and there was room for all of them
		}
		if len(jobs) == 0 || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(idle):
			}
		}
	}
	close(queue)
	wg.Wait()

	if len(unsent) > 0 {
		if err := s.Queue.Release(workCtx, unsent); err != nil {
			// They stay "sending" and are requeued on the next start; nothing was sent to them.
			s.logger().Error("release unsent recipients", "error", err, "count", len(unsent))
		}
	}
}

func (s *Sender) process(ctx context.Context, j Job) {
	now := s.now()
	attempts := j.Attempts + 1
	log := s.logger().With("delivery_id", j.DeliveryID, "recipient_id", j.RecipientID, "email_ref", s.RefKey.Ref(j.Email))

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
		err = s.Queue.Record(ctx, j.RecipientID, o, s.now())
		if err == nil {
			log.Info("send attempt finished", "status", o.Status, "error_code", o.Code, "attempts", o.Attempts)
			return
		}
		if errors.Is(err, ErrRecipientGone) {
			log.Warn("recipient was deleted before the result was recorded", "status", o.Status)
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
