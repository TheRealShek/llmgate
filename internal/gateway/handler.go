package gateway

import (
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
)

// ChatLimits controls the request resources checked before forwarding chat requests.
type ChatLimits struct {
	// MaxBodyBytes bounds the incoming JSON body.
	MaxBodyBytes int64
	// MaxTokens bounds explicit output-token requests and supplies omitted values.
	// Zero disables token validation and insertion.
	MaxTokens int64
}

// NewHandler builds the gateway routes and validates chat before forwarding.
// nonStreaming handles model lists and JSON chat responses; streaming handles SSE chat.
// limits bounds accepted request bodies and output-token options before either proxy runs.
// A missing nonStreaming backend leaves health available but returns 503 for models and chat.
// readiness checks backend health; nil returns 503 on the readiness route.
func NewHandler(nonStreaming, streaming, readiness http.Handler, limits ChatLimits) http.Handler {
	var chat http.Handler
	if nonStreaming != nil {
		chat = &chatHandler{
			nonStreaming: nonStreaming,
			streaming:    streaming,
			maxBodyBytes: limits.MaxBodyBytes,
			maxTokens:    limits.MaxTokens,
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", healthHandler)
	mux.Handle("GET /v1/readyz", backendHandler(readiness))
	mux.Handle("GET /v1/models", backendHandler(nonStreaming))
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
