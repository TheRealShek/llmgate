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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.Handle("GET /v1/models", modelsHandler(upstream))
	return mux
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Liveness only checks this process; an unavailable backend belongs to readiness.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// A failed write means the response could not be delivered; there is no further work.
	_, _ = w.Write([]byte("ok\n"))
}

func modelsHandler(upstream http.Handler) http.Handler {
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
