package metrics_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/queue"
	"github.com/therealshek/llmgate/internal/requestlog"
)

func newMetrics() *metrics.Metrics {
	return metrics.New(metrics.Options{Backends: map[string]*queue.Queue{"primary": nil}, KeyCapacity: 2, MaxScrapes: 2, ScrapeTimeout: time.Second})
}

func scrape(t *testing.T, m *metrics.Metrics) map[string]*dto.MetricFamily {
	t.Helper()

	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)

	result, err := parser.TextToMetricFamilies(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func instrument(m *metrics.Metrics, next http.Handler) http.Handler {
	return requestlog.NewHandler(m.Observe(next), slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func count(family *dto.MetricFamily, key, backend, status string) float64 {
	for _, sample := range family.GetMetric() {
		labels := map[string]string{}
		for _, label := range sample.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}

		if labels["key"] == key && labels["backend"] == backend && labels["status"] == status {
			return sample.GetCounter().GetValue()
		}
	}

	return 0
}

// TestRequestMetrics checks key attribution, backend selection, HTTP status, and scrape exclusion.
func TestRequestMetrics(t *testing.T) {
	m := newMetrics()
	handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			m.ServeHTTP(w, r)
			return
		}

		metrics.Authenticate(r.Context(), 7)
		metrics.SelectBackend(r.Context(), "primary")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/private-path?secret=query", nil))

	for range 2 {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	}

	data := scrape(t, m)
	if count(data["gateway_requests_total"], "7", "primary", "429") != 1 {
		t.Fatal("wrong request labels", data["gateway_requests_total"])
	}

	if len(data["gateway_requests_total"].GetMetric()) != 2 {
		t.Fatal("scrapes changed request counter")
	}

	for _, sample := range data["gateway_request_duration_seconds"].GetMetric() {
		if sample.GetLabel()[0].GetValue() == "primary" && sample.GetHistogram().GetSampleCount() != 1 {
			t.Fatal("request duration not recorded")
		}
	}
}

// TestKeyLabelBound checks concurrent distinct identities cannot grow the label map without limit.
func TestKeyLabelBound(t *testing.T) {
	m := newMetrics()
	handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.Header.Get("X-Test-ID"), 10, 64)
		if err != nil {
			t.Error(err)
			return
		}

		metrics.Authenticate(r.Context(), id)
		w.WriteHeader(http.StatusNoContent)
	}))

	var workers sync.WaitGroup
	for id := range 40 {
		workers.Go(func() {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("X-Test-ID", strconv.Itoa(id+1))
			handler.ServeHTTP(httptest.NewRecorder(), r)
		})
	}

	workers.Wait()

	data := scrape(t, m)
	if len(data["gateway_requests_total"].GetMetric()) != 4 || count(data["gateway_requests_total"], "other", "none", "204") != 38 {
		t.Fatal("key cardinality exceeded capacity")
	}
}

// TestZeroKeyCapacity checks authenticated keys share one label while unauthenticated requests stay distinct.
func TestZeroKeyCapacity(t *testing.T) {
	m := metrics.New(metrics.Options{Backends: map[string]*queue.Queue{"primary": nil}, KeyCapacity: 0, MaxScrapes: 2, ScrapeTimeout: time.Second})
	handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.Header.Get("X-Test-ID"), 10, 64)
		metrics.Authenticate(r.Context(), id)
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, id := range []string{"1", "2", ""} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Test-ID", id)
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}

	data := scrape(t, m)["gateway_requests_total"]
	if len(data.GetMetric()) != 3 || count(data, "other", "none", "204") != 2 ||
		count(data, "none", "none", "204") != 1 {
		t.Fatal("zero capacity did not aggregate authenticated keys", data)
	}
}

// TestInferenceMeasurements checks arrival-based TTFT and stream cleanup before accounting waits.
func TestInferenceMeasurements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMetrics()
		handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)

			metrics.SelectBackend(r.Context(), "primary")
			finish := metrics.BeginInference(r.Context(), true)

			if got := scrape(t, m)["gateway_active_streams"].GetMetric()[0].GetGauge().GetValue(); got != 1 {
				t.Fatal(got)
			}

			time.Sleep(time.Second)
			metrics.FirstData(r.Context(), time.Now())
			finish()

			if got := scrape(t, m)["gateway_active_streams"].GetMetric()[0].GetGauge().GetValue(); got != 0 {
				t.Fatal(got)
			}

			time.Sleep(4 * time.Second)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

		data := scrape(t, m)

		ttft := data["gateway_ttft_seconds"].GetMetric()[0].GetHistogram()
		if ttft.GetSampleCount() != 1 || ttft.GetSampleSum() != 3 {
			t.Fatal("TTFT excluded preflight", ttft)
		}

		for _, sample := range data["gateway_request_duration_seconds"].GetMetric() {
			if sample.GetLabel()[0].GetValue() == "primary" && sample.GetHistogram().GetSampleSum() != 7 {
				t.Fatal("duration excluded accounting", sample)
			}
		}
	})
}

// TestAdmissionMeasurements checks exact elapsed time and fixed rejection labels across outcomes.
func TestAdmissionMeasurements(t *testing.T) {
	m := newMetrics()
	for _, err := range []error{nil, fmt.Errorf("acquire: %w", queue.ErrFull), queue.ErrTimeout,
		context.Canceled, context.DeadlineExceeded, queue.ErrUnavailable} {
		handler := instrument(m, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			metrics.Admission(r.Context(), "primary", 2*time.Second, err)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	}

	data := scrape(t, m)
	wait := data["gateway_queue_wait_seconds"].GetMetric()[0].GetHistogram()

	if wait.GetSampleCount() != 6 || wait.GetSampleSum() != 12 {
		t.Fatal("incorrect acquisition timing", wait)
	}

	rejections := data["gateway_rejected_total"].GetMetric()
	if len(rejections) != 3 {
		t.Fatal("unexpected rejection labels", rejections)
	}

	for _, sample := range rejections {
		want := float64(1)
		if sample.GetLabel()[0].GetValue() == "no_backend" {
			want = 0
		}

		if sample.GetCounter().GetValue() != want {
			t.Fatal("cancellation or unavailable admission counted as overload", sample)
		}
	}

	if data["gateway_queue_depth"] != nil {
		t.Fatal("missing queue exported a depth gauge")
	}

	metrics.Admission(context.Background(), "primary", time.Second, queue.ErrFull)

	if got := scrape(t, m)["gateway_queue_wait_seconds"].GetMetric()[0].GetHistogram().GetSampleCount(); got != 6 {
		t.Fatal("uninstrumented request changed metrics", got)
	}
}

// TestResponseControl checks flush, first final status, and abort panic propagation.
func TestResponseControl(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(strconv.FormatBool(abort), func(t *testing.T) {
			m := newMetrics()
			handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metrics.SelectBackend(r.Context(), "primary")

				finish := metrics.BeginInference(r.Context(), true)
				defer finish()

				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusAccepted)

				_, _ = io.WriteString(w, "data: {}\n\n")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}

				if abort {
					panic(http.ErrAbortHandler)
				}
			}))
			w := httptest.NewRecorder()

			func() {
				defer func() {
					if got := recover(); (abort && got != http.ErrAbortHandler) || (!abort && got != nil) {
						t.Errorf("panic changed: %v", got)
					}
				}()

				handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))
			}()

			data := scrape(t, m)
			if !w.Flushed || count(data["gateway_requests_total"], "none", "primary", "202") != 1 || data["gateway_active_streams"].GetMetric()[0].GetGauge().GetValue() != 0 {
				t.Fatal("response or cleanup changed")
			}
		})
	}
}

// TestCanceledBeforeHeaders checks cancellation is counted without inventing an HTTP success.
func TestCanceledBeforeHeaders(t *testing.T) {
	m := newMetrics()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	instrument(m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))

	if count(scrape(t, m)["gateway_requests_total"], "none", "none", "none") != 1 {
		t.Fatal("cancellation disappeared")
	}
}

// TestPrivateRegistry checks independent gateway instances do not share counters or expose request payloads.
func TestPrivateRegistry(t *testing.T) {
	first, second := newMetrics(), newMetrics()
	r := httptest.NewRequest(http.MethodGet, "/private-prompt?token=private-secret", strings.NewReader("private-prompt"))
	r.Header.Set("Authorization", "Bearer private-key")
	instrument(first, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) })).ServeHTTP(httptest.NewRecorder(), r)
	w := httptest.NewRecorder()
	second.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))

	if bytes.Contains(w.Body.Bytes(), []byte("private-")) || count(scrape(t, second)["gateway_requests_total"], "none", "none", "201") != 0 {
		t.Fatal("registry leaked state or private data")
	}
}

type plainWriter struct{ http.ResponseWriter }

// TestUnsupportedFlush checks failed response control cannot replace a later HTTP error status.
func TestUnsupportedFlush(t *testing.T) {
	m := newMetrics()
	handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err == nil {
			t.Error("unsupported flush succeeded")
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	handler.ServeHTTP(plainWriter{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))

	if count(scrape(t, m)["gateway_requests_total"], "none", "none", "400") != 1 {
		t.Fatal("failed flush invented success")
	}
}

// TestMatchedRouteLogging checks adding metric context preserves the outer logger's route pattern.
func TestMatchedRouteLogging(t *testing.T) {
	m := newMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })

	var logs bytes.Buffer

	handler := requestlog.NewHandler(m.Observe(mux), slog.New(slog.NewJSONHandler(&logs, nil)))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

	if !strings.Contains(logs.String(), `"route":"GET /v1/healthz"`) {
		t.Fatal("metric wrapper hid matched route", logs.String())
	}
}

type deadlineWriter struct {
	http.ResponseWriter
	deadlines []time.Time
}

func (w *deadlineWriter) SetWriteDeadline(at time.Time) error {
	w.deadlines = append(w.deadlines, at)
	return nil
}

// TestScrapeWriteDeadline checks only the scrape response gains a temporary delivery deadline.
func TestScrapeWriteDeadline(t *testing.T) {
	m := newMetrics()
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))

	if len(w.deadlines) != 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
		t.Fatal("scrape deadline not cleared", w.deadlines)
	}
}

// TestBackendMetricIsolation checks configured labels stay separate and unknown selections cannot create series.
func TestBackendMetricIsolation(t *testing.T) {
	m := metrics.New(metrics.Options{
		Backends: map[string]*queue.Queue{"a": nil, "b": nil}, MaxScrapes: 2, ScrapeTimeout: time.Second,
	})
	for _, name := range []string{"a", "b", "untrusted-name"} {
		handler := instrument(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metrics.SelectBackend(r.Context(), name)

			finish := metrics.BeginInference(r.Context(), true)
			defer finish()

			metrics.FirstData(r.Context(), time.Now())
			w.WriteHeader(http.StatusOK)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	}

	data := scrape(t, m)
	for _, family := range []string{"gateway_active_streams", "gateway_ttft_seconds", "gateway_queue_wait_seconds"} {
		samples := data[family].GetMetric()
		if len(samples) != 2 {
			t.Fatal("unexpected backend cardinality", family, samples)
		}

		for _, sample := range samples {
			if label := sample.GetLabel()[0].GetValue(); label != "a" && label != "b" {
				t.Fatal("unconfigured label", sample)
			}

			if family == "gateway_ttft_seconds" && sample.GetHistogram().GetSampleCount() != 1 {
				t.Fatal("TTFT crossed backends", sample)
			}

			if family == "gateway_active_streams" && sample.GetGauge().GetValue() != 0 {
				t.Fatal("stream gauge leaked", sample)
			}
		}
	}

	if count(data["gateway_requests_total"], "none", "a", "200") != 1 ||
		count(data["gateway_requests_total"], "none", "b", "200") != 1 ||
		count(data["gateway_requests_total"], "none", "none", "200") != 1 {
		t.Fatal("request attribution crossed backends", data["gateway_requests_total"])
	}
}
