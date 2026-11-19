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

// fakeTelemetry remembers what the use cases reported.
type fakeTelemetry struct {
	mu        sync.Mutex
	accepted  []int
	sends     []SendSpan
	results   []sendRecord
	webhooks  []string // traceparent given to StartWebhook
	failures  []WebhookFailure
	nextTrace string // traceparent StartWebhook returns
}

type sendRecord struct {
	result SendResult
	code   domain.ErrorCode
	mailed bool
}

func (f *fakeTelemetry) Accepted(_ context.Context, recipients int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = append(f.accepted, recipients)
}

func (f *fakeTelemetry) StartSend(ctx context.Context, s SendSpan) (context.Context, func(SendResult, domain.ErrorCode, bool, time.Duration)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, s)
	return ctx, func(r SendResult, c domain.ErrorCode, mailed bool, _ time.Duration) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.results = append(f.results, sendRecord{r, c, mailed})
	}
}

func (f *fakeTelemetry) StartWebhook(ctx context.Context, _, traceparent string) (context.Context, string, func(WebhookFailure)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.webhooks = append(f.webhooks, traceparent)
	return ctx, f.nextTrace, func(w WebhookFailure) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.failures = append(f.failures, w)
	}
}

const testTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func TestSendTelemetry(t *testing.T) {
	past := fixedNow.Add(-time.Minute)
	tests := []struct {
		name    string
		job     Job
		mailErr error
		demo    bool
		want    sendRecord
	}{
		{name: "AC-R001-24 a delivered mail is counted as delivered", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			want: sendRecord{SendDelivered, "", true}},
		{name: "AC-R001-24 a temporary failure is counted as a retry with its code", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			mailErr: &domain.SendError{Code: domain.ErrRateLimited, Temporary: true}, want: sendRecord{SendRetry, domain.ErrRateLimited, true}},
		{name: "AC-R001-24 a permanent failure is counted as failed with its code", job: Job{RecipientID: 1, Email: "a@example.invalid"},
			mailErr: &domain.SendError{Code: domain.ErrInvalidAddress}, want: sendRecord{SendFailed, domain.ErrInvalidAddress, true}},
		{name: "AC-R001-24 an expired recipient is counted as skipped and not timed", job: Job{RecipientID: 1, Email: "a@example.invalid", ExpiresAt: &past},
			want: sendRecord{SendSkipped, domain.ErrExpired, false}},
		{name: "AC-R001-24 a real address in demo mode is counted as skipped", demo: true, job: Job{RecipientID: 1, Email: "parent@example.com"},
			want: sendRecord{SendSkipped, "", false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tel := &fakeTelemetry{}
			job := tt.job
			job.DeliveryID = "dlv_1"
			job.Attempts = 2
			job.Traceparent = testTraceparent
			s := &Sender{RefKey: testRefKey, Queue: &fakeQueue{jobs: []Job{job}}, Mailer: &fakeMailer{err: tt.mailErr}, DemoMode: tt.demo,
				Now: func() time.Time { return fixedNow }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: tel}

			if _, err := s.SendDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantSpan := SendSpan{DeliveryID: "dlv_1", RecipientID: 1, Attempt: 3, Traceparent: testTraceparent}
			if len(tel.sends) != 1 || tel.sends[0] != wantSpan {
				t.Fatalf("AC-R001-25 span = %+v, want %+v", tel.sends, wantSpan)
			}
			if len(tel.results) != 1 || tel.results[0] != tt.want {
				t.Fatalf("result = %+v, want %+v", tel.results, tt.want)
			}
		})
	}
}

// capturingSender remembers the events it was asked to send.
type capturingSender struct {
	err  error
	sent []WebhookEvent
}

func (c *capturingSender) Send(_ context.Context, e WebhookEvent, _ time.Time) error {
	c.sent = append(c.sent, e)
	return c.err
}

func TestWebhookTelemetry(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	const spanTrace = "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01"
	tests := []struct {
		name       string
		sendErr    error
		createdAgo time.Duration
		want       WebhookFailure
	}{
		{name: "AC-R001-24 an accepted webhook is not a failure", want: ""},
		{name: "AC-R001-24 a failure that is retried is counted", sendErr: errors.New("503"), createdAgo: time.Minute, want: WebhookRetrying},
		{name: "AC-R001-24 a webhook given up after 24 hours is counted", sendErr: errors.New("timeout"), createdAgo: 24 * time.Hour, want: WebhookGaveUp},
		{name: "AC-R001-24 a rejected signature is counted", sendErr: ErrWebhookUnauthorized, want: WebhookUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tel := &fakeTelemetry{nextTrace: spanTrace}
			sender := &capturingSender{err: tt.sendErr}
			store := &fakeWebhookStore{due: []WebhookEvent{{ID: "evt_1", Traceparent: testTraceparent, CreatedAt: now.Add(-tt.createdAgo)}}}
			n := &Notifier{Store: store, Sender: sender, Now: func() time.Time { return now },
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: tel}

			if err := n.Dispatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(tel.webhooks) != 1 || tel.webhooks[0] != testTraceparent {
				t.Fatalf("AC-R001-25 span parent = %v, want the stored traceparent", tel.webhooks)
			}
			if len(sender.sent) != 1 || sender.sent[0].Traceparent != spanTrace {
				t.Fatalf("AC-R001-25 the webhook did not carry the span's traceparent: %+v", sender.sent)
			}
			if len(tel.failures) != 1 || tel.failures[0] != tt.want {
				t.Fatalf("failure = %v, want %q", tel.failures, tt.want)
			}
		})
	}
}

func TestAcceptTelemetry(t *testing.T) {
	tests := []struct {
		name string
		runs int
		want []int
	}{
		{name: "AC-R001-24 a new delivery is counted with its recipients", runs: 1, want: []int{1}},
		{name: "AC-R001-24 a repeated request is not counted again", runs: 2, want: []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tel := &fakeTelemetry{}
			a := &Acceptor{Store: newMemStore(), Now: func() time.Time { return fixedNow }, Telemetry: tel}
			for range tt.runs {
				if _, err := a.Accept(context.Background(), request(), ""); err != nil {
					t.Fatal(err)
				}
			}
			if len(tel.accepted) != len(tt.want) || tel.accepted[0] != tt.want[0] {
				t.Fatalf("accepted = %v, want %v", tel.accepted, tt.want)
			}
		})
	}
}
