// Package webhook posts result events to the business app (AC-R001-16 to 20).
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/yuukioda999/notify-relay/internal/adapter/signature"
	"github.com/yuukioda999/notify-relay/internal/usecase"
)

// Timeout is how long the business app has to answer (AC-R001-19).
const Timeout = 10 * time.Second

// Client signs events like the delivery API (api/openapi.yaml) and posts them to one fixed URL.
type Client struct {
	URL    string
	KeyID  string
	Secret []byte
	HTTP   *http.Client
	// Timeout overrides the 10-second limit (tests use a shorter one).
	Timeout time.Duration
}

var _ usecase.WebhookSender = (*Client)(nil)

// Send adds sent_at to the saved body, signs it and posts it. A 2xx answer is success,
// 401 is usecase.ErrWebhookUnauthorized, and anything else (or no answer) is an error to retry.
func (c *Client) Send(ctx context.Context, e usecase.WebhookEvent, sentAt time.Time) error {
	var body map[string]any
	if err := json.Unmarshal(e.Payload, &body); err != nil {
		return fmt.Errorf("event %s has a broken payload: %w", e.ID, err)
	}
	body["sent_at"] = sentAt.UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	ts := sentAt.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signature.HeaderKeyID, c.KeyID)
	req.Header.Set(signature.HeaderTimestamp, fmt.Sprint(ts))
	req.Header.Set(signature.HeaderSignature, signature.Sign(c.Secret, ts, http.MethodPost, u.EscapedPath(), raw))
	if e.Traceparent != "" {
		req.Header.Set("traceparent", e.Traceparent)
	}

	client := c.HTTP
	if client == nil {
		// Never follow a redirect: the receiver URL is fixed by configuration
		client = &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return usecase.ErrWebhookUnauthorized
	default:
		return errors.New("webhook receiver answered " + resp.Status)
	}
}
