// Package signature computes the HMAC-SHA256 request signature shared by the delivery API and
// the webhooks (api/openapi.yaml). Both the HTTP API and the webhook client use it.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Signature headers (api/openapi.yaml).
const (
	HeaderKeyID     = "X-Relay-Key-Id"
	HeaderTimestamp = "X-Relay-Timestamp"
	HeaderSignature = "X-Relay-Signature"
	Prefix          = "v1="
)

// Sign returns the X-Relay-Signature value. The method and path are signed together with
// the body, so a signature for one operation cannot be reused for another.
func Sign(secret []byte, ts int64, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write([]byte(method))
	mac.Write([]byte("."))
	mac.Write([]byte(path))
	mac.Write([]byte("."))
	mac.Write(body)
	return Prefix + hex.EncodeToString(mac.Sum(nil))
}
