package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestRunCommandDispatch checks run selects startup or key creation and keeps diagnostics private.
func TestRunCommandDispatch(t *testing.T) {
	for _, tt := range []struct {
		name             string
		args             []string
		wantError        bool
		wantHelp         bool
		wantParsingCause bool
	}{
		{name: "startup requires database configuration", wantError: true},
		{name: "key creation help", args: []string{"key-create", "-h"}, wantHelp: true},
		{
			name:             "key creation invalid flag",
			args:             []string{"key-create", "-requests-per-min", "private-secret"},
			wantError:        true,
			wantParsingCause: true,
		},
		{name: "unknown command", args: []string{"private-secret"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GATEWAY_DATABASE_URL", "")

			output, err := os.CreateTemp(t.TempDir(), "command-output")
			if err != nil {
				t.Fatal(err)
			}

			originalArgs, originalOutput := os.Args, os.Stdout

			t.Cleanup(func() {
				os.Args, os.Stdout = originalArgs, originalOutput

				if err := output.Close(); err != nil {
					t.Error(err)
				}
			})

			os.Args = append([]string{"gateway"}, tt.args...)
			os.Stdout = output

			err = run(t.Context())
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, want error %v", err, tt.wantError)
			}

			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("command diagnostics exposed input")
			}

			if tt.wantParsingCause {
				var safeError *keyCreateError
				if !errors.As(err, &safeError) || errors.Unwrap(safeError) == nil {
					t.Fatal("run did not preserve the key-create parsing cause")
				}
			}

			body, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}

			if tt.wantHelp {
				if !strings.Contains(string(body), "Usage: gateway key-create") {
					t.Fatal("run did not dispatch key-create help")
				}
			} else if len(body) != 0 {
				t.Fatal("failed command produced success output")
			}
		})
	}
}
