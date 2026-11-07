package httpapi

import (
	"bytes"
	"crypto/hmac"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/yuukioda999/notify-relay/internal/adapter/signature"
	"github.com/yuukioda999/notify-relay/internal/domain"
)

// Signature headers (api/openapi.yaml), shared with the webhook client through package signature.
const (
	HeaderKeyID     = signature.HeaderKeyID
	HeaderTimestamp = signature.HeaderTimestamp
	HeaderSignature = signature.HeaderSignature
	// MaxClockSkew is how far the request time may differ from ours (replay protection).
	MaxClockSkew = 5 * time.Minute
)

var signaturePattern = regexp.MustCompile(`^v1=[0-9a-f]{64}$`)

// Sign returns the X-Relay-Signature value (see package signature).
func Sign(secret []byte, ts int64, method, path string, body []byte) string {
	return signature.Sign(secret, ts, method, path, body)
}

// requireSignature rejects requests whose signature, key ID or time is wrong, before the
// handler sees them (AC-R001-02). Headers are checked before the body is read, so an
// unsigned request cannot make us buffer up to 2MB. It also enforces the body limit (AC-R001-05).
func requireSignature(keys map[string][]byte, now func() time.Time, next http.Handler) http.Handler {
	unauthorized := func(w http.ResponseWriter) {
		// One answer for every reason, so callers cannot probe which part was wrong.
		writeProblem(w, http.StatusUnauthorized, "Signature is invalid", nil)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ts, ok := checkHeaders(keys, now(), r.Header)
		if !ok {
			unauthorized(w)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, domain.MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeProblem(w, http.StatusRequestEntityTooLarge, "Request body is larger than 2MB", nil)
				return
			}
			writeProblem(w, http.StatusBadRequest, "Request body could not be read", nil)
			return
		}

		want := Sign(secret, ts, r.Method, r.URL.Path, body)
		if !hmac.Equal([]byte(r.Header.Get(HeaderSignature)), []byte(want)) {
			unauthorized(w)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// checkHeaders validates everything that does not need the body.
func checkHeaders(keys map[string][]byte, now time.Time, h http.Header) (secret []byte, ts int64, ok bool) {
	secret, found := keys[h.Get(HeaderKeyID)]
	if !found || len(secret) == 0 {
		return nil, 0, false
	}
	ts, err := strconv.ParseInt(h.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return nil, 0, false
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return nil, 0, false
	}
	if !signaturePattern.MatchString(h.Get(HeaderSignature)) {
		return nil, 0, false
	}
	return secret, ts, true
}
