package backend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/gateway"
)

// TestReadinessSnapshot checks initial failure, any-healthy semantics, and concurrent reads without probes.
func TestReadinessSnapshot(t *testing.T) {
	target, _ := url.Parse("http://localhost")

	registry, err := NewRegistry([]Target{{Name: "a", URL: target}, {Name: "b", URL: target}}, failingTransport{},
		HealthOptions{Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1})
	if err != nil {
		t.Fatal(err)
	}

	handler := gateway.NewHandler(gateway.HandlerOptions{Readiness: NewReadinessHandler(registry)})
	for _, ready := range []bool{false, true, false} {
		registry.record(1, ready)

		var readers sync.WaitGroup
		for range 10 {
			readers.Go(func() {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "/v1/readyz?private=value", nil)
				request.Header.Set("Authorization", "Bearer secret")
				handler.ServeHTTP(response, request)

				if ready {
					if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
						t.Errorf("ready response = %d %q", response.Code, response.Body.String())
					}
				} else if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "backend_not_ready") {
					t.Errorf("unready response = %d %q", response.Code, response.Body.String())
				}
			})
		}

		readers.Wait()
	}
}

// TestReadinessEmpty checks an empty registry cannot report readiness.
func TestReadinessEmpty(t *testing.T) {
	registry, err := NewRegistry(nil, nil, HealthOptions{
		Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewReadinessHandler(registry).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("empty registry reported ready")
	}
}
