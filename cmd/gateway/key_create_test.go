package main

import (
	"bytes"
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
