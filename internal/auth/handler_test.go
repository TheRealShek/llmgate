package auth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

type lookupFunc func(context.Context, string) (store.Key, error)

func (f lookupFunc) LookupKey(ctx context.Context, secret string) (store.Key, error) {
	return f(ctx, secret)
}

// TestAuthenticationFailures checks credentials and storage failures stop protected work.
func TestAuthenticationFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers []string
		err     error
		status  int
		lookups int
	}{
		{name: "missing", status: 401},
		{name: "empty", headers: []string{""}, status: 401},
		{name: "wrong scheme", headers: []string{"Basic secret"}, status: 401},
		{name: "missing secret", headers: []string{"Bearer "}, status: 401},
		{name: "extra field", headers: []string{"Bearer secret extra"}, status: 401},
		{name: "tab", headers: []string{"Bearer secret\textra"}, status: 401},
		{
			name: "duplicate", headers: []string{"Bearer secret", "Bearer other"},
			status: 401,
		},
		{name: "combined", headers: []string{"Bearer secret,Bearer other"}, status: 401},
		{
			name: "unknown or revoked", headers: []string{"Bearer secret"}, err: store.ErrKeyNotFound,
			status: 401, lookups: 1,
		},
		{
			name: "database failure", headers: []string{"Bearer secret"}, err: errors.New("password=private-secret"),
			status: 503, lookups: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			keys := lookupFunc(func(context.Context, string) (store.Key, error) {
				calls++
				return store.Key{}, tt.err
			})
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected caller reached protected handler")
			})

			var logs bytes.Buffer

			handler := requestlog.NewHandler(NewAuthenticator(keys, Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024}).Protect(next),
				slog.New(slog.NewJSONHandler(&logs, nil)))

			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			for _, header := range tt.headers {
				r.Header.Add("Authorization", header)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if w.Code != tt.status || calls != tt.lookups {
				t.Fatalf("status/lookups = %d/%d, want %d/%d", w.Code, calls, tt.status, tt.lookups)
			}

			if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Request-ID") == "" {
				t.Fatal("authentication error lost shared JSON format or request identity")
			}

			if tt.status == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing bearer challenge")
			}

			if strings.Contains(w.Body.String()+logs.String(), "secret") {
				t.Fatal("authentication leaked secret or storage diagnostics")
			}
		})
	}
}

// TestAuthenticationIdentity checks identity, metadata, cancellation, and flushing survive lookup.
func TestAuthenticationIdentity(t *testing.T) {
	key := store.Key{ID: 42, Name: "client", RequestsPerMin: 12, TokensPerDay: 300}

	var lookupCtx context.Context

	keys := lookupFunc(func(ctx context.Context, secret string) (store.Key, error) {
		lookupCtx = ctx

		if secret != "secret" {
			t.Error("lookup received a changed credential")
		}

		if _, ok := ctx.Deadline(); !ok {
			t.Error("lookup has no deadline")
		}

		return key, nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := Identity(r.Context())
		if !ok || got != key {
			t.Errorf("identity = %+v, present %v", got, ok)
		}

		if lookupCtx.Err() != context.Canceled {
			t.Error("lookup deadline was not released")
		}

		if _, ok := r.Context().Deadline(); ok {
			t.Error("lookup deadline leaked into stream")
		}

		if requestlog.ID(r.Context()) == "" {
			t.Error("identity replaced logging metadata")
		}

		if _, ok := w.(http.Flusher); !ok {
			t.Error("authentication removed flushing")
		}

		cancel()

		if r.Context().Err() != context.Canceled {
			t.Error("identity lost client cancellation")
		}

		w.WriteHeader(http.StatusAccepted)
	})
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	r.Header.Set("Authorization", "bearer secret")

	w := httptest.NewRecorder()
	requestlog.NewHandler(NewAuthenticator(keys, Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024}).Protect(next),
		slog.New(slog.NewJSONHandler(io.Discard, nil))).ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}

	if _, ok := Identity(context.Background()); ok {
		t.Fatal("unauthenticated context has identity")
	}
}

// TestAuthenticationCancellation checks deadlines and client disconnects cancel database work.
func TestAuthenticationCancellation(t *testing.T) {
	for _, clientCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "lookup deadline", true: "client cancellation"}[clientCancel], func(t *testing.T) {
			started := make(chan struct{})
			keys := lookupFunc(func(ctx context.Context, _ string) (store.Key, error) {
				close(started)
				<-ctx.Done()

				return store.Key{}, ctx.Err()
			})
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("canceled lookup reached next") })

			timeout := 20 * time.Millisecond
			if clientCancel {
				timeout = time.Second
			}

			ctx, cancel := context.WithCancel(t.Context())

			var workers sync.WaitGroup

			t.Cleanup(func() { cancel(); workers.Wait() })

			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer secret")

			w := httptest.NewRecorder()
			done := make(chan struct{})

			workers.Go(func() {
				NewAuthenticator(keys, Options{LookupTimeout: timeout, CacheTTL: 30 * time.Second, CacheCapacity: 1024}).Protect(next).ServeHTTP(w, r)
				close(done)
			})

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not start")
			}

			if clientCancel {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("lookup did not stop")
			}

			if clientCancel {
				if w.Body.Len() != 0 {
					t.Fatal("disconnected client received error body")
				}
			} else if w.Code != 503 {
				t.Fatalf("deadline status = %d", w.Code)
			}
		})
	}
}

// TestMissingAuthenticator checks missing dependencies cannot enable anonymous access.
func TestMissingAuthenticator(t *testing.T) {
	for _, tt := range []struct {
		name          string
		authenticator *Authenticator
	}{
		{name: "missing authenticator"},
		{name: "non-positive cache TTL", authenticator: NewAuthenticator(lookupFunc(nil), Options{
			LookupTimeout: time.Second, CacheCapacity: 1,
		})},
		{name: "non-positive cache capacity", authenticator: NewAuthenticator(lookupFunc(nil), Options{
			LookupTimeout: time.Second, CacheTTL: time.Second,
		})},
		{name: "missing key lookup", authenticator: NewAuthenticator(nil, Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024})},
		{name: "non-positive timeout", authenticator: NewAuthenticator(lookupFunc(nil), Options{LookupTimeout: 0, CacheTTL: 30 * time.Second, CacheCapacity: 1024})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer secret")

			w := httptest.NewRecorder()
			tt.authenticator.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("missing auth admitted caller")
			})).ServeHTTP(w, r)

			if w.Code != 503 {
				t.Fatalf("missing authentication status = %d", w.Code)
			}
		})
	}
}

func (lookupFunc) RevokeKey(context.Context, int64) error {
	return store.ErrDatabaseUnavailable
}

// TestAuthenticatedMetrics checks only a verified identity enters the per-key metric label.
func TestAuthenticatedMetrics(t *testing.T) {
	m := metrics.New(metrics.Options{KeyCapacity: 2, MaxScrapes: 2, ScrapeTimeout: time.Second})
	keys := lookupFunc(func(context.Context, string) (store.Key, error) { return store.Key{ID: 42}, nil })
	a := NewAuthenticator(keys, Options{LookupTimeout: time.Second, CacheTTL: time.Second, CacheCapacity: 2})

	handler := m.Observe(a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	for _, secret := range []string{"Bearer private-key", "invalid"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", secret)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}

	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))

	if !strings.Contains(w.Body.String(), `gateway_requests_total{backend="none",key="42",status="204"} 1`) || !strings.Contains(w.Body.String(), `gateway_requests_total{backend="none",key="none",status="401"} 1`) || strings.Contains(w.Body.String(), "private-key") {
		t.Fatal("identity metrics were inaccurate or leaked credentials", w.Body.String())
	}
}
