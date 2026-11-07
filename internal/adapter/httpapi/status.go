package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

var deliveryIDPattern = regexp.MustCompile(`^dlv_[0-9A-HJKMNP-TV-Z]{26}$`)

type statusResponse struct {
	DeliveryID  domain.DeliveryID    `json:"delivery_id"`
	DedupeKey   string               `json:"dedupe_key"`
	State       string               `json:"state"`
	Counts      usecase.ResultCounts `json:"counts"`
	AcceptedAt  string               `json:"accepted_at"`
	CompletedAt *string              `json:"completed_at"`
}

// handleGetDelivery answers the status query with counts only (AC-R001-21). It is for
// reconciling missed webhooks, so it has no per-recipient detail.
func handleGetDelivery(reader usecase.StatusReader, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("delivery_id")
		if !deliveryIDPattern.MatchString(id) {
			writeProblem(w, http.StatusNotFound, "Delivery not found", nil)
			return
		}
		st, err := reader.DeliveryStatus(r.Context(), domain.DeliveryID(id))
		switch {
		case errors.Is(err, usecase.ErrNotFound):
			writeProblem(w, http.StatusNotFound, "Delivery not found", nil)
			return
		case err != nil:
			logger.Error("read delivery status", "error", err, "delivery_id", id)
			writeProblem(w, http.StatusServiceUnavailable, "Delivery status could not be read; retry later", nil)
			return
		}
		res := statusResponse{
			DeliveryID: st.ID,
			DedupeKey:  st.DedupeKey,
			State:      st.State,
			Counts:     st.Counts,
			AcceptedAt: st.AcceptedAt.UTC().Format(time.RFC3339),
		}
		if st.CompletedAt != nil {
			c := st.CompletedAt.UTC().Format(time.RFC3339)
			res.CompletedAt = &c
		}
		writeJSON(w, http.StatusOK, res)
	}
}
