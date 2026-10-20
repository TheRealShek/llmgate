// Package auth identifies callers before protected gateway handlers run.
package auth

import (
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// KeyStore resolves active identities and persists revocation without retaining secrets.
type KeyStore interface {
	LookupKey(context.Context, string) (store.Key, error)
	RevokeKey(context.Context, int64) error
}

// Options bounds database lookups and the successful-identity cache. All values must be positive.
type Options struct {
	LookupTimeout time.Duration
	CacheTTL      time.Duration
	CacheCapacity int
}

// Authenticator owns key lookup, revocation, and active requests for one gateway process.
type Authenticator struct {
	keys          KeyStore
	lookupTimeout time.Duration
	cacheTTL      time.Duration
	cacheCapacity int
	// mu coordinates lookup admission with revocation publication. It never covers
	// database operations, inference handlers, or cancellation callbacks.
	mu         sync.Mutex
	generation uint64
	active     map[int64]map[*activeRequest]struct{}
	cache      map[[sha256.Size]byte]*list.Element
	cacheOrder list.List
}

// NewAuthenticator uses keys and the positive limits supplied by startup.
func NewAuthenticator(keys KeyStore, options Options) *Authenticator {
	return &Authenticator{
		keys:          keys,
		lookupTimeout: options.LookupTimeout,
		cacheTTL:      options.CacheTTL,
		cacheCapacity: options.CacheCapacity,
		active:        make(map[int64]map[*activeRequest]struct{}),
		cache:         make(map[[sha256.Size]byte]*list.Element),
	}
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

	if h.auth == nil || h.auth.keys == nil || h.auth.lookupTimeout <= 0 ||
		h.auth.cacheTTL <= 0 || h.auth.cacheCapacity <= 0 {
		writeUnavailable(w)
		return
	}

	// The matched route supplies client cancellation. Revocation can also cancel
	// this request; its database deadline ends before the request enters next.
	requestCtx, cancelRequest := context.WithCancel(r.Context())
	defer cancelRequest()

	request := &activeRequest{cancel: cancelRequest}

	lookupCtx, cancel := context.WithTimeout(requestCtx, h.auth.lookupTimeout)
	defer cancel()

	key, err := h.auth.lookupAndRegister(lookupCtx, parts[1], request)
	lookupErr := lookupCtx.Err()

	cancel()

	if err == nil {
		defer h.auth.remove(key.ID, request)
	}

	if r.Context().Err() != nil {
		return
	}

	if requestCtx.Err() != nil {
		writeUnauthorized(w)
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
	r = r.WithContext(context.WithValue(requestCtx, identityKey{}, key))
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
