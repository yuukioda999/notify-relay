package usecase

import (
	"context"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// SendResult is the metric label for one send attempt. It has a small fixed set of values
// (team-ops AC-010-06): no address, delivery or tenant goes into a label.
type SendResult string

// The results of one send attempt.
const (
	SendDelivered SendResult = "delivered"
	SendFailed    SendResult = "failed"
	SendRetry     SendResult = "retry"
	SendSkipped   SendResult = "skipped"
)

// WebhookFailure is the metric label for a webhook that was not accepted.
type WebhookFailure string

// The kinds of webhook failure.
const (
	WebhookRetrying     WebhookFailure = "retry"
	WebhookGaveUp       WebhookFailure = "gave_up"
	WebhookUnauthorized WebhookFailure = "unauthorized"
)

// SendSpan describes one send attempt for tracing. It holds no address and no message text.
type SendSpan struct {
	DeliveryID  domain.DeliveryID
	RecipientID int64
	Attempt     int
	// Traceparent is the one stored when the delivery was accepted, so every attempt,
	// retries included, joins the trace of the request (AC-R001-25).
	Traceparent string
}

// Telemetry records traces and metrics (AC-R001-24, AC-R001-25). Implementations must not
// record addresses, names, message text or links (team-ops ADR-0003, AC-010-01).
type Telemetry interface {
	// Accepted counts a newly accepted delivery and its recipients.
	Accepted(ctx context.Context, recipients int)
	// StartSend starts the span of one send attempt. The returned function ends it, records the
	// result and, when mailed is true, how long the mail server took.
	StartSend(ctx context.Context, s SendSpan) (context.Context, func(result SendResult, code domain.ErrorCode, mailed bool, took time.Duration))
	// StartWebhook starts the span of one webhook attempt. It returns the traceparent to send
	// with the webhook, so the business app continues the same trace. The returned function
	// ends the span; failure is empty when the webhook was accepted.
	StartWebhook(ctx context.Context, eventID, traceparent string) (context.Context, string, func(failure WebhookFailure))
}

// NopTelemetry records nothing. It is used when no Telemetry is set.
type NopTelemetry struct{}

// Accepted does nothing.
func (NopTelemetry) Accepted(context.Context, int) {}

// StartSend does nothing.
func (NopTelemetry) StartSend(ctx context.Context, _ SendSpan) (context.Context, func(SendResult, domain.ErrorCode, bool, time.Duration)) {
	return ctx, func(SendResult, domain.ErrorCode, bool, time.Duration) {}
}

// StartWebhook returns the stored traceparent unchanged.
func (NopTelemetry) StartWebhook(ctx context.Context, _, traceparent string) (context.Context, string, func(WebhookFailure)) {
	return ctx, traceparent, func(WebhookFailure) {}
}

// resultOf maps the next state of a recipient to its metric label.
func resultOf(s domain.RecipientStatus) SendResult {
	switch s {
	case domain.StatusDelivered:
		return SendDelivered
	case domain.StatusFailed:
		return SendFailed
	case domain.StatusSkipped:
		return SendSkipped
	default:
		return SendRetry
	}
}

func telemetryOr(t Telemetry) Telemetry {
	if t != nil {
		return t
	}
	return NopTelemetry{}
}
