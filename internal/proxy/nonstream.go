package proxy

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/therealshek/llmgate/internal/requestlog"
)

// NewNonStreamingHandler forwards non-streaming requests to target, a validated, non-nil backend
// URL.
// transport sends upstream HTTP requests and is owned by the caller.
// totalTimeout bounds the entire upstream request, including reading the response body.
func NewNonStreamingHandler(
	target *url.URL,
	transport http.RoundTripper,
	totalTimeout time.Duration,
) http.Handler {
	forwarder := newForwarder(target, transport)

	// Wrap forwarder so each request receives an upstream timeout context before ServeHTTP runs.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), totalTimeout)
		defer cancel()

		requestForwarder := *forwarder
		requestForwarder.ErrorLog = log.New(proxyLogWriter{logger: requestlog.Logger(ctx)}, "", 0)
		requestForwarder.ServeHTTP(w, r.WithContext(ctx))
	})
}
