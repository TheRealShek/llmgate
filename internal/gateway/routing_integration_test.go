package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestRoutedChatFlow checks two real HTTP targets keep counting, inference,
// durable identities, and metrics aligned for JSON and SSE, including health loss.
func TestRoutedChatFlow(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()

			entered := make(chan string, 2)
			allow := make(chan struct{})
			admissions := make(chan store.Admission, 2)
			finals := make(chan store.Usage, 2)

			var targets []backend.Target

			var backends []Backend

			queues := map[string]*queue.Queue{}
			health := map[string]*atomic.Bool{}

			for i, name := range []string{"a", "b"} {
				prompt := 3 + i*2
				healthy := &atomic.Bool{}
				healthy.Store(true)
				health[name] = healthy

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "" {
						t.Error("gateway key forwarded")
					}
					defer r.Body.Close()

					w.Header().Set("Content-Type", "application/json")

					switch r.URL.Path {
					case "/health":
						if !healthy.Load() {
							w.WriteHeader(http.StatusServiceUnavailable)
						}
					case "/apply-template":
						_, _ = io.WriteString(w, fmt.Sprintf(`{"prompt":%q}`, name))
					case "/tokenize":
						var body struct {
							Content string `json:"content"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content != name {
							t.Error("counting crossed targets", body, err)
						}

						_ = json.NewEncoder(w).Encode(map[string]any{"tokens": make([]int, prompt)})
					case "/v1/models":
						_, _ = io.WriteString(w, `{"data":[]}`)
					case "/v1/chat/completions":
						_, _ = io.Copy(io.Discard, r.Body)

						entered <- name

						select {
						case <-allow:
						case <-r.Context().Done():
							return
						}

						payload := fmt.Sprintf(`{"backend":%q,"usage":{"prompt_tokens":%d,"completion_tokens":1}}`, name, prompt)

						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
							w.(http.Flusher).Flush()
						} else {
							_, _ = io.WriteString(w, payload)
						}
					default:
						t.Error("unexpected upstream path", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()

				target, err := url.Parse(server.URL)
				if err != nil {
					t.Fatal(err)
				}

				admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: 0, WaitTimeout: time.Second})
				queues[name] = admission
				counter := quota.NewCounter(target, transport, 4096)
				reservations := accountingReservations{settle: func(context.Context, quota.Reservation, int64) error { return nil }}
				recorder := usage.NewRecorder(usage.Options{
					Store: admissionUsage{
						admit:        func(_ context.Context, record store.Admission) error { admissions <- record; return nil },
						captureUsage: func(_ context.Context, record store.Usage) error { finals <- record; return nil },
					}, Backend: name, WriteTimeout: time.Second, MaxBytes: 4096,
				})
				targets = append(targets, backend.Target{Name: name, URL: target})
				backends = append(backends, Backend{
					Name: name, Admission: admission, Usage: recorder,
					Quota:        quota.NewLimiter(counter, reservations, quota.Options{CountTimeout: time.Second, RedisTimeout: time.Second}),
					NonStreaming: proxy.NewNonStreamingHandler(target, transport, time.Second*5),
					Streaming:    proxy.NewStreamingHandler(target, transport, time.Second*5),
				})
			}

			registry, err := backend.NewRegistry(targets, transport, backend.HealthOptions{
				Interval: 5 * time.Millisecond, Timeout: time.Second, Failures: 1, Successes: 1,
			})
			if err != nil {
				t.Fatal(err)
			}

			healthCtx, stopHealth := context.WithCancel(ctx)

			healthDone := make(chan struct{})
			go func() { defer close(healthDone); registry.Run(healthCtx) }()

			defer func() { stopHealth(); <-healthDone }()

			waitRoutedHealth(t, registry, true)

			m := metrics.New(metrics.Options{Backends: queues, MaxScrapes: 2, ScrapeTimeout: time.Second})
			handler := m.Observe(NewHandler(HandlerOptions{
				Backends:       backends,
				Health:         registry,
				Readiness:      backend.NewReadinessHandler(registry),
				Metrics:        m,
				Authentication: testAuthenticator(),
				RequestLimits:  testRequestLimits(),
				Limits:         ChatLimits{MaxBodyBytes: 4096, MaxTokens: 8},
			}))
			readyInitial := httptest.NewRecorder()
			handler.ServeHTTP(readyInitial, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

			if readyInitial.Code != http.StatusOK {
				t.Fatal("healthy readiness failed")
			}

			done := make(chan *httptest.ResponseRecorder, 2)

			var requests sync.WaitGroup

			defer func() { cancel(); requests.Wait() }()

			for _, want := range []string{"a", "b"} {
				requests.Go(func() {
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, admissionRequest(ctx, stream))

					done <- w
				})

				select {
				case got := <-entered:
					if got != want {
						t.Fatalf("selected %s, want %s", got, want)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("inference did not start")
				}
			}

			full := httptest.NewRecorder()
			handler.ServeHTTP(full, admissionRequest(ctx, stream))

			if full.Code != http.StatusTooManyRequests || full.Header().Get("Retry-After") != "1" {
				t.Fatal("saturation", full.Code, full.Body.String())
			}

			modelList := httptest.NewRecorder()
			handler.ServeHTTP(modelList, authenticatedRequest(http.MethodGet, "/v1/models", nil))

			if modelList.Code != http.StatusOK {
				t.Fatal("model listing blocked by inference", modelList.Code)
			}

			close(allow)

			for range 2 {
				select {
				case w := <-done:
					if w.Code != http.StatusOK {
						t.Fatal(w.Code, w.Body.String())
					}
				case <-time.After(2 * time.Second):
					t.Fatal("inference did not finish")
				}
			}

			for range 2 {
				record := <-admissions

				want := int64(3)
				if record.Backend == "b" {
					want = 5
				}

				if record.PromptTokens != want || record.OutputTokens != 8 {
					t.Fatal("counting or admission identity mismatch", record)
				}

				final := <-finals
				if final.Status != store.UsageCompleted || final.PromptTokens == nil || *final.PromptTokens != map[string]int64{"a": 3, "b": 5}[final.Backend] {
					t.Fatal("final usage identity mismatch", final)
				}
			}

			data := scrapeAdmissionMetrics(t, handler)
			for _, sample := range data["gateway_active_streams"].GetMetric() {
				if sample.GetGauge().GetValue() != 0 {
					t.Fatal("stream gauge leaked", sample)
				}
			}

			for _, sample := range data["gateway_queue_wait_seconds"].GetMetric() {
				if sample.GetHistogram().GetSampleCount() != 1 {
					t.Fatal("wait attributed to wrong target", sample)
				}
			}

			for _, sample := range data["gateway_ttft_seconds"].GetMetric() {
				want := uint64(0)
				if stream {
					want = 1
				}

				if sample.GetHistogram().GetSampleCount() != want {
					t.Fatal("TTFT attributed to wrong target", sample)
				}
			}

			for _, state := range queues {
				if state.State() != (queue.State{}) {
					t.Fatal("inference capacity leaked")
				}
			}

			for _, state := range health {
				state.Store(false)
			}

			waitRoutedHealth(t, registry, false)

			unavailable := httptest.NewRecorder()
			handler.ServeHTTP(unavailable, admissionRequest(ctx, stream))

			if unavailable.Code != http.StatusServiceUnavailable {
				t.Fatal("unhealthy routing admitted inference", unavailable.Code)
			}

			ready := httptest.NewRecorder()
			handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

			if ready.Code != http.StatusServiceUnavailable {
				t.Fatal("readiness disagrees with router")
			}
		})
	}
}

func waitRoutedHealth(t *testing.T, registry *backend.Registry, want bool) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		matched := true
		for _, state := range registry.States() {
			matched = matched && state.Healthy == want
		}

		if matched {
			return
		}

		select {
		case <-deadline.C:
			t.Fatal("backend health did not converge")
		case <-ticker.C:
		}
	}
}
