package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/ratelimit"
)

type routeBuckets func(context.Context, int64, int64) (ratelimit.Decision, error)

func (f routeBuckets) Take(ctx context.Context, id, limit int64) (ratelimit.Decision, error) {
	return f(ctx, id, limit)
}

// TestRateLimitRouteOrder checks probes bypass Redis while protected routes authenticate and limit before validation.
func TestRateLimitRouteOrder(t *testing.T) {
	for _, tt := range []struct {
		name, method, path, secret, body string
		status, decisions                int
	}{
		{name: "liveness", method: "GET", path: "/v1/healthz", status: 200},
		{name: "readiness", method: "GET", path: "/v1/readyz", status: 204},
		{name: "unknown route", method: "GET", path: "/missing", secret: "test-key", status: 404},
		{name: "wrong method", method: "POST", path: "/v1/models", secret: "test-key", status: 405},
		{name: "missing key", method: "GET", path: "/v1/models", status: 401},
		{name: "invalid key", method: "GET", path: "/v1/models", secret: "invalid", status: 401},
		{name: "models", method: "GET", path: "/v1/models", secret: "test-key", status: 429, decisions: 1},
		{name: "HEAD models", method: "HEAD", path: "/v1/models", secret: "test-key", status: 429, decisions: 1},
		{name: "JSON chat", method: "POST", path: "/v1/chat/completions", secret: "test-key", body: `{"stream":false}`, status: 429, decisions: 1},
		{name: "streaming chat", method: "POST", path: "/v1/chat/completions", secret: "test-key", body: `{"stream":true}`, status: 429, decisions: 1},
		{name: "invalid JSON", method: "POST", path: "/v1/chat/completions", secret: "test-key", body: `{`, status: 429, decisions: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decisions := 0
			buckets := routeBuckets(func(_ context.Context, id, rate int64) (ratelimit.Decision, error) {
				decisions++

				if id != 1 || rate != 60 {
					t.Error("bucket did not receive the authenticated identity and limit")
				}

				return ratelimit.Decision{RetryAfter: time.Second}, nil
			})
			backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected request reached the backend")
			})
			readiness := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			handler := NewHandler(HandlerOptions{
				Authentication: testAuthenticator(), RequestLimits: ratelimit.NewLimiter(buckets, time.Second),
				Usage:        testUsageRecorder(),
				NonStreaming: backend, Streaming: backend, Readiness: readiness,
				Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})

			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.secret != "" {
				r.Header.Set("Authorization", "Bearer "+tt.secret)
			}

			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if w.Code != tt.status || decisions != tt.decisions {
				t.Fatalf("status/decisions = %d/%d, want %d/%d", w.Code, decisions, tt.status, tt.decisions)
			}
		})
	}
}

// TestMissingRateLimiter checks gateway construction cannot silently disable request admission.
func TestMissingRateLimiter(t *testing.T) {
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing limiter admitted inference") })
	handler := NewHandler(HandlerOptions{Authentication: testAuthenticator(), NonStreaming: backend})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authenticatedRequest(http.MethodGet, "/v1/models", nil))

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "rate_limit_unavailable") {
		t.Fatal("missing limiter did not fail closed", w.Code)
	}
}
