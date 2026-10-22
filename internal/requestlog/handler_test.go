package requestlog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestRequestIdentity checks concurrent routes and proxy failures keep separate IDs without
// logging caller secrets.
func TestRequestIdentity(t *testing.T) {
	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("transport-secret")
	})
	forwarder := proxy.NewNonStreamingHandler(
		&url.URL{Scheme: "http", Host: "backend.example"},
		transport,
		time.Second,
	)
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming:   forwarder,
		Authentication: auth.NewAuthenticator(logTestKeyLookup{}, auth.Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024}),
		RequestLimits:  testRequestLimits(),
		Usage:          testUsageRecorder(),
		Limits:         gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	handler := requestlog.NewHandler(routes, logger)
	paths := []string{"/v1/healthz", "/v1/models", "/missing/private-secret?token=query-secret"}

	type result struct {
		id       string
		logCount int
	}

	ids := make(chan result, len(paths)*4)

	var workers sync.WaitGroup

	for range 4 {
		for _, path := range paths {
			workers.Go(func() {
				request := httptest.NewRequest(http.MethodGet, path, strings.NewReader("prompt-secret"))
				request.Header.Set("X-Request-ID", "caller-secret")
				request.Header.Set("Authorization", "Bearer key-secret")

				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)

				id := response.Header().Get("X-Request-ID")
				if id == "" || id == "caller-secret" {
					t.Errorf("invalid generated request ID: %q", id)
				}

				logCount := 2
				if path == "/v1/models" {
					logCount++
				}

				ids <- result{id: id, logCount: logCount}
			})
		}
	}

	workers.Wait()
	close(ids)

	seen := make(map[string]int)
	for got := range ids {
		if seen[got.id] != 0 {
			t.Fatalf("requests shared ID %q", got.id)
		}

		seen[got.id] = got.logCount
	}

	for _, secret := range []string{
		"prompt-secret",
		"caller-secret",
		"key-secret",
		"query-secret",
		"private-secret",
		"transport-secret",
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs exposed %q", secret)
		}
	}

	counts := make(map[string]int)

	decoder := json.NewDecoder(&logs)
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}

		id, _ := entry["request_id"].(string)
		if seen[id] == 0 {
			t.Fatalf("log has an unknown request ID: %v", entry)
		}

		counts[id]++

		if entry["msg"] == "request finished" {
			if entry["completed"] != true || entry["duration_ms"].(float64) < 0 {
				t.Fatalf("invalid completion log: %v", entry)
			}
		}
	}

	for id, want := range seen {
		if counts[id] != want {
			t.Fatalf("request %s has %d logs, want %d", id, counts[id], want)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }

// TestProxyRequestLogs checks forwarded IDs, flushing, and safe correlated logs for both proxy
// failure paths.
func TestProxyRequestLogs(t *testing.T) {
	for _, mode := range []string{"JSON", "stream", "transport failure", "body failure"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer

			logger := slog.New(slog.NewJSONHandler(&logs, nil))

			var upstreamID string

			transport := transportFunc(func(r *http.Request) (*http.Response, error) {
				upstreamID = r.Header.Get("X-Request-ID")

				if mode == "transport failure" {
					return nil, errors.New("transport-secret")
				}

				var body io.ReadCloser = io.NopCloser(strings.NewReader("data: [DONE]\n\n"))
				if mode == "body failure" {
					body = brokenBody{}
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": {"text/event-stream"},
						"X-Request-Id": {"backend-secret"},
					},
					Body: body, Request: r, ContentLength: -1,
				}, nil
			})

			target, err := url.Parse("http://backend.example")
			if err != nil {
				t.Fatal(err)
			}

			var forwarder http.Handler = proxy.NewNonStreamingHandler(target, transport, time.Second)
			if mode == "stream" || mode == "body failure" {
				forwarder = proxy.NewStreamingHandler(target, transport, time.Second)
			}

			handler := requestlog.NewHandler(forwarder, logger)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			request.Header.Set("X-Request-ID", "caller-secret")

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			id := response.Header().Get("X-Request-ID")
			if id == "" || id != upstreamID || len(response.Header().Values("X-Request-ID")) != 1 {
				t.Fatalf("response ID %q differs from upstream ID %q: %v", id, upstreamID, response.Header())
			}

			if mode == "stream" && (!response.Flushed || response.Body.String() != "data: [DONE]\n\n") {
				t.Fatal("logging changed streaming bytes or flushing")
			}

			if mode == "transport failure" && response.Code != http.StatusBadGateway {
				t.Fatalf("transport failure returned %d", response.Code)
			}

			for _, secret := range []string{"caller-secret", "backend-secret", "transport-secret"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("logs exposed %q", secret)
				}
			}

			entries := 0

			decoder := json.NewDecoder(&logs)
			for decoder.More() {
				var entry map[string]any
				if err := decoder.Decode(&entry); err != nil {
					t.Fatal(err)
				}

				if entry["request_id"] != id {
					t.Fatalf("proxy log lost request identity: %v", entry)
				}

				if entry["msg"] == "upstream request failed" || entry["msg"] == "upstream response failed" {
					entries++
				}
			}

			if (mode == "transport failure" || mode == "body failure") && entries == 0 {
				t.Fatal("proxy failure was not logged")
			}
		})
	}
}

// TestAbortedRequest checks logging preserves cancellation, the original writer, and HTTP abort
// panics.
func TestAbortedRequest(t *testing.T) {
	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	response := httptest.NewRecorder()
	handler := requestlog.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w != response || !errors.Is(r.Context().Err(), context.Canceled) {
			t.Fatal("logging changed writer identity or cancellation")
		}

		requestlog.Logger(r.Context()).Warn("handler aborted")
		panic(http.ErrAbortHandler)
	}), logger)

	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("abort panic = %v", recovered)
			}
		}()

		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx),
		)
	}()

	if !strings.Contains(logs.String(), `"completed":false`) {
		t.Fatalf("abort was logged as completed: %s", &logs)
	}
}

type logTestKeyLookup struct{}

func (logTestKeyLookup) LookupKey(_ context.Context, secret string) (store.Key, error) {
	if secret != "key-secret" {
		return store.Key{}, store.ErrKeyNotFound
	}

	return store.Key{ID: 1}, nil
}

func (logTestKeyLookup) RevokeKey(context.Context, int64) error {
	return store.ErrDatabaseUnavailable
}

// allowRequestBuckets isolates existing route and logging tests from Redis.
type allowRequestBuckets struct{}

func (allowRequestBuckets) Take(context.Context, int64, int64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func testRequestLimits() *ratelimit.Limiter {
	return ratelimit.NewLimiter(allowRequestBuckets{}, time.Second)
}

// discardUsage keeps route, authentication, and logging tests independent of persistence.
type discardUsage struct{}

func (discardUsage) RecordUsage(context.Context, store.Usage) error { return nil }

func testUsageRecorder() *usage.Recorder {
	return usage.NewRecorder(usage.Options{
		Store: discardUsage{}, Backend: "test", WriteTimeout: time.Second, MaxBytes: 64 << 10,
	})
}
