// Package backend checks the availability of the configured inference server.
package backend

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/requestlog"
)

type readinessHandler struct {
	healthURL string
	client    *http.Client
	timeout   time.Duration
}

// NewReadinessHandler probes target's health endpoint on each readiness request.
// target must be a validated, non-nil backend base URL. The caller owns transport.
// timeout bounds the probe; only HTTP 200 means the backend is ready.
func NewReadinessHandler(
	target *url.URL,
	transport http.RoundTripper,
	timeout time.Duration,
) http.Handler {
	return &readinessHandler{
		healthURL: target.JoinPath("health").String(),
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout: timeout,
	}
}

func (h *readinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A gateway readiness request enters here. The probe inherits client cancellation
	// and gets its own deadline before going to the backend's /health endpoint.
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, h.healthURL, nil)
	if err != nil {
		writeNotReady(w)
		return
	}

	if id := requestlog.ID(ctx); id != "" {
		probe.Header.Set("X-Request-ID", id)
	}

	response, err := h.client.Do(probe)
	if err != nil {
		// The URL and transport error can contain sensitive details. Return a fixed
		// error; the outer request logger supplies identity and elapsed time.
		writeNotReady(w)
		return
	}
	// Readiness uses the backend status, not its response body. Close immediately
	// so a large or stalled body cannot hold the probe open or reach the client.
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeNotReady(w)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func writeNotReady(w http.ResponseWriter) {
	_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
		Message: "backend is not ready",
		Type:    "server_error",
		Code:    "backend_not_ready",
	})
}
