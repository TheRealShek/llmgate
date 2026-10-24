package main

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestInferenceUsageRecords checks actual proxy outcomes persist even after cancellation or an HTTP copy abort.
func TestInferenceUsageRecords(t *testing.T) {
	pool, _ := authenticationTestPool(t)

	migration, err := os.ReadFile("../../migrations/000002_usage.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Header.Get("X-Test-Scenario") {
		case "JSON":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
		case "HTTP error":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "timeout":
			<-r.Context().Done()
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {}\n\n")
			w.(http.Flusher).Flush()

			switch r.Header.Get("X-Test-Scenario") {
			case "stream":
				_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
			case "copy abort":
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}

				_ = connection.Close()
			default:
				<-r.Context().Done()
			}
		}
	}))
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	t.Cleanup(transport.CloseIdleConnections)

	keys := store.NewKeyStore(pool)
	authentication := auth.NewAuthenticator(keys, auth.Options{
		LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 32,
	})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming:   proxy.NewNonStreamingHandler(target, transport, time.Second),
		Streaming:      proxy.NewStreamingHandler(target, transport, time.Second),
		Authentication: authentication, RequestLimits: testRequestLimits(),
		Quota: testTokenQuota(),
		Usage: usage.NewRecorder(usage.Options{
			Store: store.NewUsageStore(pool), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
		}),
		Limits: gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 128},
	})
	public := httptest.NewServer(requestlog.NewHandler(routes, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	t.Cleanup(public.Close)
	client := public.Client()
	client.Timeout = 3 * time.Second

	for _, tt := range []struct {
		name   string
		stream bool
		status store.UsageStatus
		http   int
		known  bool
	}{
		{name: "JSON", status: store.UsageCompleted, http: 200, known: true},
		{name: "stream", stream: true, status: store.UsageCompleted, http: 200, known: true},
		{name: "HTTP error", status: store.UsageFailed, http: 503},
		{name: "timeout", status: store.UsageFailed, http: 504},
		{name: "copy abort", stream: true, status: store.UsageInterrupted, http: 200},
		{name: "disconnect", stream: true, status: store.UsageCanceled, http: 200},
		{name: "revocation", stream: true, status: store.UsageCanceled, http: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key, secret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
				Name: tt.name, RequestsPerMin: 60, TokensPerDay: 1000,
			})
			if err != nil {
				t.Fatal(err)
			}

			streamField := "false"
			if tt.stream {
				streamField = "true"
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			r, err := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"test-model","messages":[],"stream":`+streamField+`}`))
			if err != nil {
				t.Fatal(err)
			}

			r.Header.Set("Authorization", "Bearer "+secret)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Test-Scenario", tt.name)

			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			if response.StatusCode != tt.http {
				t.Fatal("unexpected inference status", response.StatusCode)
			}

			if tt.name == "disconnect" || tt.name == "revocation" {
				if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
					t.Fatal(err)
				}

				if tt.name == "disconnect" {
					cancel()
				} else if err := authentication.RevokeKey(t.Context(), key.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				_, copyErr := io.ReadAll(response.Body)
				if (copyErr != nil) != (tt.name == "copy abort") {
					t.Fatal("proxy completion behavior changed", copyErr)
				}
			}

			deadline, stop := context.WithTimeout(t.Context(), 2*time.Second)
			defer stop()

			for {
				var count int
				if err := pool.QueryRow(deadline, "SELECT count(*) FROM usage WHERE request_id=$1",
					response.Header.Get("X-Request-ID")).Scan(&count); err != nil {
					t.Fatal("usage record did not arrive", err)
				}

				if count == 1 {
					break
				}

				select {
				case <-deadline.Done():
					t.Fatal("usage record did not arrive")
				case <-time.After(10 * time.Millisecond):
				}
			}

			var (
				status             string
				httpStatus         int
				recordedKey        int64
				prompt, completion *int64
			)
			if err := pool.QueryRow(t.Context(), `SELECT key_id, status, http_status, prompt_tokens, completion_tokens
			    FROM usage WHERE request_id=$1`, response.Header.Get("X-Request-ID")).
				Scan(&recordedKey, &status, &httpStatus, &prompt, &completion); err != nil {
				t.Fatal(err)
			}

			if recordedKey != key.ID || status != string(tt.status) || httpStatus != tt.http ||
				(prompt != nil) != tt.known || (completion != nil) != tt.known {
				t.Fatalf("wrong persisted outcome: key=%d status=%s HTTP=%d counts=%v/%v", recordedKey, status, httpStatus, prompt, completion)
			}
		})
	}
}

// discardUsage keeps route, authentication, and logging tests independent of persistence.
type discardUsage struct{}

func (discardUsage) RecordUsage(context.Context, store.Usage) error { return nil }

func testUsageRecorder() *usage.Recorder {
	return usage.NewRecorder(usage.Options{
		Store: discardUsage{}, Backend: "test", WriteTimeout: time.Second, MaxBytes: 64 << 10,
	})
}
