package usecase

import (
	"context"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// Delivery states returned by the status query (api/openapi.yaml DeliveryStatus).
const (
	StateAccepted   = "accepted"   // nothing has been tried yet
	StateProcessing = "processing" // some recipients have been tried
	StateCompleted  = "completed"  // every recipient is final
)

// ResultCounts counts recipients by status. Sending recipients are counted as pending.
type ResultCounts struct {
	Pending   int `json:"pending"`
	Delivered int `json:"delivered"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// DeliveryStatus is the answer to the status query (AC-R001-21). It has counts only, no
// per-recipient detail: it is for reconciling missed webhooks.
type DeliveryStatus struct {
	ID          domain.DeliveryID
	DedupeKey   string
	State       string
	Counts      ResultCounts
	AcceptedAt  time.Time
	CompletedAt *time.Time
}

// StatusReader reads the status of one delivery. It returns ErrNotFound for an unknown
// delivery or one deleted after 7 days.
type StatusReader interface {
	DeliveryStatus(ctx context.Context, id domain.DeliveryID) (DeliveryStatus, error)
}
