// Package config loads runtime settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yuukioda999/notify-relay/internal/domain"
)

// Config holds the settings notify-relay needs at startup.
type Config struct {
	// Addr is the listen address of the HTTP server, for example ":8080".
	Addr string
	// ShutdownTimeout bounds how long in-flight work may run after SIGTERM (AC-R001-14).
	ShutdownTimeout time.Duration
	// InvalidRecipientsOnly restricts recipients to the reserved .invalid domain (AC-R001-06).
	// It is on when RECIPIENTS_INVALID_ONLY or DEMO_MODE is true: team-ops-api uses the first
	// name, and either one on its own must keep this side of the double check (AC-007-13).
	InvalidRecipientsOnly bool
	// DatabaseDSN points at notify-relay's own schema (ADR-0004).
	DatabaseDSN string
	// SigningKeys maps X-Relay-Key-Id to the shared secret. More than one key allows rotation.
	SigningKeys map[string][]byte
	// EmailRefKey keys the email_ref kept in logs and delivery records (ADR-0003).
	EmailRefKey []byte
	// SendConcurrency is how many mails are sent at once (AC-R001-08).
	SendConcurrency int
	SMTP            SMTP
	// Webhook is where results are sent (AC-R001-16 to 20). Webhooks are off when URL is empty.
	Webhook Webhook
}

// Webhook is the business app's fixed result endpoint and the key used to sign for it.
type Webhook struct {
	URL   string
	KeyID string
}

// SMTP is the outgoing mail server.
type SMTP struct {
	Addr     string
	From     string
	FromName string
	Username string
	Password string
	TLS      string // none, starttls or tls
	Footer   string
}

var keyIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

const defaultFooter = "このメールは送信専用です。返信いただいてもお答えできません。"

// publishedLocalKeys are dummy secrets committed for local development (team-ops compose.yaml).
// Anyone can read them, so they are refused unless ALLOW_LOCAL_SIGNING_KEY=true.
var publishedLocalKeys = []string{"local-dev-relay-signing-key-0001", "local-dev-relay-email-ref-key-001"}

// plaintextSMTPHosts may be used with SMTP_TLS=none: they are on the same machine or the
// local compose network, so addresses and login links do not cross a real network in clear.
var plaintextSMTPHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "mailpit": true}

// plaintextWebhookHosts may receive webhooks over http: the same machine or the compose network.
var plaintextWebhookHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "api": true}

// Load reads Config from the environment, applying defaults for unset values.
func Load() (Config, error) {
	cfg := Config{
		Addr:            ":" + getEnv("PORT", "8080"),
		ShutdownTimeout: 30 * time.Second,
		DatabaseDSN:     os.Getenv("DB_DSN"),
		SendConcurrency: 50,
		SMTP: SMTP{
			Addr:     getEnv("SMTP_HOST", "localhost") + ":" + getEnv("SMTP_PORT", "587"),
			From:     getEnv("SMTP_FROM", "no-reply@team-ops.invalid"),
			FromName: getEnv("SMTP_FROM_NAME", "チーム運営"),
			Username: os.Getenv("SMTP_USERNAME"),
			Password: os.Getenv("SMTP_PASSWORD"),
			TLS:      getEnv("SMTP_TLS", "starttls"),
			Footer:   getEnv("MAIL_FOOTER", defaultFooter),
		},
	}

	var errs []error
	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
		}
		cfg.ShutdownTimeout = d
	}
	for _, name := range []string{"RECIPIENTS_INVALID_ONLY", "DEMO_MODE"} {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		cfg.InvalidRecipientsOnly = cfg.InvalidRecipientsOnly || b
	}
	if v := os.Getenv("SEND_CONCURRENCY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("SEND_CONCURRENCY must be a positive integer"))
		}
		cfg.SendConcurrency = n
	}
	if cfg.DatabaseDSN == "" {
		errs = append(errs, errors.New("DB_DSN is required"))
	}
	keys, err := parseKeys(os.Getenv("RELAY_SIGNING_KEYS"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.SigningKeys = keys
	if os.Getenv("ALLOW_LOCAL_SIGNING_KEY") != "true" {
		for id, secret := range keys {
			for _, published := range publishedLocalKeys {
				if string(secret) == published {
					errs = append(errs, fmt.Errorf("RELAY_SIGNING_KEYS: key %q is the published local development key; set a real secret", id))
				}
			}
		}
	}
	refKey, err := base64.StdEncoding.DecodeString(os.Getenv("EMAIL_REF_KEY"))
	switch {
	case os.Getenv("EMAIL_REF_KEY") == "":
		errs = append(errs, errors.New("EMAIL_REF_KEY is required"))
	case err != nil || len(refKey) < domain.MinEmailRefKeyBytes:
		errs = append(errs, errors.New("EMAIL_REF_KEY must be base64 of at least 32 bytes"))
	case os.Getenv("ALLOW_LOCAL_SIGNING_KEY") != "true" && slices.Contains(publishedLocalKeys, string(refKey)):
		errs = append(errs, errors.New("EMAIL_REF_KEY is the published local development key; set a real secret"))
	}
	cfg.EmailRefKey = refKey
	switch cfg.SMTP.TLS {
	case "starttls", "tls":
	case "none":
		if !plaintextSMTPHosts[getEnv("SMTP_HOST", "localhost")] {
			errs = append(errs, fmt.Errorf("SMTP_TLS=none is allowed only for a local SMTP server (localhost or mailpit)"))
		}
	default:
		errs = append(errs, fmt.Errorf("SMTP_TLS must be none, starttls or tls"))
	}

	webhook, err := parseWebhook(os.Getenv("WEBHOOK_URL"), os.Getenv("WEBHOOK_KEY_ID"), keys)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.Webhook = webhook

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// parseWebhook checks the result endpoint. The URL is fixed here and never taken from a
// request, so results cannot be sent somewhere unintended. It must be https, except for a
// receiver on the same machine or the compose network. The key ID defaults to the only key.
func parseWebhook(rawURL, keyID string, keys map[string][]byte) (Webhook, error) {
	if rawURL == "" {
		return Webhook{}, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.User != nil {
		return Webhook{}, errors.New("WEBHOOK_URL must be an absolute http(s) URL without query or credentials")
	}
	if u.Scheme == "http" && !plaintextWebhookHosts[u.Hostname()] {
		return Webhook{}, errors.New("WEBHOOK_URL must use https, except for localhost or the compose service api")
	}
	if keyID == "" && len(keys) == 1 {
		for id := range keys {
			keyID = id
		}
	}
	if _, ok := keys[keyID]; !ok {
		return Webhook{}, errors.New("WEBHOOK_KEY_ID must name one of RELAY_SIGNING_KEYS")
	}
	return Webhook{URL: rawURL, KeyID: keyID}, nil
}

// parseKeys reads "keyid:base64secret,keyid2:base64secret". Secrets must be at least 32 bytes.
// Error messages never include the secret.
func parseKeys(v string) (map[string][]byte, error) {
	if strings.TrimSpace(v) == "" {
		return nil, errors.New("RELAY_SIGNING_KEYS is required")
	}
	keys := map[string][]byte{}
	for part := range strings.SplitSeq(v, ",") {
		id, secret, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || !keyIDPattern.MatchString(id) {
			return nil, errors.New("RELAY_SIGNING_KEYS: each entry must be keyid:base64secret with keyid matching ^[a-z0-9-]{1,32}$")
		}
		b, err := base64.StdEncoding.DecodeString(secret)
		if err != nil || len(b) < 32 {
			return nil, fmt.Errorf("RELAY_SIGNING_KEYS: secret for %q must be base64 of at least 32 bytes", id)
		}
		if _, dup := keys[id]; dup {
			return nil, fmt.Errorf("RELAY_SIGNING_KEYS: key %q appears twice", id)
		}
		keys[id] = b
	}
	return keys, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
