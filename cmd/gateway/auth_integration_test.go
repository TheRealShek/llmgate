package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"github.com/therealshek/llmgate/internal/quota"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/store"
)

// TestKeyCreationAndAuthentication checks the command's persisted key authorizes routes until revoked.
func TestKeyCreationAndAuthentication(t *testing.T) {
	pool, databaseURL := authenticationTestPool(t)
	getenv := func(k string) string {
		if k == "GATEWAY_DATABASE_URL" {
			return databaseURL
		}

		return ""
	}

	var output bytes.Buffer
	if err := createKey(t.Context(), []string{"-name", "integration"}, &output, getenv); err != nil {
		t.Fatal(err)
	}

	var created struct {
		ID     int64  `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(output.Bytes(), &created); err != nil {
		t.Fatal("command output is not JSON")
	}

	if created.ID <= 0 || !strings.HasPrefix(created.APIKey, "lg_") {
		t.Fatal("command did not return an identity and secret")
	}

	var hash []byte
	if err := pool.QueryRow(t.Context(), "SELECT key_hash FROM api_keys WHERE id = $1", created.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}

	if len(hash) != 32 || bytes.Contains(hash, []byte(created.APIKey)) {
		t.Fatal("database did not store a hash")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("backend received gateway secret")
		}

		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	keys := store.NewKeyStore(pool)
	authentication := auth.NewAuthenticator(keys, auth.Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming:   proxy.NewNonStreamingHandler(target, transport, time.Second),
		Streaming:      proxy.NewStreamingHandler(target, transport, time.Second),
		Authentication: authentication,
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "model listing", method: http.MethodGet, path: "/v1/models"},
		{
			name:   "JSON chat",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			body:   `{"stream":false}`,
		},
		{
			name:   "streaming chat",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			body:   `{"stream":true}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer "+created.APIKey)
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			routes.ServeHTTP(w, r)

			if w.Code != 200 {
				t.Fatalf("created key rejected: %d", w.Code)
			}
		})
	}

	if err := authentication.RevokeKey(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+created.APIKey)

	w := httptest.NewRecorder()
	routes.ServeHTTP(w, r)

	if w.Code != 401 {
		t.Fatalf("revoked key status = %d", w.Code)
	}

	pool.Close()

	w = httptest.NewRecorder()
	routes.ServeHTTP(w, r)

	if w.Code != 503 {
		t.Fatalf("unavailable database status = %d", w.Code)
	}
}

// TestAuthenticationCacheDatabaseOutage checks cached identities survive a closed pool while misses fail closed.
func TestAuthenticationCacheDatabaseOutage(t *testing.T) {
	pool, _ := authenticationTestPool(t)
	keys := store.NewKeyStore(pool)

	key, cachedSecret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "cached", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, uncachedSecret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "uncached", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	authentication := auth.NewAuthenticator(keys, auth.Options{
		LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 2,
	})
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming: backend, Authentication: authentication,
		RequestLimits: testRequestLimits(),
		Quota:         testTokenQuota(),
		Usage:         testUsageRecorder(),
	})
	checkStatus := func(secret string, want int) {
		t.Helper()

		r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+secret)

		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)

		if w.Code != want {
			t.Fatalf("authentication status = %d, want %d", w.Code, want)
		}
	}
	checkStatus(cachedSecret, http.StatusNoContent)
	pool.Close()
	checkStatus(cachedSecret, http.StatusNoContent)
	checkStatus(uncachedSecret, http.StatusServiceUnavailable)

	if err := authentication.RevokeKey(t.Context(), key.ID); err == nil {
		t.Fatal("revocation unexpectedly succeeded with a closed database pool")
	}

	checkStatus(cachedSecret, http.StatusNoContent)
}

func authenticationTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	databaseURL := os.Getenv("LLMGATE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set LLMGATE_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("invalid LLMGATE_TEST_DATABASE_URL")
	}

	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 2 * time.Second

	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("cannot open test database pool")
	}

	schema := "auth_test_" + strings.ToLower(rand.Text())

	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal("cannot create authentication test schema")
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		defer admin.Close()

		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Error("cannot remove authentication test schema")
		}
	})

	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("authentication integration test requires a PostgreSQL URL")
	}

	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()

	pool, err := store.OpenPool(ctx, u.String(), store.PoolLimits{MaxConns: 2, ConnectTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	migration, err := os.ReadFile("../../migrations/000001_api_keys.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal("cannot migrate authentication test schema")
	}

	return pool, u.String()
}

// allowRequestBuckets isolates existing route and logging tests from Redis.
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
