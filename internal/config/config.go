// Package config loads runtime settings from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds the settings notify-relay needs at startup.
type Config struct {
	// Addr is the listen address of the HTTP server, for example ":8080".
	Addr string
	// ShutdownTimeout bounds how long in-flight work may run after SIGTERM (AC-R001-14).
	ShutdownTimeout time.Duration
	// DemoMode restricts recipients to the reserved .invalid domain (AC-R001-06).
	DemoMode bool
	// DatabaseDSN points at notify-relay's own schema (ADR-0004).
	DatabaseDSN string
	// SigningKeys maps X-Relay-Key-Id to the shared secret. More than one key allows rotation.
	SigningKeys map[string][]byte
	// SendConcurrency is how many mails are sent at once (AC-R001-08).
	SendConcurrency int
	SMTP            SMTP
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
	if v := os.Getenv("DEMO_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("DEMO_MODE: %w", err))
		}
		cfg.DemoMode = b
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
	switch cfg.SMTP.TLS {
	case "none", "starttls", "tls":
	default:
		errs = append(errs, fmt.Errorf("SMTP_TLS must be none, starttls or tls"))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
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
