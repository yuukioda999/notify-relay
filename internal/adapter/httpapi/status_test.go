package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

type stubStatus struct {
	st  usecase.DeliveryStatus
	err error
}

func (s stubStatus) DeliveryStatus(context.Context, domain.DeliveryID) (usecase.DeliveryStatus, error) {
	return s.st, s.err
}

func TestGetDelivery(t *testing.T) {
	const id = "dlv_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8"
	done := testNow.Add(time.Minute)
	tests := []struct {
		name     string
		path     string
		sign     bool
		reader   stubStatus
		wantCode int
		wantBody string
	}{
		{
			name: "AC-R001-21 returns the state and counts", path: "/v1/deliveries/" + id, sign: true,
			reader: stubStatus{st: usecase.DeliveryStatus{ID: id, DedupeKey: "k-00000001", State: usecase.StateCompleted,
				Counts: usecase.ResultCounts{Delivered: 2, Failed: 1}, AcceptedAt: testNow, CompletedAt: &done}},
			wantCode: http.StatusOK,
			wantBody: `{"delivery_id":"dlv_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8","dedupe_key":"k-00000001","state":"completed","counts":{"pending":0,"delivered":2,"failed":1,"skipped":0},"accepted_at":"2026-11-05T12:00:00Z","completed_at":"2026-11-05T12:01:00Z"}`,
		},
		{name: "AC-R001-02 an unsigned query is refused", path: "/v1/deliveries/" + id, sign: false, wantCode: http.StatusUnauthorized},
		{name: "AC-R001-22 a purged delivery is 404", path: "/v1/deliveries/" + id, sign: true, reader: stubStatus{err: usecase.ErrNotFound}, wantCode: http.StatusNotFound},
		{name: "AC-R001-21 a malformed ID is 404", path: "/v1/deliveries/nope", sign: true, wantCode: http.StatusNotFound},
		{name: "AC-R001-21 a database error is 503", path: "/v1/deliveries/" + id, sign: true, reader: stubStatus{err: errors.New("db down")}, wantCode: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := NewRouter(Deps{
				Status: tt.reader,
				Keys:   map[string][]byte{"k2026-11": testSecret},
				Now:    func() time.Time { return testNow },
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			if tt.sign {
				r.Header.Set(HeaderKeyID, "k2026-11")
				r.Header.Set(HeaderTimestamp, strconv.FormatInt(testNow.Unix(), 10))
				r.Header.Set(HeaderSignature, Sign(testSecret, testNow.Unix(), http.MethodGet, tt.path, nil))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, r)
			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body)
			}
			if tt.wantBody != "" {
				var got, want any
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				_ = json.Unmarshal([]byte(tt.wantBody), &want)
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				if string(gb) != string(wb) {
					t.Fatalf("body = %s, want %s", gb, wb)
				}
			}
		})
	}
}
