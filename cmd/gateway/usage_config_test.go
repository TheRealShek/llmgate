package main

import (
	"strings"
	"testing"
	"time"
)

// TestUsageConfiguration checks positive writer deadlines and observation bounds.
func TestUsageConfiguration(t *testing.T) {
	for _, tt := range []struct {
		setting, value string
		invalid        bool
	}{
		{setting: "GATEWAY_USAGE_WRITE_TIMEOUT", value: "750ms"},
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
