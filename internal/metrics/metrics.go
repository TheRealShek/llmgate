// Package metrics owns process-local Prometheus instrumentation for the public gateway.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/therealshek/llmgate/internal/requestlog"
)

// Options bounds authenticated key labels and simultaneous scrape work.
type Options struct {
	KeyCapacity   int
	MaxScrapes    int
	ScrapeTimeout time.Duration
}

// Metrics owns one private registry. Startup shares it with the public handler.
type Metrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	ttft          prometheus.Histogram
	active        prometheus.Gauge
	scrape        http.Handler
	scrapeTimeout time.Duration
	mu            sync.Mutex
	keys          map[int64]string
	keyCapacity   int
}

// New creates the four Phase 1 metrics without global registration or background work.
// Startup validates all limits before construction.
func New(options Options) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(), keys: make(map[int64]string),
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
	m.ttft = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:        "gateway_ttft_seconds",
		Help:        "Arrival to first complete nonterminal SSE data event accepted by the response writer.",
		ConstLabels: prometheus.Labels{"backend": "primary"},
		Buckets:     []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
	})
	m.active = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "gateway_active_streams",
		Help:        "Admitted streaming inference attempts currently proxying, including prefill.",
		ConstLabels: prometheus.Labels{"backend": "primary"},
	})
	m.registry.MustRegister(m.requests, m.duration, m.ttft, m.active)
	m.requests.WithLabelValues("none", "none", "200")
	m.duration.WithLabelValues("none")
	m.duration.WithLabelValues("primary")
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
	backend bool
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
			if state.backend {
				backend = "primary"
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

// Proxy marks a selected model-list backend after authentication and rate admission.
func Proxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if state, ok := r.Context().Value(contextKey{}).(*request); ok {
			state.backend = true
		}

		next.ServeHTTP(w, r)
	})
}

// BeginInference marks backend selection and returns cleanup for the proxy lifetime.
// Cleanup runs before quota settlement and usage writes, including panic unwinding.
func BeginInference(ctx context.Context, streaming bool) func() {
	state, ok := ctx.Value(contextKey{}).(*request)
	if !ok {
		return func() {}
	}

	state.backend = true

	if !streaming {
		return func() {}
	}

	state.metrics.active.Inc()

	return state.metrics.active.Dec
}

// FirstData records TTFT once, when the existing SSE observer accepts its first data event.
func FirstData(ctx context.Context, at time.Time) {
	if state, ok := ctx.Value(contextKey{}).(*request); ok {
		state.metrics.ttft.Observe(at.Sub(state.started).Seconds())
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
