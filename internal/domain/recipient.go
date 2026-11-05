package domain

import "time"

// RecipientStatus is the state of one address within a delivery.
type RecipientStatus string

// Recipient statuses. Pending and sending can still change; the rest are final.
const (
	StatusPending   RecipientStatus = "pending"
	StatusSending   RecipientStatus = "sending" // recorded right before the SMTP call to shrink the double-send window
	StatusDelivered RecipientStatus = "delivered"
	StatusFailed    RecipientStatus = "failed"
	StatusSkipped   RecipientStatus = "skipped"
)

// IsFinal reports whether the status will never change again. The address is deleted
// as soon as a recipient reaches a final status (ADR-0003, AC-R001-15).
func (s RecipientStatus) IsFinal() bool {
	return s == StatusDelivered || s == StatusFailed || s == StatusSkipped
}

// ErrorCode explains a failed or skipped recipient. It never contains provider text,
// which can echo the address.
type ErrorCode string

// Error codes reported for failed and skipped recipients (api/openapi.yaml RecipientResult).
const (
	ErrInvalidAddress      ErrorCode = "invalid_address"
	ErrProviderRejected    ErrorCode = "provider_rejected"
	ErrProviderUnavailable ErrorCode = "provider_unavailable"
	ErrRateLimited         ErrorCode = "rate_limited"
	ErrExpired             ErrorCode = "expired"
)

// SendError is how a mailer reports a failed attempt.
type SendError struct {
	Code      ErrorCode
	Temporary bool // 4xx or no connection: retry later. Otherwise give up now (AC-R001-11).
	Err       error
}

// Error describes the failure. It may contain provider text, so it is never logged.
func (e *SendError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *SendError) Unwrap() error { return e.Err }

// retryDelays is the backoff after each temporary failure (AC-R001-10).
var retryDelays = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	time.Hour,
}

// MaxRetries is how many times a temporary failure is retried before the recipient fails.
var MaxRetries = len(retryDelays)

// RetryDelay returns the wait before the next attempt, given how many attempts have failed
// so far. ok is false when the retries are used up.
func RetryDelay(failedAttempts int) (delay time.Duration, ok bool) {
	if failedAttempts < 1 || failedAttempts > len(retryDelays) {
		return 0, false
	}
	return retryDelays[failedAttempts-1], true
}
