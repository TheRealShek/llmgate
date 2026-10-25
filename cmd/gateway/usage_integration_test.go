package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	for _, name := range []string{"000002_usage.up.sql", "000003_inference_admissions.up.sql"} {
		migration, err := os.ReadFile("../../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
			t.Fatal(err)
		}
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

// TestAdmissionSurvivesDeliveryFailure checks a lost final write retains admission and cached auth cannot bypass storage.
func TestAdmissionSurvivesDeliveryFailure(t *testing.T) {
	pool, databaseURL := authenticationTestPool(t)

	for _, name := range []string{"000002_usage.up.sql", "000003_inference_admissions.up.sql"} {
		migration, err := os.ReadFile("../../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
			t.Fatal(err)
		}
	}

	keys := store.NewKeyStore(pool)

	key, secret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "durable-admission", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	authentication := auth.NewAuthenticator(keys, auth.Options{
		LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 32,
	})
	calls := 0
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++

		pool.Close()

		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming: backend, Authentication: authentication, RequestLimits: testRequestLimits(),
		Quota: testTokenQuota(), Limits: gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 128},
		Usage: usage.NewRecorder(usage.Options{
			Store: store.NewUsageStore(pool), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
		}),
	})
	handler := requestlog.NewHandler(routes, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	var firstID string

	for _, want := range []int{200, 503} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != want || calls != 1 {
			t.Fatal("storage failure changed admitted output or let cached auth bypass admission", w.Code, calls)
		}

		if want == 200 {
			firstID = w.Header().Get("X-Request-ID")
		} else if !strings.Contains(w.Body.String(), "usage_unavailable") {
			t.Fatal("request did not reach the durable admission gate", w.Body.String())
		}
	}

	reopened, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(reopened.Close)

	var admitted, missingFinal int

	err = reopened.QueryRow(t.Context(), `
	    SELECT count(*), count(*) FILTER (WHERE u.request_id IS NULL)
	    FROM inference_admissions a LEFT JOIN usage u ON u.request_id = a.request_id
	    WHERE a.request_id = $1 AND a.key_id = $2`, firstID, key.ID).Scan(&admitted, &missingFinal)
	if err != nil || admitted != 1 || missingFinal != 1 {
		t.Fatal("admission disappeared with failed final delivery", admitted, missingFinal, err)
	}
}

// discardUsage keeps route, authentication, and logging tests independent of persistence.
type discardUsage struct{}

func (discardUsage) RecordAdmission(context.Context, store.Admission) error { return nil }

func (discardUsage) RecordUsage(context.Context, store.Usage) error { return nil }

func testUsageRecorder() *usage.Recorder {
	return usage.NewRecorder(usage.Options{
		Store: discardUsage{}, Backend: "test", WriteTimeout: time.Second, MaxBytes: 64 << 10,
	})
}

// TestForcedShutdownPersistsUsage checks forced cancellation joins every final Postgres insert.
func TestForcedShutdownPersistsUsage(t *testing.T) {
	pool, _ := authenticationTestPool(t)

	for _, name := range []string{"000002_usage.up.sql", "000003_inference_admissions.up.sql"} {
		migration, err := os.ReadFile("../../migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
			t.Fatal(err)
		}
	}

	backendCanceled := make(chan struct{}, 3)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()

		backendCanceled <- struct{}{}
	}))
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	keys := store.NewKeyStore(pool)

	key, secret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "forced-shutdown", RequestsPerMin: 60, TokensPerDay: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}

	routes := gateway.NewHandler(gateway.HandlerOptions{
		Streaming:    proxy.NewStreamingHandler(target, transport, time.Second),
		NonStreaming: proxy.NewNonStreamingHandler(target, transport, time.Second),
		Authentication: auth.NewAuthenticator(keys, auth.Options{
			LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 8,
		}),
		RequestLimits: testRequestLimits(), Quota: testTokenQuota(),
		Usage: usage.NewRecorder(usage.Options{
			Store: store.NewUsageStore(pool), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
		}),
		Limits: gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	server := &http.Server{Addr: "127.0.0.1:0", Handler: routes}

	bound, err := listenServers([]namedServer{{name: "gateway", server: server}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	var workers sync.WaitGroup

	t.Cleanup(func() {
		cancel()

		_ = server.Close()

		workers.Wait()
	})

	done := make(chan error, 1)

	workers.Go(func() { done <- runServers(ctx, bound, 50*time.Millisecond, time.Second) })

	streaming := make(chan struct{}, 3)

	for range 3 {
		workers.Go(func() {
			request, err := http.NewRequest(http.MethodPost,
				"http://"+bound[0].listener.Addr().String()+"/v1/chat/completions",
				strings.NewReader(`{"model":"test","stream":true,"max_tokens":8}`))
			if err != nil {
				t.Error("create inference request", err)
				return
			}

			request.Header.Set("Authorization", "Bearer "+secret)
			request.Header.Set("Content-Type", "application/json")

			client := &http.Client{Timeout: 5 * time.Second}

			response, err := client.Do(request)
			if err != nil {
				t.Error("start inference request")
				return
			}
			defer response.Body.Close()

			reader := bufio.NewReader(response.Body)

			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "data:") {
				t.Error("request did not start streaming")
				return
			}

			streaming <- struct{}{}

			_, _ = io.Copy(io.Discard, reader)
		})
	}

	for range 3 {
		select {
		case <-streaming:
		case <-time.After(5 * time.Second):
			t.Fatal("three inference requests did not become active")
		}
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("forced shutdown lost its deadline failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced shutdown did not finish")
	}

	var recorded int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM usage
	    WHERE key_id=$1 AND status='canceled' AND streaming
	    AND prompt_tokens IS NULL AND completion_tokens IS NULL`, key.ID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}

	if recorded != 3 {
		t.Fatal("shutdown returned before all canceled inference records were written", recorded)
	}

	for range 3 {
		select {
		case <-backendCanceled:
		case <-time.After(time.Second):
			t.Fatal("backend request did not receive cancellation")
		}
	}
}
