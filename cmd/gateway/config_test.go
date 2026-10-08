package main

import (
	"strings"
	"testing"
	"time"
)

// TestLoadConfigUpstreamURL checks optional backend URLs and rejects unsafe or invalid settings.
func TestLoadConfigUpstreamURL(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantError bool
	}{
		{name: "unset"},
		{name: "HTTP backend", value: "http://localhost:8081"},
		{name: "HTTPS base path", value: "https://backend.example/api/"},
		{name: "IPv6 backend", value: "http://[::1]:8081"},
		{name: "relative path", value: "/api", wantError: true},
		{name: "missing host", value: "http:///api", wantError: true},
		{name: "unsupported scheme", value: "ftp://backend.example", wantError: true},
		{name: "credentials", value: "http://user:secret@localhost:8081", wantError: true},
		{name: "malformed credentials", value: "http://user:secret%zz@localhost", wantError: true},
		{name: "query", value: "http://localhost?token=secret", wantError: true},
		{name: "empty query", value: "http://localhost?", wantError: true},
		{name: "fragment", value: "http://localhost#secret", wantError: true},
		{name: "empty fragment", value: "http://localhost#", wantError: true},
		{name: "invalid port", value: "http://localhost:invalid", wantError: true},
		{name: "zero port", value: "http://localhost:0", wantError: true},
		{name: "out of range port", value: "http://localhost:65536", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "GATEWAY_UPSTREAM_URL" {
					return tt.value
				}
				return ""
			})
			if tt.wantError {
				if err == nil {
					t.Fatal("loadConfig() expected error, got nil")
				}
				if !strings.Contains(err.Error(), "GATEWAY_UPSTREAM_URL") || strings.Contains(err.Error(), "secret") {
					t.Fatalf("error must identify the setting without exposing credentials: %v", err)
				}
				if cfg != (config{}) {
					t.Fatalf("invalid URL returned partial config: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}
			if tt.value == "" {
				if cfg.upstreamURL != nil {
					t.Fatal("unset URL must remain nil")
				}
				return
			}
			if cfg.upstreamURL == nil || cfg.upstreamURL.String() != tt.value {
				t.Fatalf("upstream URL = %v, want %q", cfg.upstreamURL, tt.value)
			}
		})
	}
}

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
				readTimeout:       10 * time.Second,
				maxBodyBytes:      1 << 20,
				upstreamTimeout:   30 * time.Second,
				readHeaderTimeout: 5 * time.Second,
				idleTimeout:       60 * time.Second,
				shutdownTimeout:   10 * time.Second,
			},
		},
		{
			name: "custom environment values",
			env: map[string]string{
				"GATEWAY_ADDR":                "127.0.0.1:9090",
				"GATEWAY_READ_TIMEOUT":        "3s",
				"GATEWAY_MAX_BODY_BYTES":      "2048",
				"GATEWAY_UPSTREAM_TIMEOUT":    "20s",
				"GATEWAY_READ_HEADER_TIMEOUT": "2s",
				"GATEWAY_IDLE_TIMEOUT":        "30s",
				"GATEWAY_SHUTDOWN_TIMEOUT":    "15s",
			},
			want: config{
				addr:              "127.0.0.1:9090",
				readTimeout:       3 * time.Second,
				maxBodyBytes:      2048,
				upstreamTimeout:   20 * time.Second,
				readHeaderTimeout: 2 * time.Second,
				idleTimeout:       30 * time.Second,
				shutdownTimeout:   15 * time.Second,
			},
		},
		{
			name:      "zero read timeout",
			env:       map[string]string{"GATEWAY_READ_TIMEOUT": "0s"},
			wantError: true,
		},
		{
			name:      "invalid body limit",
			env:       map[string]string{"GATEWAY_MAX_BODY_BYTES": "bad"},
			wantError: true,
		},
		{
			name:      "zero body limit",
			env:       map[string]string{"GATEWAY_MAX_BODY_BYTES": "0"},
			wantError: true,
		},
		{
			name:      "negative body limit",
			env:       map[string]string{"GATEWAY_MAX_BODY_BYTES": "-1"},
			wantError: true,
		},
		{
			name:      "zero upstream timeout",
			env:       map[string]string{"GATEWAY_UPSTREAM_TIMEOUT": "0s"},
			wantError: true,
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
