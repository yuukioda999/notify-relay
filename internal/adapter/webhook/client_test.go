package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/adapter/httpapi"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestClientSend(t *testing.T) {
	sentAt := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	event := usecase.WebhookEvent{
		ID:          "evt_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8",
		Payload:     []byte(`{"event_id":"evt_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8","type":"delivery.results","delivery_id":"dlv_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8","dedupe_key":"k-00000001","results":[]}`),
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	}
	tests := []struct {
		name    string
		status  int
		wantErr error // nil: success
		retry   bool  // any error other than ErrWebhookUnauthorized
	}{
		{name: "AC-R001-16 a 2xx answer is success", status: http.StatusNoContent},
		{name: "AC-R001-19 a 5xx answer is retried", status: http.StatusServiceUnavailable, retry: true},
		{name: "AC-R001-20 a 401 answer is not retried", status: http.StatusUnauthorized, wantErr: usecase.ErrWebhookUnauthorized},
		{name: "AC-R001-19 a redirect is not followed and is retried", status: http.StatusFound, retry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *http.Request
			var body []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r
				body, _ = io.ReadAll(r.Body)
				if tt.status == http.StatusFound {
					w.Header().Set("Location", "https://elsewhere.invalid/")
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := &Client{URL: srv.URL + "/api/internal/notify-relay/events", KeyID: "k1", Secret: secret}
			err := c.Send(context.Background(), event, sentAt)
			switch {
			case tt.retry:
				if err == nil || errors.Is(err, usecase.ErrWebhookUnauthorized) {
					t.Fatalf("Send() = %v, want an error to retry", err)
				}
			case tt.wantErr == nil && err != nil:
				t.Fatalf("Send() = %v", err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("Send() = %v, want %v", err, tt.wantErr)
			}

			// AC-R001-18 signature, time, event_id and traceparent
			ts := got.Header.Get(httpapi.HeaderTimestamp)
			if ts != strconv.FormatInt(sentAt.Unix(), 10) || got.Header.Get(httpapi.HeaderKeyID) != "k1" {
				t.Fatalf("headers = %v", got.Header)
			}
			if want := httpapi.Sign(secret, sentAt.Unix(), http.MethodPost, "/api/internal/notify-relay/events", body); got.Header.Get(httpapi.HeaderSignature) != want {
				t.Fatal("signature does not match the body")
			}
			if got.Header.Get("traceparent") != event.Traceparent {
				t.Fatalf("traceparent = %q", got.Header.Get("traceparent"))
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			if m["event_id"] != event.ID || m["sent_at"] != "2026-11-07T12:00:00Z" {
				t.Fatalf("body = %s", body)
			}
		})
	}
}
