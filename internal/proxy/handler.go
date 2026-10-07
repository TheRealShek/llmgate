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

// NewHandler forwards model requests to target, a validated, non-nil backend URL.
// transport sends upstream HTTP requests and is owned by the caller.
// timeout bounds the entire upstream request, including reading the response body.
func NewHandler(target *url.URL, transport http.RoundTripper, timeout time.Duration) http.Handler {
	backend := *target
	forwarder := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&backend)
		},
		Transport:    transport,
		ErrorLog:     log.New(proxyLogWriter{}, "", 0),
		ErrorHandler: handleError,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		forwarder.ServeHTTP(w, r.WithContext(ctx))
	})
}

func handleError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	// Transport errors can contain URLs or credentials; log only the outcome.
	slog.Warn("upstream models request failed", "status", status)
	_ = api.WriteError(w, status, api.Error{
		Message: "upstream request failed",
		Type:    "server_error",
		Code:    "upstream_error",
	})
}

type proxyLogWriter struct{}

func (proxyLogWriter) Write(p []byte) (int, error) {
	// ReverseProxy logs body-copy failures separately from ErrorHandler.
	// Discard its raw message because errors may contain sensitive request data.
	slog.Warn("upstream models response failed")
	return len(p), nil
}
