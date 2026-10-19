package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/store"
)

type keyStoreStub struct {
	lookup func(context.Context, string) (store.Key, error)
	revoke func(context.Context, int64) error
}

func (s keyStoreStub) LookupKey(ctx context.Context, secret string) (store.Key, error) {
	return s.lookup(ctx, secret)
}

func (s keyStoreStub) RevokeKey(ctx context.Context, id int64) error {
	return s.revoke(ctx, id)
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach expected state")
	}
}

// TestRevocationIsolation checks successful revocation signals every target request and preserves other keys.
func TestRevocationIsolation(t *testing.T) {
	var revoked atomic.Bool

	keys := keyStoreStub{
		lookup: func(_ context.Context, secret string) (store.Key, error) {
			if secret == "target" {
				if revoked.Load() {
					return store.Key{}, store.ErrKeyNotFound
				}

				return store.Key{ID: 1}, nil
			}

			return store.Key{ID: 2}, nil
		},
		revoke: func(context.Context, int64) error { revoked.Store(true); return nil },
	}
	a := NewAuthenticator(keys, time.Second)
	ctx, cancel := context.WithCancel(t.Context())

	var workers sync.WaitGroup

	t.Cleanup(func() { cancel(); workers.Wait() })

	started := make(chan struct{}, 3)
	ended := make(chan int64, 3)

	handler := a.Protect(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		key, _ := Identity(r.Context())

		started <- struct{}{}

		<-r.Context().Done()

		ended <- key.ID
	}))
	for _, secret := range []string{"target", "target", "other"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+secret)
		workers.Go(func() { handler.ServeHTTP(httptest.NewRecorder(), r) })
	}

	for range 3 {
		await(t, started)
	}

	if err := a.RevokeKey(t.Context(), 1); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		select {
		case id := <-ended:
			if id != 1 {
				t.Fatal("another key was canceled")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("target request did not stop")
		}
	}

	select {
	case <-ended:
		t.Fatal("other key stopped before client cancellation")
	default:
	}

	if err := a.RevokeKey(t.Context(), 1); err != nil {
		t.Fatal("repeated revocation failed", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer target")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked admission status = %d", w.Code)
	}

	cancel()
	workers.Wait()

	if len(a.active) != 0 {
		t.Fatal("completed requests retained registry entries")
	}
}

// TestRevocationCrossingLookup checks stale successful lookups retry before entering a protected handler.
func TestRevocationCrossingLookup(t *testing.T) {
	for _, revokeID := range []int64{1, 2} {
		t.Run(map[int64]string{1: "same key", 2: "other key"}[revokeID], func(t *testing.T) {
			lookupStarted := make(chan struct{})
			releaseLookup := make(chan struct{})

			var (
				calls   atomic.Int32
				revoked atomic.Bool
			)

			keys := keyStoreStub{
				lookup: func(ctx context.Context, _ string) (store.Key, error) {
					if calls.Add(1) == 1 {
						close(lookupStarted)

						select {
						case <-releaseLookup:
						case <-ctx.Done():
							return store.Key{}, ctx.Err()
						}

						return store.Key{ID: 1}, nil
					}

					if revokeID == 1 && revoked.Load() {
						return store.Key{}, store.ErrKeyNotFound
					}

					return store.Key{ID: 1}, nil
				},
				revoke: func(context.Context, int64) error { revoked.Store(true); return nil },
			}
			a := NewAuthenticator(keys, time.Second)
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if revokeID == 1 {
					t.Error("stale identity reached protected work")
				}

				w.WriteHeader(http.StatusNoContent)
			})
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer secret")

			w := httptest.NewRecorder()
			done := make(chan struct{})

			var workers sync.WaitGroup

			t.Cleanup(workers.Wait)
			workers.Go(func() { a.Protect(next).ServeHTTP(w, r); close(done) })
			await(t, lookupStarted)

			if err := a.RevokeKey(t.Context(), revokeID); err != nil {
				t.Fatal(err)
			}

			close(releaseLookup)
			await(t, done)

			want := http.StatusUnauthorized
			if revokeID == 2 {
				want = http.StatusNoContent
			}

			if w.Code != want || calls.Load() != 2 || len(a.active) != 0 {
				t.Fatalf("status/calls/registry = %d/%d/%d", w.Code, calls.Load(), len(a.active))
			}
		})
	}
}

// TestRevocationFailure checks storage errors preserve active requests and their generation.
func TestRevocationFailure(t *testing.T) {
	cause := errors.New("database failed")
	a := NewAuthenticator(keyStoreStub{
		revoke: func(context.Context, int64) error { return cause },
	}, time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a.active[1] = map[*activeRequest]struct{}{{cancel: cancel}: {}}
	if err := a.RevokeKey(t.Context(), 1); !errors.Is(err, cause) {
		t.Fatal("storage cause lost", err)
	}

	if ctx.Err() != nil || a.generation != 0 || len(a.active[1]) != 1 {
		t.Fatal("failed revocation changed active requests")
	}
}

// TestRegistryCleanup checks normal returns, client cancellation, and panics release all tracking state.
func TestRegistryCleanup(t *testing.T) {
	for _, outcome := range []string{"normal", "client cancellation", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			a := NewAuthenticator(lookupFunc(func(context.Context, string) (store.Key, error) {
				return store.Key{ID: 1}, nil
			}), time.Second)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				switch outcome {
				case "client cancellation":
					cancel()
				case "panic":
					panic(http.ErrAbortHandler)
				}
			})
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer secret")

			for range 100 {
				func() {
					defer func() {
						if value := recover(); value != nil && value != http.ErrAbortHandler {
							t.Errorf("unexpected panic: %v", value)
						}
					}()

					a.Protect(next).ServeHTTP(httptest.NewRecorder(), r)
				}()
			}

			if len(a.active) != 0 {
				t.Fatal("completed requests retained state")
			}
		})
	}
}

// TestConcurrentRevocationAndCleanup checks registry synchronization under repeated admission and revocation.
func TestConcurrentRevocationAndCleanup(t *testing.T) {
	a := NewAuthenticator(keyStoreStub{
		lookup: func(context.Context, string) (store.Key, error) { return store.Key{ID: 1}, nil },
		revoke: func(context.Context, int64) error { return nil },
	}, time.Second)
	handler := a.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	var workers sync.WaitGroup

	for range 100 {
		workers.Go(func() {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer secret")
			handler.ServeHTTP(httptest.NewRecorder(), r)
		})
		workers.Go(func() {
			if err := a.RevokeKey(t.Context(), 1); err != nil {
				t.Error(err)
			}
		})
	}

	workers.Wait()

	if len(a.active) != 0 {
		t.Fatal("registry grew after concurrent requests completed")
	}
}

// TestCommittedRevocationAfterDisconnect checks successful persistence still signals requests after operator cancellation.
func TestCommittedRevocationAfterDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a := NewAuthenticator(keyStoreStub{
		revoke: func(context.Context, int64) error { cancel(); return nil },
	}, time.Second)
	called := false
	request := &activeRequest{cancel: func() {
		called = true

		if !a.mu.TryLock() {
			t.Error("cancellation ran under the registry lock")
			return
		}

		a.mu.Unlock()
	}}
	a.active[1] = map[*activeRequest]struct{}{request: {}}

	if err := a.RevokeKey(ctx, 1); err != nil || !called {
		t.Fatal("committed revocation skipped cancellation", err)
	}

	a.remove(1, request)

	if len(a.active) != 0 {
		t.Fatal("canceled request could not release its entry")
	}
}

// TestLookupRetryDeadline checks crossed lookups share one deadline and cannot retain rejected requests.
func TestLookupRetryDeadline(t *testing.T) {
	var (
		a             *Authenticator
		firstDeadline time.Time
	)

	calls := 0
	keys := keyStoreStub{
		lookup: func(ctx context.Context, _ string) (store.Key, error) {
			calls++

			deadline, _ := ctx.Deadline()
			if calls == 1 {
				firstDeadline = deadline

				if err := a.RevokeKey(ctx, 2); err != nil {
					t.Error(err)
				}
			} else {
				if !deadline.Equal(firstDeadline) {
					t.Error("retry reset the lookup deadline")
				}

				<-ctx.Done()
			}

			return store.Key{ID: 1}, nil
		},
		revoke: func(context.Context, int64) error { return nil },
	}
	a = NewAuthenticator(keys, 20*time.Millisecond)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer secret")

	w := httptest.NewRecorder()
	a.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("expired retry reached protected work")
	})).ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable || calls != 2 || len(a.active) != 0 {
		t.Fatalf("status/calls/registry = %d/%d/%d", w.Code, calls, len(a.active))
	}
}
