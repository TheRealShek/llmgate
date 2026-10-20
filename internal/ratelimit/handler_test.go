package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

type bucketFunc func(context.Context, int64, int64) (Decision, error)

func (f bucketFunc) Take(ctx context.Context, id, rate int64) (Decision, error) {
	return f(ctx, id, rate)
}

type testKeys struct{ identities map[string]store.Key }

func (keys testKeys) LookupKey(_ context.Context, secret string) (store.Key, error) {
	if keys.identities != nil {
		key, ok := keys.identities[secret]
		if !ok {
			return store.Key{}, store.ErrKeyNotFound
		}

		return key, nil
	}

	switch secret {
	case "low":
		return store.Key{ID: 1, RequestsPerMin: 2}, nil
	case "high":
		return store.Key{ID: 2, RequestsPerMin: 4}, nil
	default:
		return store.Key{}, store.ErrKeyNotFound
	}
}

func (testKeys) RevokeKey(context.Context, int64) error { return nil }

func protected(limiter *Limiter, next http.Handler) http.Handler {
	a := auth.NewAuthenticator(testKeys{}, auth.Options{
		LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 8,
	})

	return a.Protect(limiter.Protect(next))
}

func request(secret string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)

	return r
}

// TestLimiterResponses checks identity, retry rounding, safe errors, and downstream admission.
func TestLimiterResponses(t *testing.T) {
	for _, tt := range []struct {
		name       string
		decision   Decision
		cause      error
		status     int
		retryAfter string
	}{
		{name: "admitted", decision: Decision{Allowed: true}, status: 204},
		{name: "subsecond retry", decision: Decision{RetryAfter: time.Millisecond}, status: 429, retryAfter: "1"},
		{name: "whole second retry", decision: Decision{RetryAfter: time.Second}, status: 429, retryAfter: "1"},
		{name: "fractional retry", decision: Decision{RetryAfter: 1001 * time.Millisecond}, status: 429, retryAfter: "2"},
		{name: "Redis failure", cause: errors.New("private-secret diagnostics"), status: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			buckets := bucketFunc(func(ctx context.Context, id, rate int64) (Decision, error) {
				if id != 1 || rate != 2 {
					t.Error("authenticated limits did not reach the bucket")
				}

				if _, ok := ctx.Deadline(); !ok {
					t.Error("admission has no deadline")
				}

				return tt.decision, tt.cause
			})
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++

				w.WriteHeader(http.StatusNoContent)
			})

			var logs bytes.Buffer

			handler := requestlog.NewHandler(protected(NewLimiter(buckets, time.Second), next),
				slog.New(slog.NewJSONHandler(&logs, nil)))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request("low"))

			if w.Code != tt.status || w.Header().Get("Retry-After") != tt.retryAfter {
				t.Fatalf("status/retry = %d/%q", w.Code, w.Header().Get("Retry-After"))
			}

			if (calls == 1) != tt.decision.Allowed {
				t.Fatal("rejected request reached downstream work")
			}

			if strings.Contains(w.Body.String()+logs.String(), "private-secret") {
				t.Fatal("limiter diagnostics exposed private data")
			}

			if tt.status != 204 && !strings.Contains(w.Body.String(), `"error":`) {
				t.Fatal("rejection did not use the shared JSON error format")
			}
		})
	}
}

// TestLimiterMissingDependencies checks incomplete construction and missing identity fail closed.
func TestLimiterMissingDependencies(t *testing.T) {
	for _, limiter := range []*Limiter{nil, NewLimiter(nil, time.Second), NewLimiter(bucketFunc(nil), 0)} {
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing limiter admitted work") })
		w := httptest.NewRecorder()
		protected(limiter, next).ServeHTTP(w, request("low"))

		if w.Code != http.StatusServiceUnavailable {
			t.Fatal("missing limiter did not fail closed", w.Code)
		}
	}

	buckets := bucketFunc(func(context.Context, int64, int64) (Decision, error) {
		t.Error("unauthenticated request reached Redis")
		return Decision{}, nil
	})
	w := httptest.NewRecorder()
	NewLimiter(buckets, time.Second).Protect(nil).ServeHTTP(w, request("low"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("missing identity did not fail closed", w.Code)
	}
}

// TestLimiterDeadline checks a stalled decision returns 503 without entering protected work.
func TestLimiterDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		buckets := bucketFunc(func(ctx context.Context, _, _ int64) (Decision, error) {
			<-ctx.Done()
			return Decision{Allowed: true}, nil
		})
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("expired decision admitted work") })
		w := httptest.NewRecorder()
		protected(NewLimiter(buckets, time.Second), next).ServeHTTP(w, request("low"))

		if w.Code != http.StatusServiceUnavailable {
			t.Fatal("decision deadline did not fail closed", w.Code)
		}
	})
}

// TestAdmissionDeadlineDoesNotLimitStreams checks cleanup releases the decision context before inference.
func TestAdmissionDeadlineDoesNotLimitStreams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var decisionCtx context.Context

		buckets := bucketFunc(func(ctx context.Context, _, _ int64) (Decision, error) {
			decisionCtx = ctx
			return Decision{Allowed: true}, nil
		})
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if decisionCtx.Err() != context.Canceled {
				t.Error("decision context was not released")
			}

			time.Sleep(2 * time.Second)

			if r.Context().Err() != nil {
				t.Error("admission deadline leaked into inference")
			}

			w.(http.Flusher).Flush()
		})
		w := httptest.NewRecorder()
		protected(NewLimiter(buckets, time.Second), next).ServeHTTP(w, request("low"))

		if !w.Flushed {
			t.Fatal("limiter lost streaming flush support")
		}
	})
}

// TestLimiterCancellation checks disconnects and revocation cancel in-flight admission and release its handler.
func TestLimiterCancellation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "client disconnect", true: "admin revocation"}[revoke], func(t *testing.T) {
			started := make(chan struct{})
			done := make(chan struct{})
			buckets := bucketFunc(func(ctx context.Context, _, _ int64) (Decision, error) {
				close(started)
				<-ctx.Done()

				return Decision{}, ctx.Err()
			})
			a := auth.NewAuthenticator(testKeys{}, auth.Options{
				LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 8,
			})
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("canceled admission reached backend") })
			handler := a.Protect(NewLimiter(buckets, time.Minute).Protect(next))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			w := httptest.NewRecorder()

			t.Cleanup(func() { cancel(); <-done })

			go func() { defer close(done); handler.ServeHTTP(w, request("low").WithContext(ctx)) }()

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("admission did not start")
			}

			if revoke {
				if err := a.RevokeKey(t.Context(), 1); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("admission did not stop")
			}

			if w.Body.Len() != 0 {
				t.Fatal("canceled request attempted an error response")
			}
		})
	}
}
