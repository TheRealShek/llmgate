package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/store"
)

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

// allowRequestBuckets keeps routing tests independent of Redis admission policy.
type allowRequestBuckets struct{}

func (allowRequestBuckets) Take(context.Context, int64, int64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func testRequestLimits() *ratelimit.Limiter {
	return ratelimit.NewLimiter(allowRequestBuckets{}, time.Second)
}
