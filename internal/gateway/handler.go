package gateway

import (
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/auth"
)

// ChatLimits controls the request resources checked before forwarding chat requests.
type ChatLimits struct {
	// MaxBodyBytes bounds the incoming JSON body.
	MaxBodyBytes int64
	// MaxTokens bounds explicit output-token requests and supplies omitted values.
	// Zero disables token validation and insertion.
	MaxTokens int64
}

// HandlerOptions names the dependencies and limits used by the gateway routes.
type HandlerOptions struct {
	// NonStreaming forwards model lists and JSON chat responses.
	NonStreaming http.Handler
	// Streaming forwards SSE chat responses.
	Streaming http.Handler
	// Readiness probes backend health; nil returns 503.
	Readiness http.Handler
	// Authentication protects models and chat; nil fails closed.
	Authentication *auth.Authenticator
	// Limits bounds chat bodies and output-token options.
	Limits ChatLimits
}

// NewHandler builds public health routes and authenticated model and chat routes.
// Authentication runs before chat validation or forwarding. A missing NonStreaming
// backend returns 503 for authenticated model and chat requests.
func NewHandler(options HandlerOptions) http.Handler {
	var chat http.Handler
	if options.NonStreaming != nil {
		chat = &chatHandler{
			nonStreaming: options.NonStreaming,
			streaming:    options.Streaming,
			maxBodyBytes: options.Limits.MaxBodyBytes,
			maxTokens:    options.Limits.MaxTokens,
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", healthHandler)
	mux.Handle("GET /v1/readyz", backendHandler(options.Readiness))
	mux.Handle("GET /v1/models", options.Authentication.Protect(backendHandler(options.NonStreaming)))
	mux.Handle("POST /v1/chat/completions", options.Authentication.Protect(backendHandler(chat)))

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
