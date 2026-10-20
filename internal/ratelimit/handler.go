// Package ratelimit admits authenticated requests before validation or inference.
package ratelimit

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/requestlog"
)

// BucketStore consumes request credits using an atomic shared admission decision.
type BucketStore interface {
	Take(context.Context, int64, int64) (Decision, error)
}

// Limiter owns the admission deadline, without extending it to inference work.
type Limiter struct {
	buckets BucketStore
	timeout time.Duration
}

// NewLimiter uses the bucket store and positive decision timeout supplied by startup.
func NewLimiter(buckets BucketStore, timeout time.Duration) *Limiter {
	return &Limiter{buckets: buckets, timeout: timeout}
}

// Protect places request admission between authentication and the protected handler.
// Missing dependencies fail closed. The original writer preserves streaming flush support.
func (l *Limiter) Protect(next http.Handler) http.Handler {
	return &handler{limiter: l, next: next}
}

type handler struct {
	limiter *Limiter
	next    http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := auth.Identity(r.Context())
	if !ok || h.limiter == nil || h.limiter.buckets == nil || h.limiter.timeout <= 0 {
		writeUnavailable(w)
		return
	}

	// Authentication supplies identity, client cancellation, and revocation signals.
	// Only Redis admission gains this deadline; next keeps the original request context.
	ctx, cancel := context.WithTimeout(r.Context(), h.limiter.timeout)
	defer cancel()

	decision, err := h.limiter.buckets.Take(ctx, key.ID, key.RequestsPerMin)
	decisionErr := ctx.Err()

	cancel()

	if r.Context().Err() != nil {
		return
	}

	if err != nil || decisionErr != nil {
		requestlog.Logger(r.Context()).WarnContext(r.Context(), "request limiting failed", "key_id", key.ID)
		writeUnavailable(w)

		return
	}

	if !decision.Allowed {
		seconds := int64(decision.RetryAfter / time.Second)
		if decision.RetryAfter%time.Second > 0 {
			seconds++
		}

		w.Header().Set("Retry-After", strconv.FormatInt(max(1, seconds), 10))
		w.Header().Set("Cache-Control", "no-store")
		_ = api.WriteError(w, http.StatusTooManyRequests, api.Error{
			Message: "request rate limit exceeded", Type: "rate_limit_error", Code: "rate_limit_exceeded",
		})

		return
	}

	h.next.ServeHTTP(w, r)
}

func writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
		Message: "request limiting unavailable", Type: "server_error", Code: "rate_limit_unavailable",
	})
}
