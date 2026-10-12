package backend_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/backend"
)

// TestReadinessRecovery checks concurrent probes observe backend recovery and later failure without cached state.
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

	for _, expected := range []int{http.StatusServiceUnavailable, http.StatusOK, http.StatusServiceUnavailable} {
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
