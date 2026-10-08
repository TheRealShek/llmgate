package proxy

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/therealshek/llmgate/internal/api"
)

// NewHandler forwards non-streaming requests to target, a validated, non-nil backend URL.
// transport sends upstream HTTP requests and is owned by the caller.
// timeout bounds the entire upstream request, including reading the response body.
func NewHandler(target *url.URL, transport http.RoundTripper, timeout time.Duration) http.Handler {
	// Copy the URL value so caller mutations do not change backend.
	backend := *target

	// ReverseProxy rewrites the request, sends it over transport with RoundTrip,
	// and copies the response back to the client.
	forwarder := &httputil.ReverseProxy{
		// Rewrite points the outbound URL to the backend while preserving the original
		// path, query parameters, and headers.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&backend)
		},
		Transport:    transport,
		ErrorLog:     log.New(proxyLogWriter{}, "", 0),
		ErrorHandler: handleError,
	}

	// Wrap forwarder so each request receives an upstream timeout context before ServeHTTP runs.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		forwarder.ServeHTTP(w, r.WithContext(ctx))
	})
}

// handleError maps transport and context failures into OpenAI-compatible HTTP error responses.
func handleError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // Client disconnected early. Nothing to deliver.
	}
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	// Transport errors can contain raw URLs or credentials. Log only the status.
	slog.Warn("upstream request failed", "status", status)
	_ = api.WriteError(w, status, api.Error{
		Message: "upstream request failed",
		Type:    "server_error",
		Code:    "upstream_error",
	})
}

type proxyLogWriter struct{}

// Write intercepts ReverseProxy internal error logs to drop raw messages that may
// contain internal URLs or tokens.
func (proxyLogWriter) Write(p []byte) (int, error) {
	slog.Warn("upstream response failed")
	return len(p), nil
}
