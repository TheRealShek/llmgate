// Package requestlog attaches request identity to HTTP responses and structured logs.
package requestlog

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"
)

type contextKey struct{}

type requestInfo struct {
	id      string
	logger  *slog.Logger
	started time.Time
}

// NewHandler gives each incoming request a generated ID and a logger derived from logger.
// It passes the original response writer to next, preserving streaming and flushing.
func NewHandler(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		info := requestInfo{id: rand.Text(), started: started}
		// derived logger keeps that ID attached without modifying the shared logger.
		info.logger = logger.With("request_id", info.id)
		w.Header().Set("X-Request-ID", info.id)

		// The client request gains logging metadata before entering the gateway routes.
		// WithContext preserves client cancellation; this wrapper creates no deadline.
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, info))
		info.logger.InfoContext(r.Context(), "request started", "method", r.Method)

		completed := false
		defer func() {
			info.logger.InfoContext(r.Context(), "request finished",
				"route", r.Pattern,
				"duration_ms", float64(time.Since(started))/float64(time.Millisecond),
				"completed", completed)
		}()

		next.ServeHTTP(w, r)

		completed = true
	})
}

// Logger returns the request logger, or the default logger outside the wrapper.
func Logger(ctx context.Context) *slog.Logger {
	if info, ok := ctx.Value(contextKey{}).(requestInfo); ok {
		return info.logger
	}

	return slog.Default()
}

// ID returns the gateway-generated request ID, or an empty string outside the wrapper.
func ID(ctx context.Context) string {
	info, _ := ctx.Value(contextKey{}).(requestInfo)
	return info.id
}

// Started returns arrival time from the outer logger, or zero outside its wrapper.
func Started(ctx context.Context) time.Time {
	info, _ := ctx.Value(contextKey{}).(requestInfo)
	return info.started
}
