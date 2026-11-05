package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: Config{Addr: ":8080", ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "overrides",
			env:  map[string]string{"PORT": "9090", "SHUTDOWN_TIMEOUT": "5s", "DEMO_MODE": "true"},
			want: Config{Addr: ":9090", ShutdownTimeout: 5 * time.Second, DemoMode: true},
		},
		{
			name:    "invalid timeout",
			env:     map[string]string{"SHUTDOWN_TIMEOUT": "soon"},
			wantErr: true,
		},
		{
			name:    "invalid demo flag",
			env:     map[string]string{"DEMO_MODE": "maybe"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"PORT", "SHUTDOWN_TIMEOUT", "DEMO_MODE"} {
				t.Setenv(k, tt.env[k])
			}
			got, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("Load() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
