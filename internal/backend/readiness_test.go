package backend_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/requestlog"
)

// TestReadinessStatus checks backend health statuses, base paths, identity, and isolation from
// client inputs.
func TestReadinessStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusOK,
		http.StatusServiceUnavailable,
		http.StatusInternalServerError,
		http.StatusNoContent,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstreamIDs := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/health" {
					t.Errorf("unexpected probe: %s %s", r.Method, r.URL.RequestURI())
				}

				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.ContentLength != 0 {
					t.Error("probe forwarded client credentials or body")
				}

				upstreamIDs <- r.Header.Get("X-Request-ID")

				w.Header().Set("X-Request-ID", "backend-secret")
				w.Header().Set("Set-Cookie", "backend-secret")
				w.WriteHeader(status)

				if status != http.StatusNoContent {
					_, _ = io.WriteString(w, "backend-secret")
				}
			}))
			t.Cleanup(server.Close)

			target, err := url.Parse(server.URL + "/api/")
			if err != nil {
				t.Fatal(err)
			}

			transport := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(transport.CloseIdleConnections)
			readiness := backend.NewReadinessHandler(target, transport, time.Second)
			routes := gateway.NewHandler(gateway.HandlerOptions{
				Readiness: readiness,
			})
			handler := requestlog.NewHandler(routes, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			request := httptest.NewRequest(
				http.MethodGet,
				"/v1/readyz?token=client-secret",
				strings.NewReader("client-secret"),
			)
			request.Header.Set("Authorization", "Bearer client-secret")
			request.Header.Set("Cookie", "client-secret")

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			want := http.StatusServiceUnavailable
			if status == http.StatusOK {
				want = http.StatusOK
			}

			if response.Code != want {
				t.Fatalf("readiness status = %d, want %d", response.Code, want)
			}

			upstreamID := <-upstreamIDs
			if upstreamID == "" || upstreamID != response.Header().Get("X-Request-ID") {
				t.Fatalf(
					"probe and client IDs differ: %q %q",
					upstreamID,
					response.Header().Get("X-Request-ID"),
				)
			}

			if response.Header().Get("Set-Cookie") != "" ||
				strings.Contains(response.Body.String(), "secret") {
				t.Fatal("readiness forwarded backend headers or body")
			}

			if want == http.StatusOK && response.Body.String() != "ok\n" {
				t.Fatalf("healthy response = %q", response.Body.String())
			}

			if want == http.StatusServiceUnavailable &&
				!strings.Contains(response.Body.String(), `"code":"backend_not_ready"`) {
				t.Fatalf("missing readiness error code: %s", response.Body.String())
			}

			health := httptest.NewRecorder()
			handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))

			if health.Code != http.StatusOK {
				t.Fatal("backend health affected gateway liveness")
			}
		})
	}
}

// TestReadinessCleanup checks probe deadlines, client cancellation, and early body closure
// release backend work.
func TestReadinessCleanup(t *testing.T) {
	for _, mode := range []string{"deadline", "client cancellation", "stalled response body"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)

				if mode == "stalled response body" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}

				<-r.Context().Done()
				close(stopped)
			}))
			t.Cleanup(server.Close)

			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			transport := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(transport.CloseIdleConnections)

			timeout := time.Second
			if mode == "deadline" {
				timeout = 50 * time.Millisecond
			}

			handler := backend.NewReadinessHandler(target, transport, timeout)
			ctx, cancel := context.WithCancel(t.Context())

			var workers sync.WaitGroup

			t.Cleanup(func() {
				cancel()
				workers.Wait()
			})

			done := make(chan struct{})
			response := httptest.NewRecorder()

			workers.Go(func() {
				handler.ServeHTTP(
					response,
					httptest.NewRequest(http.MethodGet, "/v1/readyz", nil).WithContext(ctx),
				)
				close(done)
			})

			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not reach backend")
			}

			if mode == "client cancellation" {
				cancel()
			}

			for _, event := range []<-chan struct{}{done, stopped} {
				select {
				case <-event:
				case <-time.After(5 * time.Second):
					t.Fatal("probe did not finish or release backend work")
				}
			}

			want := http.StatusServiceUnavailable
			if mode == "stalled response body" {
				want = http.StatusOK
			}

			if response.Code != want {
				t.Fatalf("readiness status = %d, want %d", response.Code, want)
			}
		})
	}
}

// TestReadinessRedirect checks an alternate server cannot make an unhealthy configured backend
// appear ready.
func TestReadinessRedirect(t *testing.T) {
	var followed atomic.Int32

	alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(alternate.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, alternate.URL, http.StatusFound)
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	response := httptest.NewRecorder()
	backend.NewReadinessHandler(
		target,
		transport,
		time.Second,
	).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

	if response.Code != http.StatusServiceUnavailable || followed.Load() != 0 {
		t.Fatalf("redirect status = %d, alternate requests = %d", response.Code, followed.Load())
	}
}

// TestReadinessConnectionFailure checks an unreachable backend produces a fixed 503 without
// exposing its URL.
func TestReadinessConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	server.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	response := httptest.NewRecorder()
	backend.NewReadinessHandler(
		target,
		transport,
		time.Second,
	).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

	if response.Code != http.StatusServiceUnavailable ||
		strings.Contains(response.Body.String(), server.URL) {
		t.Fatalf("unexpected connection failure response: %d %s", response.Code, response.Body.String())
	}
}

// TestReadinessRecovery checks concurrent probes observe backend recovery and later failure
// without cached state.
func TestReadinessRecovery(t *testing.T) {
	var (
		status atomic.Int32
		probes atomic.Int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	handler := backend.NewReadinessHandler(target, transport, time.Second)

	for _, expected := range []int{
		http.StatusServiceUnavailable,
		http.StatusOK,
		http.StatusServiceUnavailable,
	} {
		status.Store(int32(expected))

		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

				if response.Code != expected {
					t.Errorf("readiness returned stale status %d, want %d", response.Code, expected)
				}
			})
		}

		workers.Wait()
	}

	if probes.Load() != 24 {
		t.Fatalf("backend received %d probes, want 24", probes.Load())
	}
}
