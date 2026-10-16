package main

import (
	"errors"
	"strings"
	"testing"
)

// TestAdminConfiguration checks opt-in, loopback-only binding, safe credentials, and positive limits.
func TestAdminConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name      string
		env       map[string]string
		wantError bool
	}{
		{name: "disabled by default"},
		{
			name: "token alone does not enable",
			env: map[string]string{
				"GATEWAY_ADMIN_TOKEN": strings.Repeat("a", 64),
			},
		},
		{
			name: "default loopback",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
			},
		},
		{
			name: "IPv6 loopback",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "[::1]:8082",
			},
		},
		{
			name: "positive resource overrides",
			env: map[string]string{
				"GATEWAY_ADMIN_MAX_BODY_BYTES": "2048",
				"GATEWAY_ADMIN_CREATE_TIMEOUT": "750ms",
			},
		},
		{
			name: "write timeout overflow",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_READ_TIMEOUT":  "2562047h47m16.854775807s",
			},
			wantError: true,
		},
		{
			name: "overlong token",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 257),
			},
			wantError: true,
		},
		{
			name: "missing token",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
			},
			wantError: true,
		},
		{
			name: "short token",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   "private-secret",
			},
			wantError: true,
		},
		{
			name: "invalid characters",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64) + "\nprivate-secret",
			},
			wantError: true,
		},
		{
			name: "invalid enable flag",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "private-secret",
			},
			wantError: true,
		},
		{
			name: "wildcard",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "0.0.0.0:8082",
			},
			wantError: true,
		},
		{
			name: "empty host",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    ":8082",
			},
			wantError: true,
		},
		{
			name: "hostname",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "localhost:8082",
			},
			wantError: true,
		},
		{
			name: "non-loopback",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "192.0.2.1:8082",
			},
			wantError: true,
		},
		{
			name: "zero port",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "127.0.0.1:0",
			},
			wantError: true,
		},
		{
			name: "service port",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED": "true",
				"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
				"GATEWAY_ADMIN_ADDR":    "127.0.0.1:http",
			},
			wantError: true,
		},
		{
			name: "zero body limit",
			env: map[string]string{
				"GATEWAY_ADMIN_MAX_BODY_BYTES": "0",
			},
			wantError: true,
		},
		{
			name: "zero create timeout",
			env: map[string]string{
				"GATEWAY_ADMIN_CREATE_TIMEOUT": "0s",
			},
			wantError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(k string) string {
				if k == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/db"
				}

				return tt.env[k]
			})
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, want error %v", err, tt.wantError)
			}

			if err != nil {
				if strings.Contains(err.Error(), "private-secret") {
					t.Fatal("configuration error exposed input")
				}

				return
			}

			if cfg.adminEnabled != (tt.env["GATEWAY_ADMIN_ENABLED"] == "true") {
				t.Fatal("admin enablement changed")
			}

			if tt.name == "positive resource overrides" && (cfg.adminMaxBodyBytes != 2048 || cfg.adminCreateTimeout.String() != "750ms") {
				t.Fatal("admin resource overrides were lost")
			}

			if !cfg.adminEnabled && cfg.adminToken != "" {
				t.Fatal("disabled administration retained token")
			}
		})
	}
}

// TestAdminParsingCauses checks parsing errors preserve causes without exposing configuration input.
func TestAdminParsingCauses(t *testing.T) {
	for _, tt := range []struct{ name, key, value string }{
		{name: "boolean", key: "GATEWAY_ADMIN_ENABLED", value: "private-secret"},
		{name: "address format", key: "GATEWAY_ADMIN_ADDR", value: "private-secret"},
		{name: "IP address", key: "GATEWAY_ADMIN_ADDR", value: "private-secret:8082"},
		{name: "port", key: "GATEWAY_ADMIN_ADDR", value: "127.0.0.1:private-secret"},
		{name: "body size", key: "GATEWAY_ADMIN_MAX_BODY_BYTES", value: "private-secret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(func(k string) string {
				if k == tt.key {
					return tt.value
				}

				return map[string]string{
					"GATEWAY_ADMIN_ENABLED": "true",
					"GATEWAY_ADMIN_TOKEN":   strings.Repeat("a", 64),
					"GATEWAY_DATABASE_URL":  "postgres://localhost/db",
				}[k]
			})

			var safeError *configError
			if !errors.As(err, &safeError) || errors.Unwrap(safeError) == nil {
				t.Fatal("configuration parsing lost its private cause")
			}

			if strings.Contains(err.Error(), "private-secret") {
				t.Fatal("configuration parsing exposed supplied input")
			}
		})
	}
}
