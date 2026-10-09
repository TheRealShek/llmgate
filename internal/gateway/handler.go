package gateway

import (
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
)

// NewHandler builds the gateway routes without opening a listener so HTTP behavior
// can be tested independently of server startup and shutdown.
func NewHandler() http.Handler {
	return NewHandlerWithModels(nil)
}

// NewHandlerWithModels builds health and model routes using the supplied backend handler.
// A missing backend leaves health available but returns 503 for model requests.
func NewHandlerWithModels(upstream http.Handler) http.Handler {
	return newHandler(upstream, nil)
}

// NewHandlerWithNonStreamingChat adds non-streaming chat forwarding to upstream.
// maxBodyBytes limits each chat request body; upstream may be nil when no backend is configured.
func NewHandlerWithNonStreamingChat(upstream http.Handler, maxBodyBytes int64) http.Handler {
	return NewHandlerWithNonStreamingChatLimits(upstream, ChatLimits{MaxBodyBytes: maxBodyBytes})
}

// ChatLimits controls the request resources checked before forwarding chat requests.
type ChatLimits struct {
	// MaxBodyBytes bounds the incoming JSON body.
	MaxBodyBytes int64
	// MaxTokens bounds explicit output-token requests and supplies omitted values.
	// Zero preserves the legacy behavior without token validation or insertion.
	MaxTokens int64
}

// NewHandlerWithNonStreamingChatLimits validates chat requests before non-streaming forwarding.
// upstream receives accepted requests; limits supplies the validated body and token bounds.
func NewHandlerWithNonStreamingChatLimits(upstream http.Handler, limits ChatLimits) http.Handler {
	return NewHandlerWithBackends(upstream, nil, limits)
}

// NewHandlerWithBackends routes chat through validation to the selected response mode.
// nonStreaming handles model lists and JSON chat responses; streaming handles SSE chat.
// limits bounds accepted request bodies and output-token options before either proxy runs.
func NewHandlerWithBackends(nonStreaming, streaming http.Handler, limits ChatLimits) http.Handler {
	var chat http.Handler
	if nonStreaming != nil {
		chat = &chatHandler{
			nonStreaming: nonStreaming,
			streaming:    streaming,
			maxBodyBytes: limits.MaxBodyBytes,
			maxTokens:    limits.MaxTokens,
		}
	}
	return newHandler(nonStreaming, chat)
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
