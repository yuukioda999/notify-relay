// Package config loads runtime settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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
}

// Load reads Config from the environment, applying defaults for unset values.
func Load() (Config, error) {
	cfg := Config{
		Addr:            ":" + getEnv("PORT", "8080"),
		ShutdownTimeout: 30 * time.Second,
	}

	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err)
		}
		cfg.ShutdownTimeout = d
	}

	if v := os.Getenv("DEMO_MODE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("DEMO_MODE: %w", err)
		}
		cfg.DemoMode = b
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
