package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestAdmissionRejections checks JSON and SSE overload never reach quota, accounting, or inference.
func TestAdmissionRejections(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"full", "timeout", "canceled", "missing"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				capacity := 1
				if mode == "full" {
					capacity = 0
				}

				waitTimeout := time.Second
				if mode == "timeout" {
					waitTimeout = 30 * time.Millisecond
				}

				admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: capacity, WaitTimeout: waitTimeout})

				release, err := admission.Acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()

				configured := admission
				if mode == "missing" {
					configured = nil
				}

				m := metrics.New(metrics.Options{Backends: map[string]*queue.Queue{"primary": configured}, MaxScrapes: 2, ScrapeTimeout: time.Second})

				backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejection reached inference") })
				recorder := usage.NewRecorder(usage.Options{
					Store: admissionUsage{
						admit: func(context.Context, store.Admission) error {
							t.Error("rejection created durable admission")

							return nil
						},
						captureUsage: func(context.Context, store.Usage) error {
							t.Error("rejection wrote inference usage")

							return nil
						},
					}, Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
				})
				handler := m.Observe(NewHandler(HandlerOptions{
					Backends: []Backend{{
						Name:         "primary",
						Admission:    configured,
						NonStreaming: backend,
						Streaming:    backend,
						Usage:        recorder,
						Quota: tokenQuotaFunc(func(context.Context, store.Key, []byte) (quota.Admission, error) {
							t.Error("rejection reached quota")

							return quota.Admission{}, quota.ErrUnavailable
						})}},
					Health:  testBackendHealth{},
					Metrics: m,

					Authentication: testAuthenticator(),
					RequestLimits:  testRequestLimits(),

					Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
				}))

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				request := admissionRequest(ctx, stream)
				response := httptest.NewRecorder()

				done := make(chan struct{})
				go func() {
					defer close(done)

					handler.ServeHTTP(response, request)
				}()

				defer func() {
					cancel()
					<-done
				}()

				if mode == "canceled" {
					waitAdmissionState(t, admission, queue.State{Active: 1, Waiting: 1})

					if depth := scrapeAdmissionMetrics(t, handler)["gateway_queue_depth"].GetMetric()[0].GetGauge().GetValue(); depth != 1 {
						t.Fatalf("waiting depth = %v, want 1", depth)
					}

					cancel()
				}

				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("rejected request did not finish")
				}

				data := scrapeAdmissionMetrics(t, handler)
				if got := data["gateway_queue_wait_seconds"].GetMetric()[0].GetHistogram().GetSampleCount(); got != 1 {
					t.Fatalf("acquisition samples = %d, want 1", got)
				}

				for _, sample := range data["gateway_rejected_total"].GetMetric() {
					want := float64(0)
					if sample.GetLabel()[0].GetValue() == "queue_"+mode {
						want = 1
					}

					if sample.GetCounter().GetValue() != want {
						t.Fatalf("wrong rejection count: %v", sample)
					}
				}

				if configured != nil && data["gateway_queue_depth"].GetMetric()[0].GetGauge().GetValue() != 0 {
					t.Fatal("rejected waiter retained queue depth")
				}

				code, status := "admission_unavailable", http.StatusServiceUnavailable

				switch mode {
				case "full":
					code, status = "queue_full", http.StatusTooManyRequests

					if response.Header().Get("Retry-After") != "1" {
						t.Fatal("full queue omitted retry guidance")
					}
				case "timeout":
					code = "queue_timeout"
				case "canceled":
					if response.Body.Len() != 0 {
						t.Fatal("canceled waiter wrote a response")
					}

					waitAdmissionState(t, admission, queue.State{Active: 1})

					return
				}

				if response.Code != status || !strings.Contains(response.Body.String(), code) {
					t.Fatalf("rejection = %d %s, want %d %s", response.Code, response.Body.String(), status, code)
				}

				waitAdmissionState(t, admission, queue.State{Active: 1})
			})
		}
	}
}

// TestAdmissionSlotCleanup checks both modes release capacity before final accounting, including aborts.
func TestAdmissionSlotCleanup(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/abort=%t", stream, abort), func(t *testing.T) {
				admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: 1, WaitTimeout: time.Second})
				backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if admission.State().Active != 1 {
						t.Error("inference did not own a slot")
					}

					if abort {
						panic(http.ErrAbortHandler)
					}

					if stream {
						_, _ = io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n")
						w.(http.Flusher).Flush()
					} else {
						_, _ = io.WriteString(w, `{}`)
					}
				})

				var writes atomic.Int32

				recorder := usage.NewRecorder(usage.Options{
					Store: captureUsage(func(context.Context, store.Usage) error {
						writes.Add(1)

						if admission.State() != (queue.State{}) {
							t.Error("final accounting retained inference capacity")
						}

						release, err := admission.Acquire(t.Context())
						if err != nil {
							t.Errorf("next request could not acquire capacity: %v", err)
						} else {
							release()
						}

						return nil
					}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
				})
				handler := NewHandler(HandlerOptions{
					Backends: []Backend{{
						Name:         "primary",
						Admission:    admission,
						NonStreaming: backend,
						Streaming:    backend,
						Usage:        recorder,
						Quota:        testTokenQuota()}},
					Health: testBackendHealth{},

					Authentication: testAuthenticator(),
					RequestLimits:  testRequestLimits(),
					Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
				})

				var recovered any

				func() {
					defer func() { recovered = recover() }()

					handler.ServeHTTP(httptest.NewRecorder(), admissionRequest(t.Context(), stream))
				}()

				if (abort && recovered != http.ErrAbortHandler) || (!abort && recovered != nil) {
					t.Fatalf("unexpected panic: %v", recovered)
				}

				if admission.State() != (queue.State{}) || writes.Load() != 1 {
					t.Fatal("capacity or accounting leaked after proxy exit")
				}
			})
		}
	}
}

// TestAdmissionPreparationFailure checks quota and durable-admission failures release unused capacity.
func TestAdmissionPreparationFailure(t *testing.T) {
	for _, failed := range []string{"quota", "accounting"} {
		t.Run(failed, func(t *testing.T) {
			admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: 0, WaitTimeout: time.Second})
			tokenQuota := tokenQuotaFunc(func(_ context.Context, _ store.Key, body []byte) (quota.Admission, error) {
				if failed == "quota" {
					return quota.Admission{}, quota.ErrUnavailable
				}

				return quota.Admission{Body: body}, nil
			})
			recorder := usage.NewRecorder(usage.Options{
				Store: admissionUsage{admit: func(context.Context, store.Admission) error {
					return errors.New("unavailable")
				}}, Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
			})
			handler := NewHandler(HandlerOptions{
				Backends: []Backend{{
					Name:      "primary",
					Admission: admission,
					NonStreaming: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						t.Error("preparation failure reached inference")
					}),
					Usage: recorder,
					Quota: tokenQuota}},
				Health: testBackendHealth{},

				Authentication: testAuthenticator(),
				RequestLimits:  testRequestLimits(),
				Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, admissionRequest(t.Context(), false))

			if response.Code != http.StatusServiceUnavailable || admission.State() != (queue.State{}) {
				t.Fatalf("failed preparation = %d, state %+v", response.Code, admission.State())
			}
		})
	}
}

func integrationQueue(t *testing.T, options queue.Options) *queue.Queue {
	t.Helper()

	admission, err := queue.New(options)
	if err != nil {
		t.Fatal(err)
	}

	return admission
}

func admissionRequest(ctx context.Context, stream bool) *http.Request {
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)
	request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")

	return request
}

func waitAdmissionState(t *testing.T, admission *queue.Queue, want queue.State) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for admission.State() != want {
		select {
		case <-deadline.C:
			t.Fatalf("admission state = %+v, want %+v", admission.State(), want)
		case <-ticker.C:
		}
	}
}

func scrapeAdmissionMetrics(t *testing.T, handler http.Handler) map[string]*dto.MetricFamily {
	t.Helper()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("scrape = %d: %s", w.Code, w.Body.String())
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	data, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))

	if err != nil {
		t.Fatal(err)
	}

	return data
}

// TestAdmissionWaitThenForward checks queued JSON and SSE requests start only after the active request exits.
func TestAdmissionWaitThenForward(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/cancel=%t", stream, canceled), func(t *testing.T) {
				admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: 1, WaitTimeout: time.Second})
				m := metrics.New(metrics.Options{Backends: map[string]*queue.Queue{"primary": admission}, MaxScrapes: 2, ScrapeTimeout: time.Second})

				var counts, admissions, calls atomic.Int32

				entered := make(chan struct{}, 2)
				allow := make(chan struct{})
				backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					number := calls.Add(1)

					entered <- struct{}{}

					if number == 1 {
						select {
						case <-r.Context().Done():
							return
						case <-allow:
						}
					}

					if stream {
						_, _ = io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n")
					} else {
						_, _ = io.WriteString(w, `{}`)
					}
				})
				recorder := usage.NewRecorder(usage.Options{
					Store: admissionUsage{
						admit: func(context.Context, store.Admission) error {
							admissions.Add(1)

							return nil
						},
						captureUsage: func(context.Context, store.Usage) error { return nil },
					}, Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
				})
				handler := m.Observe(NewHandler(HandlerOptions{
					Backends: []Backend{{
						Name:         "primary",
						Admission:    admission,
						NonStreaming: backend,
						Streaming:    backend,
						Usage:        recorder,
						Quota: tokenQuotaFunc(func(_ context.Context, _ store.Key, body []byte) (quota.Admission, error) {
							counts.Add(1)

							return quota.Admission{Body: body}, nil
						})}},
					Health:  testBackendHealth{},
					Metrics: m,

					Authentication: testAuthenticator(),
					RequestLimits:  testRequestLimits(),

					Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
				}))

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				firstCtx, cancelFirst := context.WithCancel(ctx)
				defer cancelFirst()

				firstDone, secondDone := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(firstDone)

					handler.ServeHTTP(httptest.NewRecorder(), admissionRequest(firstCtx, stream))
				}()

				defer func() {
					cancel()
					<-firstDone
				}()

				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("first request did not enter inference")
				}

				secondResponse := httptest.NewRecorder()

				go func() {
					defer close(secondDone)

					handler.ServeHTTP(secondResponse, admissionRequest(ctx, stream))
				}()

				defer func() {
					cancel()
					<-secondDone
				}()

				waitAdmissionState(t, admission, queue.State{Active: 1, Waiting: 1})
				waitingMetrics := scrapeAdmissionMetrics(t, handler)

				if waitingMetrics["gateway_queue_depth"].GetMetric()[0].GetGauge().GetValue() != 1 ||
					waitingMetrics["gateway_queue_wait_seconds"].GetMetric()[0].GetHistogram().GetSampleCount() != 1 {
					t.Fatal("waiting request was not visible or recorded before acquisition finished")
				}

				if counts.Load() != 1 || admissions.Load() != 1 || calls.Load() != 1 {
					t.Fatal("waiting request started backend preparation")
				}

				if canceled {
					cancelFirst()
				} else {
					close(allow)
				}

				for _, done := range []chan struct{}{firstDone, secondDone} {
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Fatal("requests did not finish after capacity became available")
					}
				}

				if secondResponse.Code != http.StatusOK || counts.Load() != 2 || admissions.Load() != 2 || calls.Load() != 2 {
					t.Fatal("queued request did not complete through quota, accounting, and inference")
				}

				waitAdmissionState(t, admission, queue.State{})
				finishedMetrics := scrapeAdmissionMetrics(t, handler)

				if finishedMetrics["gateway_queue_depth"].GetMetric()[0].GetGauge().GetValue() != 0 ||
					finishedMetrics["gateway_queue_wait_seconds"].GetMetric()[0].GetHistogram().GetSampleCount() != 2 {
					t.Fatal("handoff did not clear depth and record both acquisition times")
				}
			})
		}
	}
}
