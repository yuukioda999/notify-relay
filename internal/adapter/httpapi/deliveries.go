package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

var traceparentPattern = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// DeliveryAcceptor is the use case behind POST /v1/deliveries.
type DeliveryAcceptor interface {
	Accept(ctx context.Context, req domain.Request, traceparent string) (usecase.AcceptResult, error)
}

type acceptedResponse struct {
	DeliveryID     domain.DeliveryID `json:"delivery_id"`
	DedupeKey      string            `json:"dedupe_key"`
	RecipientCount int               `json:"recipient_count"`
	AcceptedAt     string            `json:"accepted_at"`
}

func handleCreateDelivery(acceptor DeliveryAcceptor, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.Request
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			writeProblem(w, http.StatusBadRequest, "Request body is not valid JSON for a delivery request", nil)
			return
		}

		traceparent := r.Header.Get("traceparent")
		if !traceparentPattern.MatchString(traceparent) {
			traceparent = ""
		}

		res, err := acceptor.Accept(r.Context(), req, traceparent)
		var invalid *domain.ValidationError
		switch {
		case errors.As(err, &invalid):
			writeProblem(w, http.StatusUnprocessableEntity, "Delivery request is invalid", invalid.Errors)
			return
		case errors.Is(err, usecase.ErrDedupeConflict):
			writeProblem(w, http.StatusConflict, "dedupe_key was already used for a different request", nil)
			return
		case err != nil:
			// Nothing was confirmed as saved, so the caller must keep the request and retry (AC-R001-01).
			logger.Error("accept delivery", "error", err, "dedupe_key", req.DedupeKey)
			writeProblem(w, http.StatusServiceUnavailable, "Delivery request could not be saved; retry later", nil)
			return
		}

		code := http.StatusOK
		if res.Created {
			code = http.StatusAccepted
			logger.Info("delivery accepted", "delivery_id", res.ID, "dedupe_key", res.DedupeKey, "recipient_count", res.RecipientCount)
		}
		writeJSON(w, code, acceptedResponse{
			DeliveryID:     res.ID,
			DedupeKey:      res.DedupeKey,
			RecipientCount: res.RecipientCount,
			AcceptedAt:     res.AcceptedAt.UTC().Format(time.RFC3339),
		})
	}
}

type problem struct {
	Type   string              `json:"type"`
	Title  string              `json:"title"`
	Status int                 `json:"status"`
	Errors []domain.FieldError `json:"errors,omitempty"`
}

// writeProblem writes an RFC 9457 problem. Titles are fixed strings: request values,
// which can include addresses, are never echoed.
func writeProblem(w http.ResponseWriter, status int, title string, errs []domain.FieldError) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(problem{Type: "about:blank", Title: title, Status: status, Errors: errs})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
