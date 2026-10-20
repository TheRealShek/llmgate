package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/store"
)

func cacheRequest(handler http.Handler, secret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	return w
}

// TestCacheOutagePolicy checks fixed expiry, cached identity, and recovery without serving stale entries.
func TestCacheOutagePolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unavailable := false
		calls := 0
		identity := store.Key{ID: 1, Name: "caller", RequestsPerMin: 60, TokensPerDay: 1000}
		keys := lookupFunc(func(context.Context, string) (store.Key, error) {
			calls++

			if unavailable {
				return store.Key{}, store.ErrDatabaseUnavailable
			}

			return identity, nil
		})
		a := NewAuthenticator(keys, Options{
			LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
		})

		handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got, ok := Identity(r.Context()); !ok || got != identity {
				t.Error("cached identity or limits changed")
			}

			w.WriteHeader(http.StatusNoContent)
		}))
		if w := cacheRequest(handler, "known"); w.Code != http.StatusNoContent {
			t.Fatal("could not warm cache", w.Code)
		}

		unavailable = true

		time.Sleep(15 * time.Second)

		if w := cacheRequest(handler, "known"); w.Code != http.StatusNoContent || calls != 1 {
			t.Fatal("unexpired hit queried the unavailable database", w.Code, calls)
		}

		if w := cacheRequest(handler, "unknown"); w.Code != http.StatusServiceUnavailable {
			t.Fatal("cache miss did not fail closed", w.Code)
		}

		time.Sleep(15 * time.Second)

		if w := cacheRequest(handler, "known"); w.Code != http.StatusServiceUnavailable || calls != 3 {
			t.Fatal("hit extended expiry or expired identity survived outage", w.Code, calls)
		}

		unavailable = false
		identity.RequestsPerMin = 120

		if w := cacheRequest(handler, "known"); w.Code != http.StatusNoContent || calls != 4 {
			t.Fatal("database recovery did not refresh identity", w.Code, calls)
		}

		if len(a.active) != 0 || len(a.cache) != 1 || a.cacheOrder.Len() != 1 {
			t.Fatal("request cleanup or cache size is incorrect")
		}
	})
}

// TestCacheEviction checks oldest-entry eviction, hash-only indexing, and bounded expired state.
func TestCacheEviction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		unavailable := false
		keys := lookupFunc(func(context.Context, string) (store.Key, error) {
			if unavailable {
				return store.Key{}, store.ErrDatabaseUnavailable
			}

			return store.Key{ID: 1}, nil
		})
		a := NewAuthenticator(keys, Options{
			LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
		})

		handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		for _, secret := range []string{"oldest", "second", "oldest", "third"} {
			if w := cacheRequest(handler, secret); w.Code != http.StatusNoContent {
				t.Fatal("could not fill cache", w.Code)
			}

			if len(a.cache) > 2 || a.cacheOrder.Len() > 2 {
				t.Fatal("cache exceeded capacity")
			}
		}

		if _, ok := a.cache[sha256.Sum256([]byte("oldest"))]; ok {
			t.Fatal("hit changed eviction order")
		}

		unavailable = true

		for _, secret := range []string{"second", "third"} {
			if w := cacheRequest(handler, secret); w.Code != http.StatusNoContent {
				t.Fatal("retained entry unavailable", w.Code)
			}
		}

		if w := cacheRequest(handler, "oldest"); w.Code != http.StatusServiceUnavailable {
			t.Fatal("evicted identity was served", w.Code)
		}

		time.Sleep(30 * time.Second)

		for _, secret := range []string{"second", "third"} {
			if w := cacheRequest(handler, secret); w.Code != http.StatusServiceUnavailable {
				t.Fatal("expired identity was served", w.Code)
			}
		}

		if len(a.cache) != 0 || a.cacheOrder.Len() != 0 {
			t.Fatal("expired entries retained after access")
		}
	})
}

// TestCacheFailuresNotRetained checks failed lookups never become positive or negative cache entries.
func TestCacheFailuresNotRetained(t *testing.T) {
	for _, cause := range []error{store.ErrKeyNotFound, store.ErrDatabaseUnavailable} {
		t.Run(cause.Error(), func(t *testing.T) {
			calls := 0
			keys := lookupFunc(func(context.Context, string) (store.Key, error) {
				calls++
				if calls == 1 {
					return store.Key{}, cause
				}

				return store.Key{ID: 1}, nil
			})
			a := NewAuthenticator(keys, Options{
				LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
			})
			handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))

			want := http.StatusServiceUnavailable
			if errors.Is(cause, store.ErrKeyNotFound) {
				want = http.StatusUnauthorized
			}

			if w := cacheRequest(handler, "key"); w.Code != want || len(a.cache) != 0 {
				t.Fatal("failed lookup was cached or returned wrong status", w.Code)
			}

			if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent || calls != 2 {
				t.Fatal("failed lookup prevented later authentication", w.Code, calls)
			}
		})
	}
}

// TestCachedRevocation checks cache-hit requests are canceled and failed revocation preserves cache entries.
func TestCachedRevocation(t *testing.T) {
	var revoked atomic.Bool

	failRevocation := true
	calls := 0
	keys := keyStoreStub{
		lookup: func(_ context.Context, secret string) (store.Key, error) {
			calls++

			if secret == "target" {
				if revoked.Load() {
					return store.Key{}, store.ErrKeyNotFound
				}

				return store.Key{ID: 1}, nil
			}

			return store.Key{ID: 2}, nil
		},
		revoke: func(context.Context, int64) error {
			if failRevocation {
				return store.ErrDatabaseUnavailable
			}

			revoked.Store(true)

			return nil
		},
	}
	a := NewAuthenticator(keys, Options{
		LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
	})

	quick := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, secret := range []string{"target", "other"} {
		if w := cacheRequest(quick, secret); w.Code != http.StatusNoContent {
			t.Fatal("could not warm cache", w.Code)
		}
	}

	if err := a.RevokeKey(t.Context(), 1); !errors.Is(err, store.ErrDatabaseUnavailable) {
		t.Fatal("expected revocation failure", err)
	}

	if w := cacheRequest(quick, "target"); w.Code != http.StatusNoContent || calls != 2 {
		t.Fatal("failed revocation discarded cached identity", w.Code, calls)
	}

	started := make(chan struct{})
	done := make(chan struct{})
	active := a.Protect(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer target")

	var workers sync.WaitGroup

	t.Cleanup(func() { cancel(); workers.Wait() })
	workers.Go(func() { active.ServeHTTP(httptest.NewRecorder(), r); close(done) })
	await(t, started)

	failRevocation = false

	if err := a.RevokeKey(t.Context(), 1); err != nil {
		t.Fatal(err)
	}

	await(t, done)

	if w := cacheRequest(quick, "target"); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked cached identity admitted", w.Code)
	}

	if w := cacheRequest(quick, "other"); w.Code != http.StatusNoContent || calls != 3 {
		t.Fatal("revocation discarded another key's cache entry", w.Code, calls)
	}

	if len(a.active) != 0 || len(a.cache) != 1 || a.cacheOrder.Len() != 1 {
		t.Fatal("revocation retained target state")
	}
}

// TestConcurrentCacheFills checks an overlapping lookup cannot renew an existing entry's expiry.
func TestConcurrentCacheFills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32

		started := make(chan struct{})
		release := make(chan struct{})
		keys := lookupFunc(func(context.Context, string) (store.Key, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}

			return store.Key{ID: 1}, nil
		})
		a := NewAuthenticator(keys, Options{
			LookupTimeout: time.Minute, CacheTTL: 30 * time.Second, CacheCapacity: 2,
		})
		handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		done := make(chan struct{})

		t.Cleanup(func() { close(release); <-done })

		go func() {
			defer close(done)

			if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent {
				t.Error("overlapping lookup failed", w.Code)
			}
		}()

		<-started

		time.Sleep(time.Second)

		if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent {
			t.Fatal("second lookup failed", w.Code)
		}

		time.Sleep(time.Second)

		release <- struct{}{}

		<-done

		time.Sleep(29 * time.Second)

		if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent || calls.Load() != 3 {
			t.Fatal("overlapping lookup extended fixed expiry", w.Code, calls.Load())
		}
	})
}

// TestConcurrentCacheCapacity checks concurrent admissions keep cache storage bounded and revocable.
func TestConcurrentCacheCapacity(t *testing.T) {
	keys := keyStoreStub{
		lookup: func(context.Context, string) (store.Key, error) { return store.Key{ID: 1}, nil },
		revoke: func(context.Context, int64) error { return nil },
	}
	a := NewAuthenticator(keys, Options{
		LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 8,
	})
	handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	var workers sync.WaitGroup
	for i := range 64 {
		workers.Go(func() {
			if w := cacheRequest(handler, strconv.Itoa(i)); w.Code != http.StatusNoContent {
				t.Error("concurrent admission failed", w.Code)
			}

			a.mu.Lock()
			defer a.mu.Unlock()

			if len(a.cache) > 8 || a.cacheOrder.Len() != len(a.cache) {
				t.Error("concurrent fills exceeded capacity or left unindexed entries")
			}
		})
	}

	workers.Wait()

	if err := a.RevokeKey(t.Context(), 1); err != nil {
		t.Fatal(err)
	}

	if len(a.cache) != 0 || a.cacheOrder.Len() != 0 || len(a.active) != 0 {
		t.Fatal("concurrent requests retained state after revocation")
	}
}

// TestCacheExpiryDoesNotCancelStream checks expiry limits future admission without ending admitted work.
func TestCacheExpiryDoesNotCancelStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		keys := lookupFunc(func(context.Context, string) (store.Key, error) {
			calls++

			return store.Key{ID: 1}, nil
		})
		a := NewAuthenticator(keys, Options{
			LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
		})

		handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(31 * time.Second)

			if r.Context().Err() != nil {
				t.Error("cache expiry or lookup deadline canceled admitted work")
			}

			w.WriteHeader(http.StatusNoContent)
		}))
		for range 2 {
			if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent {
				t.Fatal("long request failed", w.Code)
			}
		}

		if calls != 2 || len(a.active) != 0 {
			t.Fatal("expiry did not refresh the next request or release tracking", calls)
		}
	})
}

// TestSlowLookupCacheLifetime checks database latency cannot extend the configured freshness window.
func TestSlowLookupCacheLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		keys := lookupFunc(func(context.Context, string) (store.Key, error) {
			calls++

			time.Sleep(2 * time.Second)

			return store.Key{ID: 1}, nil
		})
		a := NewAuthenticator(keys, Options{
			LookupTimeout: 3 * time.Second, CacheTTL: time.Second, CacheCapacity: 2,
		})

		handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		for range 2 {
			if w := cacheRequest(handler, "key"); w.Code != http.StatusNoContent {
				t.Fatal("successful lookup did not authenticate", w.Code)
			}

			if len(a.cache) != 0 || a.cacheOrder.Len() != 0 {
				t.Fatal("slow lookup extended cache freshness")
			}
		}

		if calls != 2 {
			t.Fatal("slow lookup result was reused", calls)
		}
	})
}
