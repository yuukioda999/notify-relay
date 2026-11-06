// Package httpapi exposes notify-relay over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// ReadinessCheck reports whether a dependency (such as the database) can serve requests.
type ReadinessCheck func(ctx context.Context) error

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Deps are what the routes need.
type Deps struct {
	// Checks are evaluated by /readyz (AC-R001-23).
	Checks map[string]ReadinessCheck
	// Acceptor handles POST /v1/deliveries. The route is not registered when nil.
	Acceptor DeliveryAcceptor
	// Keys maps X-Relay-Key-Id to the shared secret used for request signatures.
	Keys   map[string][]byte
	Now    func() time.Time
	Logger *slog.Logger
}

// NewRouter builds the HTTP routes.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	checks := d.Checks
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Acceptor != nil {
		mux.Handle("POST /v1/deliveries", requireSignature(d.Keys, d.Now, handleCreateDelivery(d.Acceptor, d.Logger)))
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		res := healthResponse{Status: "ok", Checks: map[string]string{}}
		code := http.StatusOK
		for name, check := range checks {
			if err := check(ctx); err != nil {
				res.Checks[name] = "unavailable"
				res.Status = "unavailable"
				code = http.StatusServiceUnavailable
				continue
			}
			res.Checks[name] = "ok"
		}
		writeJSON(w, code, res)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
