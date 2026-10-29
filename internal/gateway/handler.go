package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/router"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// ChatLimits controls the request resources checked before forwarding chat requests.
type ChatLimits struct {
	// MaxBodyBytes bounds the incoming JSON body.
	MaxBodyBytes int64
	// MaxTokens bounds explicit output-token requests and supplies omitted values.
	// Zero disables token validation and insertion.
	MaxTokens int64
	// MaxAttempts bounds backend attempts to one or two; zero uses one.
	MaxAttempts int
	// StreamErrorTimeout bounds terminal SSE error delivery; zero aborts instead.
	StreamErrorTimeout time.Duration
}

// TokenQuota admits validated chat against its daily input and output budget.
type TokenQuota interface {
	Admit(context.Context, store.Key, []byte) (quota.Admission, error)
}

// Backend keeps counting, accounting, and proxying tied to one inference server.
type Backend struct {
	Name         string
	NonStreaming http.Handler
	Streaming    http.Handler
	Admission    *queue.Queue
	Quota        TokenQuota
	Usage        *usage.Recorder
}

// HandlerOptions names the dependencies and limits used by the gateway routes.
// Backend names must be nonempty and unique, validated by startup.
type HandlerOptions struct {
	Backends       []Backend
	CircuitBreaker router.CircuitOptions
	Health         router.Health
	Readiness      http.Handler
	Authentication *auth.Authenticator
	RequestLimits  *ratelimit.Limiter
	Metrics        *metrics.Metrics
	Limits         ChatLimits
}

// NewHandler builds public health routes and authenticated model and chat routes.
// Authentication and request limits run before chat validation or forwarding.
// A missing backend returns 503 after authentication and request admission.
func NewHandler(options HandlerOptions) http.Handler {
	targets := make([]router.Target, 0, len(options.Backends))
	backends := make(map[string]Backend, len(options.Backends))
	configured := false

	for _, target := range options.Backends {
		targets = append(targets, router.Target{Name: target.Name, Admission: target.Admission})
		backends[target.Name] = target
		configured = configured || target.NonStreaming != nil
	}

	routing := router.New(targets, options.Health, options.CircuitBreaker)

	var chat http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics.NoBackend(r.Context())
		noBackendHandler(w, r)
	})
	if configured {
		chat = &chatHandler{
			backends: backends, routing: routing,
			maxBodyBytes: options.Limits.MaxBodyBytes, maxTokens: options.Limits.MaxTokens,
			maxAttempts:        max(1, min(2, options.Limits.MaxAttempts)),
			streamErrorTimeout: options.Limits.StreamErrorTimeout,
		}
	}

	// Models select a healthy server but bypass inference queues and token quotas.
	modelsBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, err := routing.Select()
		if err != nil || backends[name].NonStreaming == nil {
			metrics.NoBackend(r.Context())
			noBackendHandler(w, r)

			return
		}

		metrics.SelectBackend(r.Context(), name)
		backends[name].NonStreaming.ServeHTTP(w, r)
	})
	models := options.RequestLimits.Protect(modelsBackend)
	chat = options.RequestLimits.Protect(chat)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", healthHandler)

	var metricHandler http.Handler
	if options.Metrics != nil {
		metricHandler = options.Metrics
	}

	mux.Handle("GET /v1/metrics", backendHandler(metricHandler))
	mux.Handle("GET /v1/readyz", backendHandler(options.Readiness))
	mux.Handle("GET /v1/models", options.Authentication.Protect(models))
	mux.Handle("POST /v1/chat/completions", options.Authentication.Protect(chat))

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
		Message: "no healthy backend available",
		Type:    "server_error",
		Code:    "no_backend",
	})
}
