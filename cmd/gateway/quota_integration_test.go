package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestDailyQuotaFlow checks authenticated capped responses, grace, refunds, and uncertain outcomes end to end.
func TestDailyQuotaFlow(t *testing.T) {
	for _, tt := range []struct {
		name                                                        string
		grace                                                       int64
		stream, missing, partial, refund, canceled, dbDown, aborted bool
	}{
		{name: "zero grace"}, {name: "ten percent grace", grace: 10},
		{name: "stream", stream: true}, {name: "missing usage", missing: true},
		{name: "interrupted stream", stream: true, partial: true}, {name: "refund", refund: true},
		{name: "client cancellation", stream: true, canceled: true},
		{name: "backend disconnect", stream: true, aborted: true},
		{name: "usage database write failure", refund: true, dbDown: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/apply-template":
					_, _ = io.WriteString(w, `{"prompt":"rendered"}`)
					return
				case "/tokenize":
					_, _ = io.WriteString(w, `{"tokens":[1,2,3,4,5]}`)
					return
				}

				var body struct {
					MaxTokens     int64 `json:"max_tokens"`
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}

				want := int64(15) + tt.grace/5
				if tt.refund {
					want = 10
				}

				if body.MaxTokens != want {
					t.Error("wrong backend cap", body.MaxTokens, want)
				}

				completion := body.MaxTokens
				if tt.refund {
					completion = 2
				}

				result := map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "generated text"}, "finish_reason": "length"}}}
				if !tt.missing {
					result["usage"] = map[string]int64{"prompt_tokens": 5, "completion_tokens": completion}
				}

				data, _ := json.Marshal(result)

				if tt.stream {
					if !body.StreamOptions.IncludeUsage {
						t.Error("stream usage was not requested")
					}

					w.Header().Set("Content-Type", "text/event-stream")

					if tt.partial || tt.canceled || tt.aborted {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"generated text\"}}]}\n\n")
						w.(http.Flusher).Flush()

						if tt.aborted {
							connection, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}

							_ = connection.Close()
						}

						if tt.canceled {
							<-r.Context().Done()
						}

						return
					}

					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(data)
				}
			}))
			t.Cleanup(backend.Close)

			public, secret, id, pool, client := quotaTestGateway(t, backend.URL, tt.grace)
			if tt.dbDown {
				if _, err := pool.Exec(t.Context(), "DROP TABLE usage"); err != nil {
					t.Fatal(err)
				}
			}

			requestCtx, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()

			payload := fmt.Sprintf(`{"model":"glm-edge-4b-chat","messages":[{"role":"user","content":"Hello"}],"max_tokens":50,"n_predict":50,"stream":%t}`, tt.stream)
			if tt.refund {
				payload = strings.Replace(payload, `"max_tokens":50`, `"max_tokens":10`, 1)
			}

			request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, public.URL+"/v1/chat/completions", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Authorization", "Bearer "+secret)
			request.Header.Set("Content-Type", "application/json")

			response, err := public.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}

			var data []byte

			if tt.canceled {
				line, readErr := bufio.NewReader(response.Body).ReadBytes('\n')
				if readErr != nil {
					t.Fatal(readErr)
				}

				data = line

				cancelRequest()
			} else {
				data, err = io.ReadAll(response.Body)
			}

			_ = response.Body.Close()
			if (err != nil && !tt.aborted) || response.StatusCode != 200 || !bytes.Contains(data, []byte("generated text")) {
				t.Fatal("quota discarded response", response.StatusCode, string(data), err)
			}

			expected := int64(20) + tt.grace/5
			if tt.refund {
				expected = 7
				if tt.dbDown {
					expected = 15
				}
			}

			deadline := time.Now().Add(2 * time.Second)

			for {
				fields, err := client.HLen(t.Context(), "llmgate:tokens:v1:"+strconv.FormatInt(id, 10)).Result()

				var rows int

				var queryErr error

				if tt.dbDown {
					rows = 1
				} else {
					queryErr = pool.QueryRow(t.Context(), "SELECT count(*) FROM usage").Scan(&rows)
				}

				if err != nil || queryErr != nil {
					t.Fatal(err, queryErr)
				}

				wantFields := int64(2)
				if tt.dbDown {
					wantFields = 3
				}

				if fields == wantFields && rows == 1 {
					break
				}

				if time.Now().After(deadline) {
					t.Fatal("final accounting did not finish", fields, rows)
				}

				time.Sleep(10 * time.Millisecond)
			}

			used, err := client.HGet(t.Context(), "llmgate:tokens:v1:"+strconv.FormatInt(id, 10), "used").Int64()
			if err != nil || used != expected {
				t.Fatal("wrong final quota charge", used, expected, err)
			}

			if !tt.refund {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/v1/chat/completions", strings.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				request.Header.Set("Authorization", "Bearer "+secret)
				request.Header.Set("Content-Type", "application/json")

				response, err := public.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}

				_, _ = io.Copy(io.Discard, response.Body)

				_ = response.Body.Close()
				if response.StatusCode != 429 || response.Header.Get("Retry-After") == "" {
					t.Fatal("exhausted key admitted", response.StatusCode)
				}
			}
		})
	}
}

func quotaTestGateway(t *testing.T, address string, grace int64) (*httptest.Server, string, int64, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	pool, _ := authenticationTestPool(t)

	migrateUsage(t, pool)

	redisURL := os.Getenv("LLMGATE_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set LLMGATE_TEST_REDIS_URL for quota integration tests")
	}

	client, err := ratelimit.OpenClient(t.Context(), redisURL, ratelimit.ClientLimits{MaxConns: 4, ConnectTimeout: time.Second, CommandTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	keys := store.NewKeyStore(pool)

	// Each temporary Postgres schema needs distinct IDs in the shared test Redis.
	firstID, err := rand.Int(rand.Reader, big.NewInt(1<<52))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), "SELECT setval(pg_get_serial_sequence('api_keys', 'id'), $1, false)", firstID.Int64()+1); err != nil {
		t.Fatal(err)
	}

	key, secret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{Name: "quota", RequestsPerMin: 1000, TokensPerDay: 20})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_ = client.Del(ctx, "llmgate:tokens:v1:"+strconv.FormatInt(key.ID, 10)).Err()
		_ = client.Close()
	})

	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Second
	t.Cleanup(transport.CloseIdleConnections)
	tokenQuota := quota.NewLimiter(quota.NewCounter(target, transport, 8<<20), quota.NewRedisStore(client), quota.Options{CountTimeout: time.Second, RedisTimeout: time.Second, GracePercent: grace})
	m := metrics.New(metrics.Options{KeyCapacity: 10, MaxScrapes: 2, ScrapeTimeout: time.Second})
	routes := gateway.NewHandler(gateway.HandlerOptions{
		Admission:      testInferenceQueue(),
		Metrics:        m,
		NonStreaming:   proxy.NewNonStreamingHandler(target, transport, time.Second),
		Streaming:      proxy.NewStreamingHandler(target, transport, time.Second),
		Authentication: auth.NewAuthenticator(keys, auth.Options{LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 16}),
		RequestLimits:  testRequestLimits(), Quota: tokenQuota,
		Usage:  usage.NewRecorder(usage.Options{Store: directUsageStore{store.NewUsageStore(pool)}, Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024}),
		Limits: gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 128},
	})
	public := httptest.NewServer(requestlog.NewHandler(m.Observe(routes), slog.New(slog.NewJSONHandler(io.Discard, nil))))
	t.Cleanup(public.Close)
	public.Client().Timeout = 5 * time.Second

	return public, secret, key.ID, pool, client
}

// TestLlamaQuotaResponses checks real GPU generation preserves capped JSON and SSE output.
func TestLlamaQuotaResponses(t *testing.T) {
	address := os.Getenv("LLMGATE_TEST_LLAMA_URL")
	if address == "" {
		t.Skip("set LLMGATE_TEST_LLAMA_URL to test real backend quota termination")
	}

	for _, stream := range []bool{false, true} {
		t.Run(strconv.FormatBool(stream), func(t *testing.T) {
			public, secret, id, _, client := quotaTestGateway(t, address, 0)
			payload := fmt.Sprintf(`{"model":"glm-edge-4b-chat","messages":[{"role":"user","content":"Count from one to ten."}],"max_tokens":50,"n_predict":50,"stream":%t,"temperature":0}`, stream)

			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, public.URL+"/v1/chat/completions", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Authorization", "Bearer "+secret)
			request.Header.Set("Content-Type", "application/json")

			response, err := public.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(response.Body)

			_ = response.Body.Close()
			if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte(`"finish_reason":"length"`)) {
				t.Fatal("unclean quota termination", response.StatusCode, string(body), err)
			}

			if stream && !bytes.Contains(body, []byte("[DONE]")) {
				t.Fatal("missing terminal marker")
			}

			deadline := time.Now().Add(2 * time.Second)

			for {
				fields, err := client.HLen(t.Context(), "llmgate:tokens:v1:"+strconv.FormatInt(id, 10)).Result()
				if err != nil {
					t.Fatal(err)
				}

				if fields == 2 {
					break
				}

				if time.Now().After(deadline) {
					t.Fatal("backend quota settlement timed out")
				}

				time.Sleep(10 * time.Millisecond)
			}

			used, err := client.HGet(t.Context(), "llmgate:tokens:v1:"+strconv.FormatInt(id, 10), "used").Int64()
			if err != nil || used != 20 {
				t.Fatal("real backend exceeded or underused cap", used, err)
			}
		})
	}
}
