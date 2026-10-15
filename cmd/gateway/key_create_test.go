package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/therealshek/llmgate/internal/store"
)

// TestCreateKeyOptions checks invalid options and help require no database access or secret output.
func TestCreateKeyOptions(t *testing.T) {
	for _, tt := range []struct {
		name               string
		args               []string
		wantInvalidOptions bool
		wantParsingCause   bool
	}{
		{name: "missing name", wantInvalidOptions: true},
		{name: "blank name", args: []string{"-name", " "}, wantInvalidOptions: true},
		{
			name:               "name too long",
			args:               []string{"-name", strings.Repeat("x", 129)},
			wantInvalidOptions: true,
		},
		{
			name:               "zero request limit",
			args:               []string{"-name", "test", "-requests-per-min", "0"},
			wantInvalidOptions: true,
		},
		{
			name:               "negative token limit",
			args:               []string{"-name", "test", "-tokens-per-day", "-1"},
			wantInvalidOptions: true,
		},
		{
			name:             "unknown flag",
			args:             []string{"-unknown", "private-secret"},
			wantParsingCause: true,
		},
		{
			name:             "invalid numeric flag",
			args:             []string{"-name", "test", "-requests-per-min", "private-secret"},
			wantParsingCause: true,
		},
		{name: "unexpected argument", args: []string{"-name", "test", "extra"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer

			err := createKey(t.Context(), tt.args, &output, func(string) string {
				t.Error("invalid options read database configuration")
				return ""
			})
			if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("invalid options produced output or unsafe error: %v", err)
			}

			if tt.wantInvalidOptions && !errors.Is(err, store.ErrInvalidKeyOptions) {
				t.Fatal("invalid options lost their sentinel error")
			}

			if tt.wantParsingCause {
				var safeError *keyCreateError
				if !errors.As(err, &safeError) || errors.Unwrap(safeError) == nil {
					t.Fatal("flag parsing lost its private failure cause")
				}

				if !errors.Is(err, errors.Unwrap(safeError)) {
					t.Fatal("flag parsing cause is not reachable through the error chain")
				}
			}
		})
	}

	t.Run("help without database", func(t *testing.T) {
		var output bytes.Buffer

		err := createKey(t.Context(), []string{"-h"}, &output, func(string) string {
			t.Error("help read config")
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(output.String(), "Usage:") {
			t.Fatal("help did not explain flags")
		}
	})
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
