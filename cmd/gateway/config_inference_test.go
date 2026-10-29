package main

import (
	"testing"
	"time"
)

// TestCircuitConfiguration checks defaults, overrides, and rejection of invalid circuit limits.
func TestCircuitConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failures string
		cooldown string
		want     backendCircuitConfig
		invalid  bool
	}{
		{name: "defaults", want: backendCircuitConfig{failureThreshold: 3, cooldown: 10 * time.Second}},
		{name: "overrides", failures: "5", cooldown: "250ms", want: backendCircuitConfig{failureThreshold: 5, cooldown: 250 * time.Millisecond}},
		{name: "zero failures", failures: "0", invalid: true},
		{name: "negative failures", failures: "-1", invalid: true},
		{name: "invalid failures", failures: "oops", invalid: true},
		{name: "overflow failures", failures: "9999999999999999999999", invalid: true},
		{name: "zero cooldown", cooldown: "0s", invalid: true},
		{name: "negative cooldown", cooldown: "-1s", invalid: true},
		{name: "invalid cooldown", cooldown: "oops", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadInferenceConfig(func(name string) string {
				switch name {
				case "GATEWAY_BACKEND_CIRCUIT_FAILURES":
					return tt.failures
				case "GATEWAY_BACKEND_CIRCUIT_COOLDOWN":
					return tt.cooldown
				default:
					return ""
				}
			})
			if (err != nil) != tt.invalid {
				t.Fatalf("load error = %v", err)
			}

			if !tt.invalid && cfg.circuitBreaker != tt.want {
				t.Fatalf("circuit = %+v; want %+v", cfg.circuitBreaker, tt.want)
			}
		})
	}
}
