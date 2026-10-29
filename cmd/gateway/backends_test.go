package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestLoadBackends checks named targets, shared defaults, explicit zero waiting, and legacy conversion.
func TestLoadBackends(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		want int
	}{
		{name: "unset"},
		{name: "legacy", env: map[string]string{"GATEWAY_UPSTREAM_URL": "http://localhost:8081/api"}, want: 1},
		{name: "named", env: map[string]string{"GATEWAY_BACKENDS": `[{"name":"a","url":"http://localhost:8081"},{"name":"b","url":"https://example.com/api","slots":3,"queue_capacity":0}]`}, want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			targets, err := loadBackends(func(name string) string { return tt.env[name] }, 2, 4)
			if err != nil || len(targets) != tt.want {
				t.Fatal(targets, err)
			}

			if len(targets) == 0 {
				return
			}

			if targets[0].slots != 2 || targets[0].queueCapacity != 4 {
				t.Fatal("lost defaults", targets[0])
			}

			if tt.name == "legacy" && (targets[0].name != "primary" || targets[0].url.Path != "/api") {
				t.Fatal("legacy conversion", targets[0])
			}

			if tt.name == "named" && (targets[1].name != "b" || targets[1].slots != 3 || targets[1].queueCapacity != 0) {
				t.Fatal("per-backend settings", targets[1])
			}
		})
	}
}

// TestLoadBackendsInvalid checks bounded strict configuration and secret-free failures.
func TestLoadBackendsInvalid(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, raw := range []string{
		`null`, `[]`, `{}`, `[{"name":"a","url":"http://a"}] {}`, `[{"name":"a","url":"http://a","typo":1}]`,
		`[{"name":"a","url":"http://a"},{"name":"a","url":"http://b"}]`,
		`[{"name":"","url":"http://a"}]`, `[{"name":"none","url":"http://a"}]`, `[{"name":"bad/name","url":"http://a"}]`,
		`[{"name":"a","url":"http://user:private-secret@host"}]`, `[{"name":"a","url":"http://host?private-secret"}]`,
		`[{"name":"a","url":"http://host/#private-secret"}]`, `[{"name":"a","url":"http://host:99999"}]`,
		`[{"name":"a","url":"file:///private-secret"}]`, `[{"name":"a","url":"http://host","slots":0}]`,
		`[{"name":"a","url":"http://host","queue_capacity":-1}]`,
		fmt.Sprintf(`[{"name":"a","url":"http://host","slots":%d,"queue_capacity":1}]`, maxInt),
		strings.Repeat(" ", 64<<10+1),
		`[{"name":"` + strings.Repeat("a", 65) + `","url":"http://a"}]`,
		`[` + strings.TrimSuffix(strings.Repeat(`{"name":"a","url":"http://a"},`, 17), ",") + `]`,
	} {
		if _, err := loadBackends(func(name string) string {
			if name == "GATEWAY_BACKENDS" {
				return raw
			}

			return ""
		}, 1, 4); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unsafe or missing validation: %v", err)
		}
	}

	if _, err := loadBackends(func(string) string { return "configured" }, 1, 4); err == nil {
		t.Fatal("ambiguous sources accepted")
	}
}

// TestBackendConfigIntegration checks global defaults are loaded before per-backend overrides.
func TestBackendConfigIntegration(t *testing.T) {
	env := map[string]string{
		"GATEWAY_DATABASE_URL":  "postgres://localhost/test",
		"GATEWAY_BACKEND_SLOTS": "2", "GATEWAY_BACKEND_QUEUE_CAPACITY": "7",
		"GATEWAY_BACKENDS": `[{"name":"a","url":"http://a"},{"name":"b","url":"http://b","slots":4}]`,
	}

	cfg, err := loadConfig(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.inference.targets) != 2 || cfg.inference.targets[0].slots != 2 || cfg.inference.targets[1].slots != 4 || cfg.inference.targets[1].queueCapacity != 7 {
		t.Fatal(cfg.inference.targets)
	}
}
