package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

type failoverHealth []backend.State

func (h failoverHealth) States() []backend.State { return append([]backend.State(nil), h...) }

type failoverTransport func(*http.Request) (*http.Response, error)

func (f failoverTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failoverState struct {
	mu           sync.Mutex
	mode         string
	cancel       context.CancelFunc
	ownerCancel  context.CancelFunc
	generation   []string
	sentLimits   []int64
	counts       []string
	admissions   []store.Admission
	finals       []store.Usage
	reservations []quota.Reservation
	charges      []int64
	requested    []int64
	reserveCalls int
	queues       map[string]*queue.Queue
}

func (s *failoverState) Reserve(_ context.Context, budget quota.Budget) (quota.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reserveCalls++
	if s.mode == "quota" && s.reserveCalls == 2 {
		return quota.Reservation{RetryAfter: time.Second}, nil
	}

	output := budget.MaxOutputTokens
	if s.mode == "fresh allowance" && s.reserveCalls == 1 {
		output = 2
	}

	reservation := quota.Reservation{
		ID: fmt.Sprintf("reservation-%d", s.reserveCalls), KeyID: budget.KeyID,
		PromptTokens: budget.PromptTokens, OutputTokens: output,
	}
	s.reservations = append(s.reservations, reservation)
	s.requested = append(s.requested, budget.MaxOutputTokens)

	return reservation, nil
}
func (s *failoverState) Settle(_ context.Context, _ quota.Reservation, charge int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.charges = append(s.charges, charge)

	return nil
}
func (s *failoverState) RecordAdmission(_ context.Context, record store.Admission, cancel context.CancelFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !record.Valid() {
		return errors.New("invalid admission")
	}

	s.ownerCancel = cancel
	s.admissions = append(s.admissions, record)

	return nil
}
func (s *failoverState) RecordUsage(_ context.Context, record store.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !record.Valid() {
		return errors.New("invalid final usage")
	}

	s.finals = append(s.finals, record)
	if s.mode == "ownership handoff" {
		s.ownerCancel()
	}

	if s.mode == "handoff" {
		return errors.New("durable handoff unavailable")
	}

	return nil
}

type failoverCounter struct {
	name  string
	state *failoverState
}

func (c failoverCounter) Count(context.Context, []byte) (int64, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()

	c.state.counts = append(c.state.counts, c.name)
	if c.name == "a" {
		return 3, nil
	}

	return 5, nil
}

type brokenResponse struct{ first io.Reader }

func (b *brokenResponse) Read(p []byte) (int, error) {
	if b.first != nil {
		n, err := b.first.Read(p)
		if n > 0 {
			return n, nil
		}

		if err != io.EOF {
			return n, err
		}

		b.first = nil
	}

	return 0, io.ErrUnexpectedEOF
}
func (*brokenResponse) Close() error { return nil }

func failoverGateway(t *testing.T, mode string, attempts, targetCount int) (http.Handler, *failoverState) {
	t.Helper()

	state := &failoverState{mode: mode, queues: make(map[string]*queue.Queue)}
	transport := failoverTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("gateway credential forwarded")
		}

		if r.GetBody != nil {
			t.Error("uncapped body replay survived attempt construction")
		}

		var payloadFields struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payloadFields); err != nil {
			t.Error(err)
		}

		_ = r.Body.Close()
		name := r.URL.Host

		state.mu.Lock()
		state.generation = append(state.generation, name)
		state.sentLimits = append(state.sentLimits, payloadFields.MaxTokens)
		state.mu.Unlock()

		failing := name == "a" || mode == "both"
		if failing {
			switch mode {
			case "transport", "fresh allowance", "handoff", "quota", "ownership handoff":
				return nil, errors.New("private-backend-secret")
			case "timeout":
				return nil, context.DeadlineExceeded
			case "ownership":
				state.ownerCancel()
				return nil, context.Canceled
			case "cancel":
				state.cancel()
				return nil, context.Canceled
			case "informational":
				trace := httptrace.ContextClientTrace(r.Context())
				if err := trace.Got1xxResponse(http.StatusEarlyHints, textproto.MIMEHeader{"Link": {"</model>; rel=preload"}}); err != nil {
					return nil, err
				}

				return nil, errors.New("private-backend-secret")
			}
		}

		status := http.StatusOK

		if failing {
			switch mode {
			case "502", "both":
				status = http.StatusBadGateway
			case "503":
				status = http.StatusServiceUnavailable
			case "504":
				status = http.StatusGatewayTimeout
			case "400":
				status = http.StatusBadRequest
			}
		}

		payload := `{"usage":{"prompt_tokens":5,"completion_tokens":1}}`
		contentType := "application/json"

		streaming := strings.Contains(r.Header.Get("X-Test-Mode"), "stream")
		if streaming {
			contentType = "text/event-stream"
			payload = "data: " + payload + "\n\ndata: [DONE]\n\n"
		}

		var body io.ReadCloser = io.NopCloser(strings.NewReader(payload))

		if failing {
			switch mode {
			case "partial", "headers":
				prefix := ""
				if mode == "partial" {
					prefix = `{"choices":[`
					if streaming {
						prefix = "data: {\"piece\":\"first\"}\n\n"
					}
				}

				body = &brokenResponse{first: strings.NewReader(prefix)}
			case "eof":
				body = io.NopCloser(strings.NewReader("data: {\"piece\":\"first\"}\n\n"))
			case "backend error":
				body = io.NopCloser(strings.NewReader("event: error\ndata: {\"error\":{\"message\":\"backend failed\"}}\n\n"))
			}
		}

		response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: body, ContentLength: -1, Request: r}
		if failing {
			response.Header.Set("X-Failed-Backend", "must-not-leak-on-retry")
		}

		return response, nil
	})

	var targets []Backend

	var health failoverHealth

	for _, name := range []string{"a", "b", "c"}[:targetCount] {
		admission := integrationQueue(t, queue.Options{Slots: 1, Capacity: 1, WaitTimeout: time.Second})
		state.queues[name] = admission
		health = append(health, backend.State{Name: name, Healthy: true})
		address := &url.URL{Scheme: "http", Host: name}
		targets = append(targets, Backend{
			Name: name, Admission: admission,
			Quota:        quota.NewLimiter(failoverCounter{name: name, state: state}, state, quota.Options{CountTimeout: time.Second, RedisTimeout: time.Second}),
			Usage:        usage.NewRecorder(usage.Options{Store: state, Backend: name, WriteTimeout: time.Second, MaxBytes: 4096}),
			NonStreaming: proxy.NewNonStreamingHandler(address, transport, time.Second),
			Streaming:    proxy.NewStreamingHandler(address, transport, time.Second),
		})
	}

	m := metrics.New(metrics.Options{Backends: state.queues, MaxScrapes: 2, ScrapeTimeout: time.Second})
	routes := NewHandler(HandlerOptions{
		Backends: targets, Health: health, Authentication: testAuthenticator(), RequestLimits: testRequestLimits(), Metrics: m,
		Limits: ChatLimits{MaxBodyBytes: 4096, MaxTokens: 8, MaxAttempts: attempts, StreamErrorTimeout: time.Second},
	})

	return requestlog.NewHandler(m.Observe(routes), slog.New(slog.NewJSONHandler(io.Discard, nil))), state
}

// TestChatFailover checks fresh reservations and durable attempt identities across
// transport/status failures, bounded attempts, quota exhaustion, and failed handoff.
func TestChatFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"transport", "502", "503", "504", "timeout", "both", "fresh allowance", "handoff", "quota", "400", "informational", "ownership", "ownership handoff"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				routes, state := failoverGateway(t, mode, 2, 3)

				server := httptest.NewServer(routes)
				defer server.Close()

				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"test","messages":[{"role":"user","content":"hello"}],"max_tokens":8,"stream":%t}`, stream)))
				if err != nil {
					t.Fatal(err)
				}

				request.Header.Set("Content-Type", "application/json")

				if stream {
					request.Header.Set("X-Test-Mode", "stream")
				}

				response, err := authenticatedClient(server).Do(request)
				if err != nil {
					t.Fatal(err)
				}

				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()

				if readErr != nil {
					t.Fatal(readErr)
				}

				wantStatus, wantAttempts := http.StatusOK, 2

				switch mode {
				case "both", "handoff", "informational", "ownership handoff":
					wantStatus = http.StatusBadGateway
				case "ownership":
					wantStatus = http.StatusServiceUnavailable
				case "quota":
					wantStatus = http.StatusTooManyRequests
				case "400":
					wantStatus = http.StatusBadRequest
				}

				if mode == "handoff" || mode == "400" || mode == "informational" || mode == "quota" || mode == "ownership" || mode == "ownership handoff" {
					wantAttempts = 1
				}

				if response.StatusCode != wantStatus || strings.Contains(string(body), "private-backend-secret") {
					t.Fatal(response.StatusCode, string(body))
				}

				if wantAttempts == 2 && mode != "both" && response.Header.Get("X-Failed-Backend") != "" {
					t.Fatal("failed response headers escaped into retry")
				}

				if stream && wantStatus == http.StatusOK && !strings.Contains(string(body), "[DONE]") {
					t.Fatal("retried stream did not finish")
				}

				measured := scrapeAdmissionMetrics(t, routes)

				requests := float64(0)
				for _, sample := range measured["gateway_requests_total"].GetMetric() {
					requests += sample.GetCounter().GetValue()
				}

				if requests != 1 {
					t.Fatal("internal attempts became public requests", requests)
				}

				if stream && wantStatus == http.StatusOK {
					samples := uint64(0)
					for _, sample := range measured["gateway_ttft_seconds"].GetMetric() {
						samples += sample.GetHistogram().GetSampleCount()
					}

					if samples != 1 {
						t.Fatal("retry emitted extra TTFT measurements", samples)
					}
				}

				state.mu.Lock()
				defer state.mu.Unlock()

				if len(state.generation) != wantAttempts || len(state.admissions) != wantAttempts || len(state.finals) != wantAttempts {
					t.Fatal("wrong attempt counts", state.generation, state.admissions, state.finals)
				}

				for i, record := range state.admissions {
					wantID := response.Header.Get("X-Request-ID")
					wantBackend, wantPrompt := "a", int64(3)

					if i == 1 {
						wantID += ":attempt-2"
						wantBackend, wantPrompt = "b", 5
					}

					if record.RequestID != wantID || record.Backend != wantBackend || record.PromptTokens != wantPrompt || record.ReservationID != state.reservations[i].ID {
						t.Fatal("attempt admission mismatch", record)
					}

					if i == 1 && !record.StartedAt.After(state.finals[0].StartedAt) {
						t.Fatal("retry reused the previous attempt start")
					}

					if state.finals[i].RequestID != record.RequestID || state.finals[i].Backend != record.Backend {
						t.Fatal("attempt final mismatch", state.finals[i])
					}

					if state.queues[wantBackend].State() != (queue.State{}) {
						t.Fatal("attempt retained inference capacity")
					}
				}

				if mode == "handoff" {
					if len(state.charges) != 0 {
						t.Fatal("settled without durable final handoff")
					}
				} else {
					if len(state.charges) != wantAttempts || state.charges[0] != -1 {
						t.Fatal("unknown consumption refunded", state.charges)
					}

					if wantAttempts == 2 {
						wantCharge := int64(6)
						if mode == "both" {
							wantCharge = -1
						}

						if state.charges[1] != wantCharge || state.reservations[0].ID == state.reservations[1].ID {
							t.Fatal("retry reused quota or settled wrong counts", state.charges)
						}
					}
				}

				if mode == "fresh allowance" && (state.reservations[0].OutputTokens != 2 || state.requested[1] != 8 || state.sentLimits[0] != 2 || state.sentLimits[1] != 8) {
					t.Fatal("previous capped request reused", state.requested)
				}

				if mode == "quota" && state.reserveCalls != 2 {
					t.Fatal("retry skipped a fresh reservation")
				}
			})
		}
	}
}

// TestChatAfterCommit checks final headers and partial bodies prevent retry for
// either mode, while interrupted SSE delivers exactly one terminal error event.
func TestChatAfterCommit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		modes := []string{"headers", "partial"}
		if stream {
			modes = append(modes, "eof", "backend error")
		}

		for _, mode := range modes {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, mode), func(t *testing.T) {
				routes, state := failoverGateway(t, mode, 2, 2)
				server := httptest.NewUnstartedServer(routes)
				server.Config.ErrorLog = log.New(io.Discard, "", 0)

				server.Start()
				defer server.Close()

				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)))
				request.Header.Set("Content-Type", "application/json")

				if stream {
					request.Header.Set("X-Test-Mode", "stream")
				}

				response, err := authenticatedClient(server).Do(request)
				if err != nil {
					if stream || mode != "headers" {
						t.Fatal(err)
					}
				} else {
					body, readErr := io.ReadAll(response.Body)
					_ = response.Body.Close()

					if stream {
						if readErr != nil || strings.Count(string(body), "event: error") != 1 || strings.Contains(string(body), "[DONE]") {
							t.Fatal("wrong terminal SSE behavior", string(body), readErr)
						}
					} else if readErr == nil || strings.Contains(string(body), "upstream_error") {
						t.Fatal("JSON abort was replaced by another response", string(body), readErr)
					}
				}

				if stream && mode == "headers" {
					measured := scrapeAdmissionMetrics(t, routes)
					for _, sample := range measured["gateway_ttft_seconds"].GetMetric() {
						if sample.GetHistogram().GetSampleCount() != 0 {
							t.Fatal("gateway error was counted as backend first data")
						}
					}
				}

				state.mu.Lock()
				defer state.mu.Unlock()

				if len(state.generation) != 1 || len(state.finals) != 1 || len(state.charges) != 1 || state.charges[0] != -1 {
					t.Fatal("committed response retried or refunded", state.generation, state.finals, state.charges)
				}

				want := store.UsageInterrupted
				if mode == "backend error" {
					want = store.UsageFailed
				}

				if state.finals[0].Status != want {
					t.Fatal(state.finals[0].Status, want)
				}
			})
		}
	}
}

// TestChatRetryUnavailable checks opt-out, single-backend exhaustion, and cancellation never reuse a failed target.
func TestChatRetryUnavailable(t *testing.T) {
	for _, mode := range []string{"disabled", "single", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			attempts, targets, upstreamMode := 2, 2, "transport"
			if mode == "disabled" {
				attempts = 1
			}

			if mode == "single" {
				targets = 1
			}

			if mode == "cancel" {
				upstreamMode = "cancel"
			}

			routes, state := failoverGateway(t, upstreamMode, attempts, targets)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			state.cancel = cancel
			request := admissionRequest(ctx, false)
			w := httptest.NewRecorder()
			routes.ServeHTTP(w, request)

			if mode == "cancel" {
				if w.Body.Len() != 0 {
					t.Fatal("canceled request wrote a response")
				}
			} else if w.Code != http.StatusBadGateway {
				t.Fatal("original failure was lost", w.Code, w.Body.String())
			}

			if len(state.generation) != 1 || len(state.finals) != 1 {
				t.Fatal("failed target reused", state.generation)
			}
		})
	}
}

// TestChatFallbackWait checks a timed-out or canceled retry never counts tokens or generates on its queued target.
func TestChatFallbackWait(t *testing.T) {
	for _, outcome := range []string{"timeout", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				routes, state := failoverGateway(t, "transport", 2, 2)

				release, err := state.queues["b"].Acquire(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				response := httptest.NewRecorder()

				done := make(chan struct{})
				go func() { defer close(done); routes.ServeHTTP(response, admissionRequest(ctx, false)) }()

				synctest.Wait()

				if state.queues["b"].State().Waiting != 1 {
					t.Fatal("retry did not enter its bounded queue")
				}

				if outcome == "cancel" {
					cancel()
				} else {
					time.Sleep(time.Second)
				}

				<-done

				if outcome == "cancel" {
					if response.Body.Len() != 0 {
						t.Fatal("canceled retry wrote a response")
					}
				} else if response.Code != http.StatusBadGateway {
					t.Fatal("retry wait replaced the original upstream failure", response.Code)
				}

				if len(state.counts) != 1 || len(state.generation) != 1 || len(state.admissions) != 1 {
					t.Fatal("waiting retry entered generation or accounting")
				}

				release()

				for name, q := range state.queues {
					if q.State() != (queue.State{}) {
						t.Fatal("retry queue leaked", name, q.State())
					}
				}
			})
		})
	}
}

// TestChatClearsBodyReplay checks in-process callers cannot replay their uncapped input through the transport.
func TestChatClearsBodyReplay(t *testing.T) {
	routes, state := failoverGateway(t, "fresh allowance", 2, 2)
	request := admissionRequest(t.Context(), false)
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{"max_tokens":999999}`)), nil }
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, request)

	if response.Code != http.StatusOK || len(state.generation) != 2 {
		t.Fatal(response.Code, state.generation)
	}
}

type blockedErrorWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *blockedErrorWriter) SetWriteDeadline(at time.Time) error { w.deadline = at; return nil }
func (w *blockedErrorWriter) Write(body []byte) (int, error) {
	if strings.Contains(string(body), "event: error") {
		if w.deadline.IsZero() {
			return 0, errors.New("missing delivery deadline")
		}

		time.Sleep(time.Until(w.deadline))

		return 0, context.DeadlineExceeded
	}

	return w.ResponseRecorder.Write(body)
}

// TestChatTerminalErrorDeadline checks a client that stops reading cannot retain capacity during terminal error delivery.
func TestChatTerminalErrorDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		routes, state := failoverGateway(t, "eof", 2, 2)
		writer := &blockedErrorWriter{ResponseRecorder: httptest.NewRecorder()}
		request := admissionRequest(t.Context(), true)
		request.Header.Set("X-Test-Mode", "stream")

		started := time.Now()

		var aborted any

		func() {
			defer func() { aborted = recover() }()

			routes.ServeHTTP(writer, request)
		}()

		if aborted != http.ErrAbortHandler || time.Since(started) != time.Second {
			t.Fatal("unbounded or missing terminal delivery", aborted, time.Since(started))
		}

		if !writer.deadline.IsZero() {
			t.Fatal("delivery deadline was not cleared")
		}

		if len(state.generation) != 1 || len(state.finals) != 1 || state.finals[0].Status != store.UsageInterrupted {
			t.Fatal("terminal write retried or lost accounting")
		}

		for name, q := range state.queues {
			if q.State() != (queue.State{}) {
				t.Fatal("failed delivery retained capacity", name)
			}
		}
	})
}
