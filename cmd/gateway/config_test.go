package main

import (
	"testing"
	"time"
)

// TestLoadConfig checks default configuration values, environment overrides,
// and validation of invalid or non-positive durations.
func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		want      config
		wantError bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: config{
				addr:              ":8080",
				readHeaderTimeout: 5 * time.Second,
				idleTimeout:       60 * time.Second,
				shutdownTimeout:   10 * time.Second,
			},
		},
		{
			name: "custom environment values",
			env: map[string]string{
				"GATEWAY_ADDR":                "127.0.0.1:9090",
				"GATEWAY_READ_HEADER_TIMEOUT": "2s",
				"GATEWAY_IDLE_TIMEOUT":        "30s",
				"GATEWAY_SHUTDOWN_TIMEOUT":    "15s",
			},
			want: config{
				addr:              "127.0.0.1:9090",
				readHeaderTimeout: 2 * time.Second,
				idleTimeout:       30 * time.Second,
				shutdownTimeout:   15 * time.Second,
			},
		},
		{
			name: "invalid read header timeout syntax",
			env: map[string]string{
				"GATEWAY_READ_HEADER_TIMEOUT": "not-a-duration",
			},
			wantError: true,
		},
		{
			name: "negative read header timeout",
			env: map[string]string{
				"GATEWAY_READ_HEADER_TIMEOUT": "-1s",
			},
			wantError: true,
		},
		{
			name: "zero read header timeout",
			env: map[string]string{
				"GATEWAY_READ_HEADER_TIMEOUT": "0s",
			},
			wantError: true,
		},
		{
			name: "invalid idle timeout syntax",
			env: map[string]string{
				"GATEWAY_IDLE_TIMEOUT": "invalid",
			},
			wantError: true,
		},
		{
			name: "zero idle timeout",
			env: map[string]string{
				"GATEWAY_IDLE_TIMEOUT": "0s",
			},
			wantError: true,
		},
		{
			name: "invalid shutdown timeout syntax",
			env: map[string]string{
				"GATEWAY_SHUTDOWN_TIMEOUT": "invalid",
			},
			wantError: true,
		},
		{
			name: "zero shutdown timeout",
			env: map[string]string{
				"GATEWAY_SHUTDOWN_TIMEOUT": "0s",
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				return tt.env[key]
			}
			cfg, err := loadConfig(getenv)
			if tt.wantError {
				if err == nil {
					t.Fatal("loadConfig() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}
			if cfg != tt.want {
				t.Fatalf("loadConfig() = %+v, want %+v", cfg, tt.want)
			}
		})
	}
}
