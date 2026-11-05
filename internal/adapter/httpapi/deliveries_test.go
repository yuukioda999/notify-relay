package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

var (
	testNow    = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	testSecret = []byte("0123456789abcdef0123456789abcdef")
)

type stubAcceptor struct {
	result usecase.AcceptResult
	err    error
	calls  int
	got    domain.Request
	trace  string
}

func (s *stubAcceptor) Accept(_ context.Context, req domain.Request, traceparent string) (usecase.AcceptResult, error) {
	s.calls++
	s.got = req
	s.trace = traceparent
	return s.result, s.err
}

const validBody = `{"dedupe_key":"login-link:1","tenant_ref":"system","message":{"subject":"件名","text":"本文"},"recipients":[{"recipient_ref":"user_1","email":"coach@example.invalid"}]}`

func signedRequest(t *testing.T, body string, opts ...func(h http.Header, body []byte)) *http.Request {
	t.Helper()
	ts := testNow.Unix()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/deliveries", strings.NewReader(body))
	r.Header.Set(HeaderKeyID, "k2026-11")
	r.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	r.Header.Set(HeaderSignature, Sign(testSecret, ts, http.MethodPost, "/v1/deliveries", []byte(body)))
	for _, o := range opts {
		o(r.Header, []byte(body))
	}
	return r
}

func newTestRouter(acc DeliveryAcceptor) http.Handler {
	return NewRouter(Deps{
		Acceptor: acc,
		Keys:     map[string][]byte{"k2026-11": testSecret},
		Now:      func() time.Time { return testNow },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestCreateDeliverySignature(t *testing.T) {
	resign := func(ts int64) func(http.Header, []byte) {
		return func(h http.Header, body []byte) {
			h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
			h.Set(HeaderSignature, Sign(testSecret, ts, http.MethodPost, "/v1/deliveries", body))
		}
	}
	tests := []struct {
		name string
		opt  func(http.Header, []byte)
		want int
	}{
		{name: "AC-R001-01 a correctly signed request is accepted", opt: func(http.Header, []byte) {}, want: http.StatusAccepted},
		{name: "AC-R001-02 a wrong signature is rejected", opt: func(h http.Header, _ []byte) { h.Set(HeaderSignature, "v1="+strings.Repeat("0", 64)) }, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a missing signature is rejected", opt: func(h http.Header, _ []byte) { h.Del(HeaderSignature) }, want: http.StatusUnauthorized},
		{name: "AC-R001-02 an unknown key ID is rejected", opt: func(h http.Header, _ []byte) { h.Set(HeaderKeyID, "k-unknown") }, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a request from 6 minutes ago is rejected", opt: resign(testNow.Add(-6 * time.Minute).Unix()), want: http.StatusUnauthorized},
		{name: "AC-R001-02 a request from 6 minutes in the future is rejected", opt: resign(testNow.Add(6 * time.Minute).Unix()), want: http.StatusUnauthorized},
		{name: "AC-R001-02 a request from 4 minutes ago is accepted", opt: resign(testNow.Add(-4 * time.Minute).Unix()), want: http.StatusAccepted},
		{name: "AC-R001-02 a non-numeric timestamp is rejected", opt: func(h http.Header, _ []byte) { h.Set(HeaderTimestamp, "yesterday") }, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a signature over a different body is rejected", opt: func(h http.Header, _ []byte) {
			h.Set(HeaderSignature, Sign(testSecret, testNow.Unix(), http.MethodPost, "/v1/deliveries", []byte(`{"other":true}`)))
		}, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a signature made for another path is rejected", opt: func(h http.Header, body []byte) {
			h.Set(HeaderSignature, Sign(testSecret, testNow.Unix(), http.MethodPost, "/v1/other", body))
		}, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a signature made for another method is rejected", opt: func(h http.Header, body []byte) {
			h.Set(HeaderSignature, Sign(testSecret, testNow.Unix(), http.MethodGet, "/v1/deliveries", body))
		}, want: http.StatusUnauthorized},
		{name: "AC-R001-02 a malformed signature is rejected", opt: func(h http.Header, _ []byte) { h.Set(HeaderSignature, "v1=xyz") }, want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := &stubAcceptor{result: usecase.AcceptResult{Created: true, Accepted: usecase.Accepted{ID: "dlv_X", RecipientCount: 1, AcceptedAt: testNow}}}
			rec := httptest.NewRecorder()
			newTestRouter(acc).ServeHTTP(rec, signedRequest(t, validBody, tt.opt))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusUnauthorized && acc.calls != 0 {
				t.Fatal("an unauthenticated request reached the use case (nothing may be saved)")
			}
		})
	}
}

func TestCreateDeliveryResponses(t *testing.T) {
	accepted := usecase.Accepted{ID: "dlv_01JC9ZB4X7Q2N6R8T0V2W4Y6Z8", DedupeKey: "login-link:1", RecipientCount: 1, AcceptedAt: testNow}
	tests := []struct {
		name     string
		body     string
		acceptor *stubAcceptor
		want     int
		wantType string
		pointer  string // expected pointer in a 422 body
	}{
		{name: "AC-R001-01 a new request returns 202", body: validBody, acceptor: &stubAcceptor{result: usecase.AcceptResult{Accepted: accepted, Created: true}}, want: http.StatusAccepted, wantType: "application/json"},
		{name: "AC-R001-03 a repeated request returns 200 with the first delivery", body: validBody, acceptor: &stubAcceptor{result: usecase.AcceptResult{Accepted: accepted, Created: false}}, want: http.StatusOK, wantType: "application/json"},
		{name: "AC-R001-04 a conflicting request returns 409", body: validBody, acceptor: &stubAcceptor{err: usecase.ErrDedupeConflict}, want: http.StatusConflict, wantType: "application/problem+json"},
		{name: "AC-R001-01 a storage failure returns 503, not 202", body: validBody, acceptor: &stubAcceptor{err: errors.New("db down")}, want: http.StatusServiceUnavailable, wantType: "application/problem+json"},
		{name: "AC-R001-06 an invalid request returns 422 with JSON pointers", body: validBody, acceptor: &stubAcceptor{err: &domain.ValidationError{Errors: []domain.FieldError{{Pointer: "/recipients/0/email", Message: "demo mode accepts only .invalid addresses"}}}}, want: http.StatusUnprocessableEntity, wantType: "application/problem+json", pointer: "/recipients/0/email"},
		{name: "broken JSON returns 400", body: `{"dedupe_key":`, acceptor: &stubAcceptor{}, want: http.StatusBadRequest, wantType: "application/problem+json"},
		{name: "two JSON values return 400", body: validBody + validBody, acceptor: &stubAcceptor{}, want: http.StatusBadRequest, wantType: "application/problem+json"},
		{name: "unknown fields return 422", body: strings.Replace(validBody, `"tenant_ref"`, `"surprise":1,"tenant_ref"`, 1), acceptor: &stubAcceptor{}, want: http.StatusUnprocessableEntity, wantType: "application/problem+json", pointer: ""},
		{name: "a wrong type returns 422", body: strings.Replace(validBody, `"tenant_ref":"system"`, `"tenant_ref":42`, 1), acceptor: &stubAcceptor{}, want: http.StatusUnprocessableEntity, wantType: "application/problem+json", pointer: "/tenant_ref"},
		{name: "a JSON array returns 422", body: `[1,2]`, acceptor: &stubAcceptor{}, want: http.StatusUnprocessableEntity, wantType: "application/problem+json", pointer: ""},
		{name: "AC-R001-05 a body over 2MB returns 413", body: `{"dedupe_key":"` + strings.Repeat("x", domain.MaxBodyBytes) + `"}`, acceptor: &stubAcceptor{}, want: http.StatusRequestEntityTooLarge, wantType: "application/problem+json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestRouter(tt.acceptor).ServeHTTP(rec, signedRequest(t, tt.body))

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); got != tt.wantType {
				t.Fatalf("Content-Type = %q, want %q", got, tt.wantType)
			}
			if strings.Contains(rec.Body.String(), "coach@example.invalid") {
				t.Fatal("response echoes the recipient address")
			}
			if tt.want == http.StatusAccepted || tt.want == http.StatusOK {
				var got acceptedResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.DeliveryID != accepted.ID || got.RecipientCount != 1 || got.AcceptedAt != "2026-11-05T12:00:00Z" {
					t.Fatalf("body = %+v", got)
				}
			}
			if tt.want == http.StatusUnprocessableEntity && !bytes.Contains(rec.Body.Bytes(), []byte(`"pointer":"`+tt.pointer+`"`)) {
				t.Fatalf("422 body lacks the pointer %q: %s", tt.pointer, rec.Body)
			}
			if tt.want == http.StatusUnprocessableEntity && strings.Contains(rec.Body.String(), "surprise") {
				t.Fatal("422 body echoes the request")
			}
		})
	}
}

func TestCreateDeliveryPassesTraceparent(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "AC-R001-25 a valid traceparent is kept", header: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", want: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		{name: "a malformed traceparent is dropped", header: "not-a-trace", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := &stubAcceptor{result: usecase.AcceptResult{Created: true}}
			r := signedRequest(t, validBody)
			r.Header.Set("traceparent", tt.header)
			newTestRouter(acc).ServeHTTP(httptest.NewRecorder(), r)
			if acc.trace != tt.want {
				t.Fatalf("traceparent = %q, want %q", acc.trace, tt.want)
			}
		})
	}
}

// TestSignKnownVector pins the signature format. team-ops-api tests the same vector, so the
// two sides cannot drift apart.
func TestSignKnownVector(t *testing.T) {
	got := Sign([]byte("0123456789abcdef0123456789abcdef"), 1793502000, http.MethodPost, "/v1/deliveries", []byte(`{"dedupe_key":"login-link:1"}`))
	const want = "v1=aeefeda4cddc7dc373c15cefb78705f1cae71280291b4b3322fbe292f1bea113"
	if got != want {
		t.Fatalf("Sign() = %s, want %s", got, want)
	}
}
