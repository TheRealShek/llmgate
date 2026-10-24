package gateway

import (
	"context"
	"github.com/therealshek/llmgate/internal/quota"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/store"
)

// TestHandler checks the health response and routing so unsupported methods and
// unrelated paths cannot silently pass health checks.
func TestHandler(t *testing.T) {
	handler := NewHandler(HandlerOptions{
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
	})

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "healthy", method: http.MethodGet, path: "/v1/healthz", status: http.StatusOK},
		{
			name:   "unsupported method",
			method: http.MethodPost,
			path:   "/v1/healthz",
			status: http.StatusMethodNotAllowed,
		},
		{name: "unknown route", method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
		{
			name:   "health path suffix",
			method: http.MethodGet,
			path:   "/v1/healthz/extra",
			status: http.StatusNotFound,
		},
		{
			name:   "unversioned health",
			method: http.MethodGet,
			path:   "/healthz",
			status: http.StatusNotFound,
		},
		{
			name:   "unversioned models",
			method: http.MethodGet,
			path:   "/models",
			status: http.StatusNotFound,
		},
		{
			name:   "unsupported API version",
			method: http.MethodGet,
			path:   "/v2/healthz",
			status: http.StatusNotFound,
		},
		{
			name:   "no backend",
			method: http.MethodGet,
			path:   "/v1/models",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "readiness without backend",
			method: http.MethodGet,
			path:   "/v1/readyz",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "readiness unsupported method",
			method: http.MethodPost,
			path:   "/v1/readyz",
			status: http.StatusMethodNotAllowed,
		},
		{
			name:   "unversioned readiness",
			method: http.MethodGet,
			path:   "/readyz",
			status: http.StatusNotFound,
		},
		{
			name:   "chat without backend",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "models unsupported method",
			method: http.MethodPost,
			path:   "/v1/models",
			status: http.StatusMethodNotAllowed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := authenticatedRequest(tt.method, tt.path, nil)
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}

			if tt.status == http.StatusOK {
				if body := recorder.Body.String(); body != "ok\n" {
					t.Errorf("body = %q, want %q", body, "ok\n")
				}

				contentType := recorder.Header().Get("Content-Type")
				if contentType != "text/plain; charset=utf-8" {
					t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", contentType)
				}
			}

			if tt.status == http.StatusMethodNotAllowed {
				if allow := recorder.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Errorf("Allow = %q, want GET, HEAD", allow)
				}
			}
		})
	}
}

// TestHandlerModels checks model route integration without affecting the health route.
func TestHandlerModels(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("models"))
	})
	handler := NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/models", nil))

	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "models" {
		t.Fatalf("model route response: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestProtectedRoutes checks auth runs before validation or proxying and leaves public routes alone.
func TestProtectedRoutes(t *testing.T) {
	backendCalls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++

		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Streaming:      upstream,
		Readiness:      upstream,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{
			name:   "models require key",
			method: http.MethodGet,
			path:   "/v1/models",
			status: 401,
		},
		{
			name:   "HEAD models require key",
			method: http.MethodHead,
			path:   "/v1/models",
			status: 401,
		},
		{
			name:   "chat authenticates before validation",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			body:   "malformed",
			status: 401,
		},
		{name: "public health", method: http.MethodGet, path: "/v1/healthz", status: 200},
		{name: "public readiness", method: http.MethodGet, path: "/v1/readyz", status: 204},
		{name: "unknown route", method: http.MethodGet, path: "/missing", status: 404},
		{
			name:   "unsupported method",
			method: http.MethodGet,
			path:   "/v1/chat/completions",
			status: 405,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			if w.Code != tt.status {
				t.Errorf("status %d, want %d", w.Code, tt.status)
			}
		})
	}

	if backendCalls != 1 {
		t.Fatalf("only readiness should reach backend: calls = %d", backendCalls)
	}
}

type testKeyLookup struct{}

func (testKeyLookup) LookupKey(_ context.Context, secret string) (store.Key, error) {
	if secret != "test-key" {
		return store.Key{}, store.ErrKeyNotFound
	}

	return store.Key{ID: 1, Name: "test", RequestsPerMin: 60, TokensPerDay: 100000}, nil
}

func testAuthenticator() *auth.Authenticator {
	return auth.NewAuthenticator(testKeyLookup{}, auth.Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024})
}

func authenticatedRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Authorization", "Bearer test-key")

	return r
}

type authenticatedTransport struct {
	base http.RoundTripper
}

func (t authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Test clients add credentials to a copy before sending it to the real gateway.
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer test-key")

	return t.base.RoundTrip(r)
}

func authenticatedClient(server *httptest.Server) *http.Client {
	client := *server.Client()
	client.Transport = authenticatedTransport{base: client.Transport}

	return &client
}

func (testKeyLookup) RevokeKey(context.Context, int64) error {
	return store.ErrDatabaseUnavailable
}

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
				Quota:        testTokenQuota(),
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

// allowRequestBuckets keeps routing tests independent of Redis admission policy.
type allowRequestBuckets struct{}

func (allowRequestBuckets) Take(context.Context, int64, int64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func testRequestLimits() *ratelimit.Limiter {
	return ratelimit.NewLimiter(allowRequestBuckets{}, time.Second)
}

type allowTokenQuota struct{}

func (allowTokenQuota) Admit(_ context.Context, _ store.Key, body []byte) (quota.Admission, error) {
	return quota.Admission{Body: body}, nil
}

func testTokenQuota() allowTokenQuota { return allowTokenQuota{} }

// TestMetricsRoute checks scrapes bypass authentication and unavailable inference dependencies.
func TestMetricsRoute(t *testing.T) {
	m := metrics.New(metrics.Options{KeyCapacity: 2, MaxScrapes: 2, ScrapeTimeout: time.Second})
	handler := m.Observe(NewHandler(HandlerOptions{Metrics: m}))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "/v1/metrics", nil))

		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "gateway_active_streams") {
			t.Fatal(w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/metrics", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w.Code)
	}
}
