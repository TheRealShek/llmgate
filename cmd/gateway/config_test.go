package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/store"
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
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == "GATEWAY_UPSTREAM_URL" {
					return tt.value
				}

				return ""
			})
			if tt.wantError {
				if err == nil {
					t.Fatal("loadConfig() expected error, got nil")
				}

				if !strings.Contains(err.Error(), "GATEWAY_UPSTREAM_URL") ||
					strings.Contains(err.Error(), "secret") {
					t.Fatalf("error must identify the setting without exposing credentials: %v", err)
				}

				if !reflect.DeepEqual(cfg, config{}) {
					t.Fatalf("invalid URL returned partial config: %+v", cfg)
				}

				return
			}

			if err != nil {
				t.Fatalf("loadConfig() unexpected error: %v", err)
			}

			if tt.value == "" {
				if len(cfg.backends) != 0 {
					t.Fatal("unset URL must remain nil")
				}

				return
			}

			if len(cfg.backends) != 1 || cfg.backends[0].url.String() != tt.value {
				t.Fatalf("upstream URL = %v, want %q", cfg.backends, tt.value)
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
				accountingMaxAOFBytes: 256 << 20, accountingRetention: 30 * 24 * time.Hour, accountingPruneInterval: time.Hour, accountingLeaseTimeout: 30 * time.Second, accountingMaxPending: 1024, accountingBatchSize: 64, accountingFlushInterval: 250 * time.Millisecond,
				metricsKeyCapacity: 0, metricsMaxScrapes: 2, metricsScrapeTimeout: 2 * time.Second,
				adminAddr:              "127.0.0.1:8082",
				adminMaxBodyBytes:      4096,
				adminCreateTimeout:     2 * time.Second,
				adminRevokeTimeout:     2 * time.Second,
				databaseURL:            "postgres://localhost/llmgate",
				databaseMaxConns:       10,
				databaseConnectTimeout: 5 * time.Second,
				authLookupTimeout:      2 * time.Second,
				authCacheTTL:           30 * time.Second,
				authCacheCapacity:      1024,
				redisURL:               "redis://127.0.0.1:6379/0",
				redisMaxConns:          10,
				redisConnectTimeout:    2 * time.Second,
				rateLimitTimeout:       500 * time.Millisecond,
				quotaCountTimeout:      2 * time.Second,
				quotaRedisTimeout:      500 * time.Millisecond,
				quotaCountMaxBytes:     8 << 20,
				usageWriteTimeout:      2 * time.Second,
				usageMaxBytes:          64 << 10,
				addr:                   ":8080",
				readTimeout:            10 * time.Second,
				maxBodyBytes:           1 << 20, // 1 MiB
				maxTokens:              1024,
				upstreamConnectTimeout: 5 * time.Second,
				upstreamHeaderTimeout:  30 * time.Second,
				streamIdleTimeout:      30 * time.Second,
				readinessTimeout:       2 * time.Second,
				backendHealthInterval:  5 * time.Second, backendHealthFailures: 3, backendHealthSuccesses: 2,
				backendSlots: 1, backendQueueCapacity: 4, backendQueueTimeout: 5 * time.Second,
				upstreamTimeout:        30 * time.Second,
				readHeaderTimeout:      5 * time.Second,
				idleTimeout:            60 * time.Second,
				shutdownTimeout:        10 * time.Second,
				shutdownCleanupTimeout: 3 * time.Second,
			},
		},
		{
			name: "custom environment values",
			env: map[string]string{
				"GATEWAY_ADDR":                     "127.0.0.1:9090",
				"GATEWAY_READ_TIMEOUT":             "3s",
				"GATEWAY_MAX_BODY_BYTES":           "2048",
				"GATEWAY_MAX_TOKENS":               "512",
				"GATEWAY_UPSTREAM_CONNECT_TIMEOUT": "2s",
				"GATEWAY_UPSTREAM_HEADER_TIMEOUT":  "3s",
				"GATEWAY_STREAM_IDLE_TIMEOUT":      "4s",
				"GATEWAY_READINESS_TIMEOUT":        "500ms",
				"GATEWAY_UPSTREAM_TIMEOUT":         "20s",
				"GATEWAY_READ_HEADER_TIMEOUT":      "2s",
				"GATEWAY_IDLE_TIMEOUT":             "30s",
				"GATEWAY_SHUTDOWN_TIMEOUT":         "15s",
				"GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT": "4s",
			},
			want: config{
				accountingMaxAOFBytes: 256 << 20, accountingRetention: 30 * 24 * time.Hour, accountingPruneInterval: time.Hour, accountingLeaseTimeout: 30 * time.Second, accountingMaxPending: 1024, accountingBatchSize: 64, accountingFlushInterval: 250 * time.Millisecond,
				metricsKeyCapacity: 0, metricsMaxScrapes: 2, metricsScrapeTimeout: 2 * time.Second,
				adminAddr:              "127.0.0.1:8082",
				adminMaxBodyBytes:      4096,
				adminCreateTimeout:     2 * time.Second,
				adminRevokeTimeout:     2 * time.Second,
				databaseURL:            "postgres://localhost/llmgate",
				databaseMaxConns:       10,
				databaseConnectTimeout: 5 * time.Second,
				authLookupTimeout:      2 * time.Second,
				authCacheTTL:           30 * time.Second,
				authCacheCapacity:      1024,
				redisURL:               "redis://127.0.0.1:6379/0",
				redisMaxConns:          10,
				redisConnectTimeout:    2 * time.Second,
				rateLimitTimeout:       500 * time.Millisecond,
				quotaCountTimeout:      2 * time.Second,
				quotaRedisTimeout:      500 * time.Millisecond,
				quotaCountMaxBytes:     8 << 20,
				usageWriteTimeout:      2 * time.Second,
				usageMaxBytes:          64 << 10,
				addr:                   "127.0.0.1:9090",
				readTimeout:            3 * time.Second,
				maxBodyBytes:           2048,
				maxTokens:              512,
				upstreamConnectTimeout: 2 * time.Second,
				upstreamHeaderTimeout:  3 * time.Second,
				streamIdleTimeout:      4 * time.Second,
				readinessTimeout:       500 * time.Millisecond,
				backendHealthInterval:  5 * time.Second, backendHealthFailures: 3, backendHealthSuccesses: 2,
				backendSlots: 1, backendQueueCapacity: 4, backendQueueTimeout: 5 * time.Second,
				upstreamTimeout:        20 * time.Second,
				readHeaderTimeout:      2 * time.Second,
				idleTimeout:            30 * time.Second,
				shutdownTimeout:        15 * time.Second,
				shutdownCleanupTimeout: 4 * time.Second,
			},
		},
		{
			name:      "zero readiness timeout",
			env:       map[string]string{"GATEWAY_READINESS_TIMEOUT": "0s"},
			wantError: true,
		},
		{
			name:      "negative readiness timeout",
			env:       map[string]string{"GATEWAY_READINESS_TIMEOUT": "-1s"},
			wantError: true,
		},
		{
			name:      "invalid readiness timeout",
			env:       map[string]string{"GATEWAY_READINESS_TIMEOUT": "bad"},
			wantError: true,
		},
		{
			name:      "zero connect timeout",
			env:       map[string]string{"GATEWAY_UPSTREAM_CONNECT_TIMEOUT": "0s"},
			wantError: true,
		},
		{
			name:      "invalid response header timeout",
			env:       map[string]string{"GATEWAY_UPSTREAM_HEADER_TIMEOUT": "bad"},
			wantError: true,
		},
		{
			name:      "negative stream idle timeout",
			env:       map[string]string{"GATEWAY_STREAM_IDLE_TIMEOUT": "-1s"},
			wantError: true,
		},
		{
			name:      "invalid token limit",
			env:       map[string]string{"GATEWAY_MAX_TOKENS": "bad"},
			wantError: true,
		},
		{
			name:      "zero token limit",
			env:       map[string]string{"GATEWAY_MAX_TOKENS": "0"},
			wantError: true,
		},
		{
			name:      "negative token limit",
			env:       map[string]string{"GATEWAY_MAX_TOKENS": "-1"},
			wantError: true,
		},
		{
			name:      "overflowing token limit",
			env:       map[string]string{"GATEWAY_MAX_TOKENS": "9223372036854775808"},
			wantError: true,
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
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

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

			if !reflect.DeepEqual(cfg, tt.want) {
				t.Fatalf("loadConfig() = %+v, want %+v", cfg, tt.want)
			}
		})
	}
}

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
				"GATEWAY_ADMIN_REVOKE_TIMEOUT": "900ms",
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
			name:      "zero revoke timeout",
			env:       map[string]string{"GATEWAY_ADMIN_REVOKE_TIMEOUT": "0s"},
			wantError: true,
		},
		{
			name:      "negative revoke timeout",
			env:       map[string]string{"GATEWAY_ADMIN_REVOKE_TIMEOUT": "-1s"},
			wantError: true,
		},
		{
			name:      "invalid revoke timeout",
			env:       map[string]string{"GATEWAY_ADMIN_REVOKE_TIMEOUT": "private-secret"},
			wantError: true,
		},
		{
			name: "revoke write timeout overflow",
			env: map[string]string{
				"GATEWAY_ADMIN_ENABLED":        "true",
				"GATEWAY_ADMIN_TOKEN":          strings.Repeat("a", 64),
				"GATEWAY_ADMIN_REVOKE_TIMEOUT": "2562047h47m16s",
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

			if tt.name == "positive resource overrides" && (cfg.adminMaxBodyBytes != 2048 || cfg.adminCreateTimeout.String() != "750ms" || cfg.adminRevokeTimeout.String() != "900ms") {
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
		{name: "creation timeout", key: "GATEWAY_ADMIN_CREATE_TIMEOUT", value: "private-secret"},
		{name: "revocation timeout", key: "GATEWAY_ADMIN_REVOKE_TIMEOUT", value: "private-secret"},
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

// TestAuthCacheConfiguration checks cache overrides and rejects invalid bounds without leaking values.
func TestAuthCacheConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, setting, value string
		invalid              bool
	}{
		{name: "custom TTL", setting: "GATEWAY_AUTH_CACHE_TTL", value: "45s"},
		{name: "custom capacity", setting: "GATEWAY_AUTH_CACHE_CAPACITY", value: "16"},
		{name: "zero TTL", setting: "GATEWAY_AUTH_CACHE_TTL", value: "0s", invalid: true},
		{name: "negative TTL", setting: "GATEWAY_AUTH_CACHE_TTL", value: "-1s", invalid: true},
		{name: "invalid TTL", setting: "GATEWAY_AUTH_CACHE_TTL", value: "secret", invalid: true},
		{name: "overflow TTL", setting: "GATEWAY_AUTH_CACHE_TTL", value: "999999999999h", invalid: true},
		{name: "zero capacity", setting: "GATEWAY_AUTH_CACHE_CAPACITY", value: "0", invalid: true},
		{name: "negative capacity", setting: "GATEWAY_AUTH_CACHE_CAPACITY", value: "-1", invalid: true},
		{name: "invalid capacity", setting: "GATEWAY_AUTH_CACHE_CAPACITY", value: "secret", invalid: true},
		{name: "overflow capacity", setting: "GATEWAY_AUTH_CACHE_CAPACITY", value: "99999999999999999999", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == tt.setting {
					return tt.value
				}

				return ""
			})
			if tt.invalid {
				if err == nil || !strings.Contains(err.Error(), tt.setting) ||
					strings.Contains(err.Error(), "secret") {
					t.Fatalf("expected safe configuration error for %s, got %v", tt.setting, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if tt.setting == "GATEWAY_AUTH_CACHE_TTL" && cfg.authCacheTTL != 45*time.Second {
				t.Fatal("cache TTL override was ignored")
			}

			if tt.setting == "GATEWAY_AUTH_CACHE_CAPACITY" && cfg.authCacheCapacity != 16 {
				t.Fatal("cache capacity override was ignored")
			}
		})
	}
}

// TestRedisLimitsConfiguration checks explicit Redis connection and decision bounds.
func TestRedisLimitsConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, setting, value string
		invalid              bool
	}{
		{name: "connection URL", setting: "GATEWAY_REDIS_URL", value: "redis://127.0.0.1:6380/1"},
		{name: "pool size", setting: "GATEWAY_REDIS_MAX_CONNS", value: "3"},
		{name: "connect timeout", setting: "GATEWAY_REDIS_CONNECT_TIMEOUT", value: "3s"},
		{name: "decision timeout", setting: "GATEWAY_RATE_LIMIT_TIMEOUT", value: "250ms"},
		{name: "zero pool", setting: "GATEWAY_REDIS_MAX_CONNS", value: "0", invalid: true},
		{name: "negative pool", setting: "GATEWAY_REDIS_MAX_CONNS", value: "-1", invalid: true},
		{name: "invalid pool", setting: "GATEWAY_REDIS_MAX_CONNS", value: "private-secret", invalid: true},
		{name: "overflow pool", setting: "GATEWAY_REDIS_MAX_CONNS", value: "99999999999999999999", invalid: true},
		{name: "zero connect timeout", setting: "GATEWAY_REDIS_CONNECT_TIMEOUT", value: "0s", invalid: true},
		{name: "negative decision timeout", setting: "GATEWAY_RATE_LIMIT_TIMEOUT", value: "-1s", invalid: true},
		{name: "invalid decision timeout", setting: "GATEWAY_RATE_LIMIT_TIMEOUT", value: "private-secret", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == tt.setting {
					return tt.value
				}

				return ""
			})
			if tt.invalid {
				if err == nil || !strings.Contains(err.Error(), tt.setting) || strings.Contains(err.Error(), "private-secret") {
					t.Fatal("unsafe or missing configuration error", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			switch tt.setting {
			case "GATEWAY_REDIS_URL":
				if cfg.redisURL != tt.value {
					t.Fatal("URL override ignored")
				}
			case "GATEWAY_REDIS_MAX_CONNS":
				if cfg.redisMaxConns != 3 {
					t.Fatal("pool override ignored")
				}
			case "GATEWAY_REDIS_CONNECT_TIMEOUT":
				if cfg.redisConnectTimeout != 3*time.Second {
					t.Fatal("connection deadline override ignored")
				}
			case "GATEWAY_RATE_LIMIT_TIMEOUT":
				if cfg.rateLimitTimeout != 250*time.Millisecond {
					t.Fatal("decision deadline override ignored")
				}
			}
		})
	}
}

// TestUsageConfiguration checks positive writer deadlines and observation bounds.
func TestUsageConfiguration(t *testing.T) {
	for _, tt := range []struct {
		setting, value string
		invalid        bool
	}{
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "750ms"},
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "1ns", invalid: true},
		{setting: "GATEWAY_USAGE_MAX_BYTES", value: "4096"},
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "0s", invalid: true},
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "-1s", invalid: true},
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "private-value", invalid: true},
		{setting: "GATEWAY_USAGE_MAX_BYTES", value: "0", invalid: true},
		{setting: "GATEWAY_USAGE_MAX_BYTES", value: "-1", invalid: true},
		{setting: "GATEWAY_USAGE_MAX_BYTES", value: "private-value", invalid: true},
		{setting: "GATEWAY_USAGE_MAX_BYTES", value: "999999999999999999999", invalid: true},
	} {
		t.Run(tt.setting+"/"+tt.value, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == tt.setting {
					return tt.value
				}

				return ""
			})
			if tt.invalid {
				if err == nil || !strings.Contains(err.Error(), tt.setting) || strings.Contains(err.Error(), "private-value") {
					t.Fatal("invalid usage bounds were accepted or exposed", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if tt.setting == "GATEWAY_USAGE_WRITE_TIMEOUT" && cfg.usageWriteTimeout != 750*time.Millisecond {
				t.Fatal("usage deadline override ignored")
			}

			if tt.setting == "GATEWAY_USAGE_MAX_BYTES" && cfg.usageMaxBytes != 4096 {
				t.Fatal("usage observation bound override ignored")
			}
		})
	}
}

// TestDatabaseConfiguration checks database settings are required, bounded, and do not leak secrets.
func TestDatabaseConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
	}{
		{name: "missing database URL"},
		{
			name: "zero connections",
			env: map[string]string{
				"GATEWAY_DATABASE_URL":       "postgres://localhost/db",
				"GATEWAY_DATABASE_MAX_CONNS": "0",
			},
		},
		{
			name: "connection count overflow",
			env: map[string]string{
				"GATEWAY_DATABASE_URL":       "postgres://localhost/db",
				"GATEWAY_DATABASE_MAX_CONNS": "2147483648",
			},
		},
		{
			name: "zero connect timeout",
			env: map[string]string{
				"GATEWAY_DATABASE_URL":             "postgres://localhost/db",
				"GATEWAY_DATABASE_CONNECT_TIMEOUT": "0s",
			},
		},
		{
			name: "negative lookup timeout",
			env: map[string]string{
				"GATEWAY_DATABASE_URL":        "postgres://localhost/db",
				"GATEWAY_AUTH_LOOKUP_TIMEOUT": "-1s",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadConfig(func(k string) string { return tt.env[k] }); err == nil {
				t.Fatal("invalid database settings accepted")
			}
		})
	}

	t.Run("positive overrides", func(t *testing.T) {
		cfg, err := loadConfig(func(k string) string {
			return map[string]string{
				"GATEWAY_DATABASE_URL":             "postgres://localhost/db",
				"GATEWAY_DATABASE_MAX_CONNS":       "3",
				"GATEWAY_DATABASE_CONNECT_TIMEOUT": "250ms",
				"GATEWAY_AUTH_LOOKUP_TIMEOUT":      "500ms",
			}[k]
		})
		if err != nil || cfg.databaseMaxConns != 3 || cfg.authLookupTimeout.String() != "500ms" || cfg.databaseConnectTimeout.String() != "250ms" {
			t.Fatalf("database overrides were lost: error %v", err)
		}
	})

	t.Run("canceled creation hides database credentials", func(t *testing.T) {
		var output bytes.Buffer

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := createKey(ctx, []string{"-name", "test"}, &output, func(k string) string {
			if k == "GATEWAY_DATABASE_URL" {
				return "postgres://user:private-secret@127.0.0.1:1/db"
			}

			return ""
		})
		if !errors.Is(err, store.ErrDatabaseUnavailable) || output.Len() != 0 || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("database failure did not remain safe: %v", err)
		}
	})
}

// TestQuotaConfiguration checks zero-default grace and positive operation bounds.
func TestQuotaConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		valid       bool
	}{
		{name: "default", valid: true}, {name: "zero", value: "0", valid: true},
		{name: "ten percent", value: "10", valid: true}, {name: "full grace", value: "100", valid: true},
		{name: "negative", value: "-1"}, {name: "too large", value: "101"}, {name: "fraction", value: "1.5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == "GATEWAY_QUOTA_GRACE_PERCENT" {
					return tt.value
				}

				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatal(err)
			}

			if tt.name == "default" && cfg.quotaGracePercent != 0 {
				t.Fatal("grace must default to zero")
			}
		})
	}

	for _, name := range []string{"GATEWAY_QUOTA_COUNT_TIMEOUT", "GATEWAY_QUOTA_REDIS_TIMEOUT", "GATEWAY_QUOTA_COUNT_MAX_BYTES"} {
		for _, value := range []string{"0", "-1", "invalid"} {
			if _, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				if key == name {
					return value
				}

				return ""
			}); err == nil {
				t.Fatal("invalid quota bound accepted", name, value)
			}
		}
	}
}

// TestMetricsConfiguration checks finite scrape and key-label resource bounds.
func TestMetricsConfiguration(t *testing.T) {
	for _, name := range []string{"GATEWAY_METRICS_KEY_CAPACITY", "GATEWAY_METRICS_MAX_SCRAPES", "GATEWAY_METRICS_SCRAPE_TIMEOUT"} {
		for _, value := range []string{"0", "-1", "invalid"} {
			if name == "GATEWAY_METRICS_KEY_CAPACITY" && value == "0" {
				continue
			}

			if _, err := loadConfig(func(key string) string {
				if key == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/db"
				}

				if key == name {
					return value
				}

				return ""
			}); err == nil {
				t.Fatal("invalid metrics limit accepted", name, value)
			}
		}
	}

	for _, value := range []string{"", "0"} {
		cfg, err := loadConfig(func(key string) string {
			return map[string]string{
				"GATEWAY_DATABASE_URL":         "postgres://localhost/db",
				"GATEWAY_METRICS_KEY_CAPACITY": value,
			}[key]
		})
		if err != nil || cfg.metricsKeyCapacity != 0 {
			t.Fatalf("zero capacity rejected: value=%q capacity=%d err=%v", value, cfg.metricsKeyCapacity, err)
		}
	}

	cfg, err := loadConfig(func(key string) string {
		return map[string]string{
			"GATEWAY_DATABASE_URL": "postgres://localhost/db", "GATEWAY_METRICS_KEY_CAPACITY": "10", "GATEWAY_METRICS_MAX_SCRAPES": "3", "GATEWAY_METRICS_SCRAPE_TIMEOUT": "1s",
		}[key]
	})
	if err != nil || cfg.metricsKeyCapacity != 10 || cfg.metricsMaxScrapes != 3 || cfg.metricsScrapeTimeout != time.Second {
		t.Fatal("metrics overrides lost", err)
	}
}

// TestShutdownCleanupConfiguration checks the cleanup budget rejects invalid or unbounded values.
func TestShutdownCleanupConfiguration(t *testing.T) {
	for _, value := range []string{"invalid", "0s", "-1s", "999999999999h"} {
		_, err := loadConfig(func(key string) string {
			if key == "GATEWAY_DATABASE_URL" {
				return "postgres://localhost/llmgate"
			}

			if key == "GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT" {
				return value
			}

			return ""
		})
		if err == nil || !strings.Contains(err.Error(), "GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT") {
			t.Fatal("invalid cleanup timeout accepted or misidentified", value, err)
		}
	}
}

// TestAccountingConfig checks shared capacity, batch bounds, and delivery interval validation.
func TestAccountingConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		values  map[string]string
		invalid bool
	}{
		{name: "custom", values: map[string]string{"GATEWAY_ACCOUNTING_MAX_PENDING": "128", "GATEWAY_ACCOUNTING_BATCH_SIZE": "32", "GATEWAY_ACCOUNTING_FLUSH_INTERVAL": "100ms", "GATEWAY_ACCOUNTING_MAX_AOF_BYTES": "536870912", "GATEWAY_ACCOUNTING_RETENTION": "48h", "GATEWAY_ACCOUNTING_PRUNE_INTERVAL": "1m", "GATEWAY_ACCOUNTING_LEASE_TIMEOUT": "10s"}},
		{name: "zero storage budget", values: map[string]string{"GATEWAY_ACCOUNTING_MAX_AOF_BYTES": "0"}, invalid: true},
		{name: "zero retention", values: map[string]string{"GATEWAY_ACCOUNTING_RETENTION": "0s"}, invalid: true},
		{name: "zero prune interval", values: map[string]string{"GATEWAY_ACCOUNTING_PRUNE_INTERVAL": "0s"}, invalid: true},
		{name: "unsafe lease", values: map[string]string{"GATEWAY_ACCOUNTING_LEASE_TIMEOUT": "2s"}, invalid: true},
		{name: "zero capacity", values: map[string]string{"GATEWAY_ACCOUNTING_MAX_PENDING": "0"}, invalid: true},
		{name: "negative batch", values: map[string]string{"GATEWAY_ACCOUNTING_BATCH_SIZE": "-1"}, invalid: true},
		{name: "batch exceeds capacity", values: map[string]string{"GATEWAY_ACCOUNTING_MAX_PENDING": "4"}, invalid: true},
		{name: "zero interval", values: map[string]string{"GATEWAY_ACCOUNTING_FLUSH_INTERVAL": "0s"}, invalid: true},
		{name: "invalid integer", values: map[string]string{"GATEWAY_ACCOUNTING_BATCH_SIZE": "private-value"}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(name string) string {
				if name == "GATEWAY_DATABASE_URL" {
					return "postgres://localhost/llmgate"
				}

				return tt.values[name]
			})
			if (err != nil) != tt.invalid {
				t.Fatal("unexpected accounting configuration validation", err)
			}

			if !tt.invalid && (cfg.accountingMaxPending != 128 || cfg.accountingBatchSize != 32 || cfg.accountingFlushInterval != 100*time.Millisecond || cfg.accountingMaxAOFBytes != 536870912 || cfg.accountingRetention != 48*time.Hour || cfg.accountingPruneInterval != time.Minute || cfg.accountingLeaseTimeout != 10*time.Second) {
				t.Fatal("custom accounting settings ignored")
			}
		})
	}
}

// TestBackendHealthConfig checks custom health settings and rejects invalid bounds.
func TestBackendHealthConfig(t *testing.T) {
	env := map[string]string{
		"GATEWAY_DATABASE_URL":             "postgres://localhost/llmgate",
		"GATEWAY_BACKEND_HEALTH_INTERVAL":  "250ms",
		"GATEWAY_BACKEND_HEALTH_FAILURES":  "4",
		"GATEWAY_BACKEND_HEALTH_SUCCESSES": "3",
	}

	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}

	if cfg.backendHealthInterval != 250*time.Millisecond || cfg.backendHealthFailures != 4 || cfg.backendHealthSuccesses != 3 {
		t.Fatalf("unexpected health configuration: %v, %d, %d", cfg.backendHealthInterval, cfg.backendHealthFailures, cfg.backendHealthSuccesses)
	}

	for _, key := range []string{
		"GATEWAY_BACKEND_HEALTH_INTERVAL", "GATEWAY_BACKEND_HEALTH_FAILURES", "GATEWAY_BACKEND_HEALTH_SUCCESSES",
	} {
		for _, value := range []string{"0", "-1", "invalid", "99999999999999999999999999999"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				_, err := loadConfig(func(name string) string {
					if name == "GATEWAY_DATABASE_URL" {
						return "postgres://localhost/llmgate"
					}

					if name == key {
						return value
					}

					return ""
				})
				if err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("invalid health setting error = %v", err)
				}
			})
		}
	}
}

// TestInferenceQueueConfig checks custom and zero-waiting capacity, and rejects invalid limits.
func TestInferenceQueueConfig(t *testing.T) {
	for _, capacity := range []string{"0", "8"} {
		cfg, err := loadConfig(func(name string) string {
			switch name {
			case "GATEWAY_DATABASE_URL":
				return "postgres://localhost/llmgate"
			case "GATEWAY_BACKEND_SLOTS":
				return "3"
			case "GATEWAY_BACKEND_QUEUE_CAPACITY":
				return capacity
			case "GATEWAY_BACKEND_QUEUE_TIMEOUT":
				return "250ms"
			default:
				return ""
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		if cfg.backendSlots != 3 || strconv.Itoa(cfg.backendQueueCapacity) != capacity || cfg.backendQueueTimeout != 250*time.Millisecond {
			t.Fatal("queue overrides were not applied")
		}
	}

	for _, name := range []string{"GATEWAY_BACKEND_SLOTS", "GATEWAY_BACKEND_QUEUE_CAPACITY", "GATEWAY_BACKEND_QUEUE_TIMEOUT"} {
		values := []string{"-1", "invalid", "99999999999999999999999999999"}
		if name != "GATEWAY_BACKEND_QUEUE_CAPACITY" {
			values = append(values, "0")
		}

		for _, value := range values {
			t.Run(name+"/"+value, func(t *testing.T) {
				_, err := loadConfig(func(key string) string {
					if key == "GATEWAY_DATABASE_URL" {
						return "postgres://localhost/llmgate"
					}

					if key == name {
						return value
					}

					return ""
				})
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("invalid queue configuration error = %v", err)
				}
			})
		}
	}
}
