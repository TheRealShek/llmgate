package gateway

import (
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
)

// NewHandler builds the gateway routes without opening a listener so HTTP behavior
// can be tested independently of server startup and shutdown.
func NewHandler() http.Handler {
	return NewHandlerWithUpstream(nil)
}

// NewHandlerWithUpstream builds health and model routes using the supplied backend handler.
// A missing backend leaves health available but returns 503 for model requests.
func NewHandlerWithUpstream(upstream http.Handler) http.Handler {
	return newHandler(upstream, nil)
}

// NewHandlerWithChat adds non-streaming chat forwarding to upstream.
// maxBodyBytes limits each chat request body; upstream may be nil when no backend is configured.
func NewHandlerWithChat(upstream http.Handler, maxBodyBytes int64) http.Handler {
	var chat http.Handler
	if upstream != nil {
		chat = &chatHandler{upstream: upstream, maxBodyBytes: maxBodyBytes}
	}
	return newHandler(upstream, chat)
}

func newHandler(upstream, chat http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", healthHandler)
	mux.Handle("GET /v1/models", backendHandler(upstream))
	mux.Handle("POST /v1/chat/completions", backendHandler(chat))
	return mux
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Liveness only checks this process; an unavailable backend belongs to readiness.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// A failed write means the response could not be delivered; there is no further work.
	_, _ = w.Write([]byte("ok\n"))
}

func backendHandler(upstream http.Handler) http.Handler {
	if upstream != nil {
		return upstream
	}
	return http.HandlerFunc(noBackendHandler)
}

func noBackendHandler(w http.ResponseWriter, r *http.Request) {
	_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
		Message: "no backend configured",
		Type:    "server_error",
		Code:    "no_backend",
	})
}
