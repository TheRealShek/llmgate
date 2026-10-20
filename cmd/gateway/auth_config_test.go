package main

import (
	"strings"
	"testing"
	"time"
)

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
