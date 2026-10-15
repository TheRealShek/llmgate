package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestOpenPoolFailures checks configuration, cancellation, and safe diagnostics preserve causes.
func TestOpenPoolFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for _, tt := range []struct {
		name   string
		url    string
		limits PoolLimits
		want   error
	}{
		{"invalid URL", "postgres://user:private-secret%zz@localhost/db", PoolLimits{1, time.Second}, ErrInvalidDatabaseConfig},
		{"invalid limits", "postgres://localhost/db", PoolLimits{}, ErrInvalidDatabaseConfig},
		{"canceled startup", "postgres://user:private-secret@127.0.0.1:1/db", PoolLimits{1, time.Second}, ErrDatabaseUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := OpenPool(ctx, tt.url, tt.limits)
			if pool != nil || !errors.Is(err, tt.want) {
				t.Fatalf("pool/error = %v/%v", pool, err)
			}

			if strings.Contains(err.Error(), "private-secret") {
				t.Fatal("pool error exposed credentials")
			}

			if tt.name == "canceled startup" && !errors.Is(err, context.Canceled) {
				t.Fatal("pool lost cancellation cause")
			}
		})
	}
}
