// Package auth identifies callers before protected gateway handlers run.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// KeyLookup resolves a secret to an active identity without retaining the secret.
type KeyLookup interface {
	LookupKey(context.Context, string) (store.Key, error)
}

// Authenticator checks keys with a separate deadline before invoking route handlers.
type Authenticator struct {
	keys          KeyLookup
	lookupTimeout time.Duration
}

// NewAuthenticator uses keys and a positive lookupTimeout supplied by startup.
func NewAuthenticator(keys KeyLookup, lookupTimeout time.Duration) *Authenticator {
	return &Authenticator{keys: keys, lookupTimeout: lookupTimeout}
}

// Protect authenticates a matched route without wrapping its response writer.
// A missing authenticator fails closed rather than disabling authentication.
func (a *Authenticator) Protect(next http.Handler) http.Handler {
	return &handler{auth: a, next: next}
}

type handler struct {
	auth *Authenticator
	next http.Handler
}

type identityKey struct{}

// Identity returns the authenticated key's identity and limits, never its secret or hash.
func Identity(ctx context.Context) (store.Key, bool) {
	key, ok := ctx.Value(identityKey{}).(store.Key)
	return key, ok
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		writeUnauthorized(w)
		return
	}

	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
		parts[1] == "" || strings.ContainsAny(parts[1], "\t\r\n,") {
		writeUnauthorized(w)
		return
	}

	if h.auth == nil || h.auth.keys == nil || h.auth.lookupTimeout <= 0 {
		writeUnavailable(w)
		return
	}

	// The route supplies the incoming context. Bound only its database lookup,
	// then release that deadline before the authenticated request enters next.
	lookupCtx, cancel := context.WithTimeout(r.Context(), h.auth.lookupTimeout)
	defer cancel()

	key, err := h.auth.keys.LookupKey(lookupCtx, parts[1])
	lookupErr := lookupCtx.Err()

	cancel()

	if r.Context().Err() != nil {
		return
	}

	if err != nil || lookupErr != nil {
		if lookupErr == nil && errors.Is(err, store.ErrKeyNotFound) {
			writeUnauthorized(w)
			return
		}

		requestlog.Logger(r.Context()).WarnContext(r.Context(), "authentication lookup failed")
		writeUnavailable(w)

		return
	}

	// WithContext adds identity while retaining request logging and client cancellation.
	// Pass the original writer to the protected handler so streaming can still flush.
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, key))
	h.next.ServeHTTP(w, r)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	_ = api.WriteError(w, http.StatusUnauthorized, api.Error{
		Message: "invalid API key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	})
}

func writeUnavailable(w http.ResponseWriter) {
	_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
		Message: "authentication unavailable",
		Type:    "server_error",
		Code:    "authentication_unavailable",
	})
}
