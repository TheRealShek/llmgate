// Package metrics owns process-local Prometheus instrumentation for the public gateway.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/router"
)

// Options bounds authenticated key labels and simultaneous scrape work.
type Options struct {
	KeyCapacity   int
	MaxScrapes    int
	ScrapeTimeout time.Duration
	Backends      map[string]*queue.Queue // Fixed names and optional local queue-depth sources.
}

// Metrics owns one private registry. Startup shares it with the public handler.
type Metrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	ttft          *prometheus.HistogramVec
	active        *prometheus.GaugeVec
	queueWait     *prometheus.HistogramVec
	rejected      *prometheus.CounterVec
	scrape        http.Handler
	scrapeTimeout time.Duration
	mu            sync.Mutex
	keys          map[int64]string
	keyCapacity   int
	backends      map[string]bool
}

// New creates request and admission metrics without global registration or background work.
// Startup validates all limits before construction.
func New(options Options) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(), keys: make(map[int64]string), backends: make(map[string]bool),
		keyCapacity: options.KeyCapacity, scrapeTimeout: options.ScrapeTimeout,
	}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Finished public requests by bounded key ID, selected backend, and HTTP status.",
	}, []string{"key", "backend", "status"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Public request time through handler finalization, including accounting.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"backend"})
	m.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_ttft_seconds",
		Help:    "Arrival to first complete nonterminal SSE data event accepted by the response writer.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
	}, []string{"backend"})
	m.active = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_active_streams",
		Help: "Admitted streaming inference attempts currently proxying, including prefill.",
	}, []string{"backend"})
	m.registry.MustRegister(m.requests, m.duration, m.ttft, m.active)
	m.queueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_queue_wait_seconds",
		Help:    "Time spent acquiring inference capacity, including immediate, rejected, and canceled attempts.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"backend"})
	m.rejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_rejected_total",
		Help: "Request rejections by routing or queue failure reason.",
	}, []string{"reason"})
	m.rejected.WithLabelValues("queue_full")
	m.rejected.WithLabelValues("queue_timeout")
	m.rejected.WithLabelValues("no_backend")
	m.registry.MustRegister(m.queueWait, m.rejected)

	for name, admission := range options.Backends {
		m.backends[name] = true
		m.duration.WithLabelValues(name)
		m.ttft.WithLabelValues(name)
		m.active.WithLabelValues(name)
		m.queueWait.WithLabelValues(name)

		if admission != nil {
			m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Name:        "gateway_queue_depth",
				Help:        "Requests waiting for local inference capacity, excluding occupied slots.",
				ConstLabels: prometheus.Labels{"backend": name},
			}, func() float64 { return float64(admission.State().Waiting) }))
		}
	}

	m.requests.WithLabelValues("none", "none", "200")
	m.duration.WithLabelValues("none")
	m.scrape = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: options.MaxScrapes, Timeout: options.ScrapeTimeout, DisableCompression: true,
	})

	return m
}

// ServeHTTP exports only the private registry; it never queries Redis, Postgres, or the backend.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only scrapes gain a write deadline. Streaming inference keeps its existing lifetime.
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(m.scrapeTimeout)); err == nil {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}

	m.scrape.ServeHTTP(w, r)
}

type contextKey struct{}

type request struct {
	metrics *Metrics
	started time.Time
	keyID   int64
	backend string
}

// Observe surrounds routing after request logging has established arrival time.
// Scrapes are excluded so scraping cannot change its own request metrics.
func (m *Metrics) Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/v1/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		started := requestlog.Started(r.Context())
		if started.IsZero() {
			started = time.Now()
		}

		original := r
		state := &request{metrics: m, started: started}
		r = r.WithContext(context.WithValue(r.Context(), contextKey{}, state))
		writer := &responseWriter{ResponseWriter: w}
		returned := false

		defer func() {
			// Routing updates the instrumented clone. Publish its matched pattern before
			// the outer request logger finishes, including during panic unwinding.
			original.Pattern = r.Pattern

			backend := "none"
			if state.backend != "" {
				backend = state.backend
			}

			status := writer.status.Load()
			if status == 0 && returned && r.Context().Err() == nil {
				status = http.StatusOK
			}

			label := "none"
			if status != 0 {
				label = strconv.Itoa(int(status))
			}

			m.requests.WithLabelValues(m.keyLabel(state.keyID), backend, label).Inc()
			m.duration.WithLabelValues(backend).Observe(time.Since(started).Seconds())
		}()

		next.ServeHTTP(writer, r)

		returned = true
	})
}

// Authenticate records the successful identity on the request's existing instrumentation state.
// The request goroutine owns this state; cancellation callbacks do not mutate it.
func Authenticate(ctx context.Context, id int64) {
	if state, ok := ctx.Value(contextKey{}).(*request); ok {
		state.keyID = id
	}
}

// Admission records one completed capacity acquisition before quota preparation.
// Cancellations contribute wait time but are not overload rejections.
func Admission(ctx context.Context, backend string, elapsed time.Duration, err error) {
	state, ok := ctx.Value(contextKey{}).(*request)
	if !ok {
		return
	}

	if state.metrics.backends[backend] {
		state.metrics.queueWait.WithLabelValues(backend).Observe(elapsed.Seconds())
	}

	switch {
	case errors.Is(err, router.ErrNoBackend):
		state.metrics.rejected.WithLabelValues("no_backend").Inc()
	case errors.Is(err, queue.ErrFull):
		state.metrics.rejected.WithLabelValues("queue_full").Inc()
	case errors.Is(err, queue.ErrTimeout):
		state.metrics.rejected.WithLabelValues("queue_timeout").Inc()
	}
}

// SelectBackend attributes subsequent proxy measurements to a configured backend.
// Fixed startup names prevent request-controlled metric cardinality.
func SelectBackend(ctx context.Context, name string) {
	if state, ok := ctx.Value(contextKey{}).(*request); ok && state.metrics.backends[name] {
		state.backend = name
	}
}

// NoBackend counts a routing rejection that never selects an inference server.
func NoBackend(ctx context.Context) {
	if state, ok := ctx.Value(contextKey{}).(*request); ok {
		state.metrics.rejected.WithLabelValues("no_backend").Inc()
	}
}

// BeginInference returns proxy-lifetime cleanup for the already selected backend.
// Cleanup runs before quota settlement and usage writes, including panic unwinding.
func BeginInference(ctx context.Context, streaming bool) func() {
	state, ok := ctx.Value(contextKey{}).(*request)
	if !ok || state.backend == "" {
		return func() {}
	}

	if !streaming {
		return func() {}
	}

	active := state.metrics.active.WithLabelValues(state.backend)
	active.Inc()

	return active.Dec
}

// FirstData records TTFT once, when the existing SSE observer accepts its first data event.
func FirstData(ctx context.Context, at time.Time) {
	if state, ok := ctx.Value(contextKey{}).(*request); ok && state.backend != "" {
		state.metrics.ttft.WithLabelValues(state.backend).Observe(at.Sub(state.started).Seconds())
	}
}

func (m *Metrics) keyLabel(id int64) string {
	if id <= 0 {
		return "none"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if label, ok := m.keys[id]; ok {
		return label
	}

	if len(m.keys) >= m.keyCapacity {
		return "other"
	}

	label := strconv.FormatInt(id, 10)
	m.keys[id] = label

	return label
}

// responseWriter preserves response control while recording the first final HTTP status.
// ReverseProxy may flush on another goroutine, so status updates use atomic operations.
type responseWriter struct {
	http.ResponseWriter
	status atomic.Int32
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status.CompareAndSwap(0, int32(status))
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	w.status.CompareAndSwap(0, http.StatusOK)
	return w.ResponseWriter.Write(data)
}

func (w *responseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil {
		w.status.CompareAndSwap(0, http.StatusOK)
	}

	return err
}

func (w *responseWriter) Flush() { _ = w.FlushError() }
