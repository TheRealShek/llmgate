package proxy

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/requestlog"
)

func newForwarder(target *url.URL, transport http.RoundTripper) *httputil.ReverseProxy {
	// Copy the URL value so caller mutations do not change backend.
	backend := *target

	// ReverseProxy rewrites the request, sends it over transport with RoundTrip,
	// and copies the response back to the client.
	return &httputil.ReverseProxy{
		// Rewrite points the outbound URL to the backend and replaces caller identity
		// with the gateway ID while preserving the path, query, and other headers.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&backend)
			if id := requestlog.ID(r.In.Context()); id != "" {
				r.Out.Header.Set("X-Request-ID", id)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			if requestlog.ID(response.Request.Context()) != "" {
				// The logging wrapper already set the client ID. ReverseProxy adds
				// backend headers, so remove its ID to avoid a duplicate or spoofed value.
				response.Header.Del("X-Request-ID")
			}
			return nil
		},
		Transport:    transport,
		ErrorLog:     log.New(proxyLogWriter{logger: slog.Default()}, "", 0),
		ErrorHandler: handleUpstreamError,
	}
}

// handleUpstreamError maps transport and context failures into OpenAI-compatible HTTP error responses.
func handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // Client disconnected early. Nothing to deliver.
	}
	status := http.StatusBadGateway
	var timeoutError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeoutError) && timeoutError.Timeout()) {
		status = http.StatusGatewayTimeout
	}
	// Transport errors can contain raw URLs or credentials. Log only the status.
	requestlog.Logger(r.Context()).WarnContext(r.Context(), "upstream request failed", "status", status)
	_ = api.WriteError(w, status, api.Error{
		Message: "upstream request failed",
		Type:    "server_error",
		Code:    "upstream_error",
	})
}

type proxyLogWriter struct {
	logger *slog.Logger
}

// Write intercepts ReverseProxy internal error logs to drop raw messages that may
// contain internal URLs or tokens.
func (w proxyLogWriter) Write(p []byte) (int, error) {
	w.logger.Warn("upstream response failed")
	return len(p), nil
}
