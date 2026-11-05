package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// Signature headers (api/openapi.yaml).
const (
	HeaderKeyID     = "X-Relay-Key-Id"
	HeaderTimestamp = "X-Relay-Timestamp"
	HeaderSignature = "X-Relay-Signature"
	signaturePrefix = "v1="
	// MaxClockSkew is how far the request time may differ from ours (replay protection).
	MaxClockSkew = 5 * time.Minute
)

// Sign returns the X-Relay-Signature value for body sent at ts.
func Sign(secret []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// requireSignature rejects requests whose signature, key ID or time is wrong, before the
// handler sees them (AC-R001-02). It also enforces the 2MB body limit (AC-R001-05).
func requireSignature(keys map[string][]byte, now func() time.Time, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		if !validSignature(keys, now(), r.Header, body) {
			// One answer for every reason, so callers cannot probe which part was wrong.
			writeProblem(w, http.StatusUnauthorized, "Signature is invalid", nil)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

func validSignature(keys map[string][]byte, now time.Time, h http.Header, body []byte) bool {
	secret, ok := keys[h.Get(HeaderKeyID)]
	if !ok || len(secret) == 0 {
		return false
	}
	ts, err := strconv.ParseInt(h.Get(HeaderTimestamp), 10, 64)
	if err != nil {
		return false
	}
	skew := now.Sub(time.Unix(ts, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return false
	}
	got := h.Get(HeaderSignature)
	if !strings.HasPrefix(got, signaturePrefix) {
		return false
	}
	return hmac.Equal([]byte(got), []byte(Sign(secret, ts, body)))
}
