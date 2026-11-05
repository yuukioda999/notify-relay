package config

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	secret32 = strings.Repeat("s", 32)
	key32    = base64.StdEncoding.EncodeToString([]byte(secret32))
)

func TestLoad(t *testing.T) {
	required := map[string]string{
		"DB_DSN":             "u:p@tcp(db:3306)/notify_relay",
		"RELAY_SIGNING_KEYS": "k2026-11:" + key32,
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range required {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	defaults := Config{
		Addr:            ":8080",
		ShutdownTimeout: 30 * time.Second,
		DatabaseDSN:     "u:p@tcp(db:3306)/notify_relay",
		SigningKeys:     map[string][]byte{"k2026-11": []byte(secret32)},
		SendConcurrency: 50,
		SMTP: SMTP{
			Addr: "localhost:587", From: "no-reply@team-ops.invalid", FromName: "チーム運営",
			TLS: "starttls", Footer: defaultFooter,
		},
	}

	tests := []struct {
		name    string
		env     map[string]string
		want    func() Config
		wantErr string
	}{
		{name: "defaults", env: with(nil), want: func() Config { return defaults }},
		{
			name: "overrides",
			env: with(map[string]string{
				"PORT": "9090", "SHUTDOWN_TIMEOUT": "5s", "DEMO_MODE": "true", "SEND_CONCURRENCY": "10",
				"SMTP_HOST": "mailpit", "SMTP_PORT": "1025", "SMTP_TLS": "none",
				"RELAY_SIGNING_KEYS": "k-old:" + key32 + ", k-new:" + key32,
			}),
			want: func() Config {
				c := defaults
				c.Addr, c.ShutdownTimeout, c.DemoMode, c.SendConcurrency = ":9090", 5*time.Second, true, 10
				c.SMTP.Addr, c.SMTP.TLS = "mailpit:1025", "none"
				c.SigningKeys = map[string][]byte{"k-old": []byte(secret32), "k-new": []byte(secret32)}
				return c
			},
		},
		{name: "invalid timeout", env: with(map[string]string{"SHUTDOWN_TIMEOUT": "soon"}), wantErr: "SHUTDOWN_TIMEOUT"},
		{name: "invalid demo flag", env: with(map[string]string{"DEMO_MODE": "maybe"}), wantErr: "DEMO_MODE"},
		{name: "missing DSN", env: map[string]string{"RELAY_SIGNING_KEYS": required["RELAY_SIGNING_KEYS"]}, wantErr: "DB_DSN"},
		{name: "missing signing keys", env: map[string]string{"DB_DSN": required["DB_DSN"]}, wantErr: "RELAY_SIGNING_KEYS"},
		{name: "short signing secret", env: with(map[string]string{"RELAY_SIGNING_KEYS": "k1:" + base64.StdEncoding.EncodeToString([]byte("short"))}), wantErr: "at least 32 bytes"},
		{name: "bad key id", env: with(map[string]string{"RELAY_SIGNING_KEYS": "Key One:" + key32}), wantErr: "keyid"},
		{name: "duplicate key id", env: with(map[string]string{"RELAY_SIGNING_KEYS": "k1:" + key32 + ",k1:" + key32}), wantErr: "twice"},
		{name: "unknown TLS mode", env: with(map[string]string{"SMTP_TLS": "maybe"}), wantErr: "SMTP_TLS"},
		{name: "zero concurrency", env: with(map[string]string{"SEND_CONCURRENCY": "0"}), wantErr: "SEND_CONCURRENCY"},
	}

	keys := []string{"PORT", "SHUTDOWN_TIMEOUT", "DEMO_MODE", "DB_DSN", "RELAY_SIGNING_KEYS", "SEND_CONCURRENCY",
		"SMTP_HOST", "SMTP_PORT", "SMTP_FROM", "SMTP_FROM_NAME", "SMTP_USERNAME", "SMTP_PASSWORD", "SMTP_TLS", "MAIL_FOOTER"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range keys {
				t.Setenv(k, tt.env[k])
			}
			got, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want it to mention %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), key32) || strings.Contains(err.Error(), secret32) {
					t.Fatal("error message leaks the signing secret")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if want := tt.want(); !reflect.DeepEqual(got, want) {
				t.Errorf("Load() = %+v, want %+v", got, want)
			}
		})
	}
}
