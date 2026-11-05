// Package httpapi exposes notify-relay over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReadinessCheck reports whether a dependency (such as the database) can serve requests.
type ReadinessCheck func(ctx context.Context) error

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// NewRouter builds the HTTP routes. checks are evaluated by /readyz (AC-R001-23).
func NewRouter(checks map[string]ReadinessCheck) http.Handler {
	mux := http.NewServeMux()

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
