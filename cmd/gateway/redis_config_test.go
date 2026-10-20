package main

import (
	"strings"
	"testing"
	"time"
)

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
