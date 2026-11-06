// Package usecase implements notify-relay's operations. It defines the interfaces it needs
// and depends only on the domain package.
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// ErrDedupeConflict means the dedupe key was already used for different content (409, AC-R001-04).
var ErrDedupeConflict = errors.New("dedupe key already used with different content")

// ErrDuplicateDedupeKey is returned by DeliveryStore.Create when the key already exists.
var ErrDuplicateDedupeKey = errors.New("dedupe key already exists")

// ErrNotFound is returned when a delivery does not exist.
var ErrNotFound = errors.New("not found")

// Accepted describes a stored delivery request.
type Accepted struct {
	ID             domain.DeliveryID
	DedupeKey      string
	ContentHash    string
	RecipientCount int
	AcceptedAt     time.Time
}

// NewDelivery is what the store saves in one transaction: the request and all its recipients.
type NewDelivery struct {
	Accepted
	Request     domain.Request
	Traceparent string
}

// DeliveryStore persists delivery requests.
type DeliveryStore interface {
	// Create saves the delivery and all recipients atomically. It returns ErrDuplicateDedupeKey
	// if the dedupe key is taken.
	Create(ctx context.Context, d NewDelivery) error
	// FindByDedupeKey returns the stored delivery for the key, or ErrNotFound.
	FindByDedupeKey(ctx context.Context, key string) (Accepted, error)
}

// AcceptResult tells the caller whether the request is new (202) or a repeat (200).
type AcceptResult struct {
	Accepted
	Created bool
}

// Acceptor handles incoming delivery requests (AC-R001-01, 03, 04, 06).
type Acceptor struct {
	Store    DeliveryStore
	DemoMode bool
	Now      func() time.Time
}

// Accept validates and stores the request. The request is saved, recipients included,
// before success is reported, so an accepted request is never lost (AC-R001-01).
func (a *Acceptor) Accept(ctx context.Context, req domain.Request, traceparent string) (AcceptResult, error) {
	if err := req.Validate(a.DemoMode); err != nil {
		return AcceptResult{}, err
	}
	hash := req.ContentHash()

	existing, err := a.Store.FindByDedupeKey(ctx, req.DedupeKey)
	switch {
	case err == nil:
		return repeatOf(existing, hash)
	case !errors.Is(err, ErrNotFound):
		return AcceptResult{}, err
	}

	now := a.now()
	id, err := domain.NewDeliveryID(now, nil)
	if err != nil {
		return AcceptResult{}, err
	}
	acc := Accepted{
		ID:             id,
		DedupeKey:      req.DedupeKey,
		ContentHash:    hash,
		RecipientCount: len(req.Recipients),
		AcceptedAt:     now,
	}
	err = a.Store.Create(ctx, NewDelivery{Accepted: acc, Request: req, Traceparent: traceparent})
	if errors.Is(err, ErrDuplicateDedupeKey) {
		// Another request with the same key won the race. Answer as if it had arrived first.
		existing, findErr := a.Store.FindByDedupeKey(ctx, req.DedupeKey)
		if findErr != nil {
			return AcceptResult{}, findErr
		}
		return repeatOf(existing, hash)
	}
	if err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{Accepted: acc, Created: true}, nil
}

func repeatOf(existing Accepted, hash string) (AcceptResult, error) {
	if existing.ContentHash != hash {
		return AcceptResult{}, ErrDedupeConflict
	}
	return AcceptResult{Accepted: existing, Created: false}, nil
}

func (a *Acceptor) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}
