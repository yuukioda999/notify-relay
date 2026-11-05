package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthEndpoints(t *testing.T) {
	ok := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("db down") }

	tests := []struct {
		name       string
		path       string
		checks     map[string]ReadinessCheck
		wantCode   int
		wantStatus string
	}{
		{name: "liveness is always ok", path: "/healthz", checks: map[string]ReadinessCheck{"db": down}, wantCode: http.StatusOK, wantStatus: "ok"},
		{name: "ready when all checks pass", path: "/readyz", checks: map[string]ReadinessCheck{"db": ok}, wantCode: http.StatusOK, wantStatus: "ok"},
		{name: "not ready when a check fails", path: "/readyz", checks: map[string]ReadinessCheck{"db": down}, wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRouter(tt.checks).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			if rec.Code != tt.wantCode {
				t.Fatalf("status code = %d, want %d", rec.Code, tt.wantCode)
			}
			var body healthResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", body.Status, tt.wantStatus)
			}
		})
	}
}

func TestUnknownMethodIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter(nil).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
