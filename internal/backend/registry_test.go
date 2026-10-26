package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegistryTransitions checks consecutive thresholds, streak resets, recovery, and snapshot isolation.
func TestRegistryTransitions(t *testing.T) {
	registry := testRegistry(t, "http://localhost", HealthOptions{
		Interval: time.Second, Timeout: time.Second, Failures: 3, Successes: 2,
	})

	results := []struct{ success, healthy bool }{
		{true, false}, {false, false}, {true, false}, {true, true},
		{false, true}, {false, true}, {true, true},
		{false, true}, {false, true}, {false, false},
		{true, false}, {false, false}, {true, false}, {true, true},
	}
	for i, result := range results {
		registry.record(0, result.success)

		states := registry.States()
		if states[0].Healthy != result.healthy {
			t.Fatalf("result %d: healthy = %v, want %v", i, states[0].Healthy, result.healthy)
		}

		states[0].Name = "changed"
		if registry.States()[0].Name != "primary" {
			t.Fatal("snapshot changed registry")
		}
	}
}

// TestRegistryProbe checks HTTP status, base paths, redirect isolation, and early body closure.
func TestRegistryProbe(t *testing.T) {
	for _, status := range []int{200, 302, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)

				if r.URL.Path != "/api/health" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected health request: %s", r.URL)
				}

				w.Header().Set("Location", "/redirected")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()

			registry := testRegistry(t, upstream.URL+"/api", HealthOptions{
				Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1,
			})
			if got := registry.probe(t.Context(), registry.entries[0].healthURL); got != (status == 200) {
				t.Fatalf("healthy = %v for status %d", got, status)
			}

			if calls.Load() != 1 {
				t.Fatal("followed redirect")
			}
		})
	}
}

// TestRegistryWorkers checks independent progress, bounded probes, deadlines, and joined cancellation.
func TestRegistryWorkers(t *testing.T) {
	transport := &healthTransport{}
	slowURL, _ := url.Parse("http://slow")
	fastURL, _ := url.Parse("http://fast")

	registry, err := NewRegistry([]Target{{Name: "slow", URL: slowURL}, {Name: "fast", URL: fastURL}}, transport,
		HealthOptions{Interval: 5 * time.Millisecond, Timeout: 30 * time.Millisecond, Failures: 1, Successes: 1})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)

		registry.Run(ctx)
	}()

	defer func() {
		cancel()
		<-done
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()

	for !registry.States()[1].Healthy || transport.calls.Load() < 2 {
		select {
		case <-deadline.C:
			t.Fatal("healthy backend or timed-out probe made no progress")
		case <-poll.C:
		}
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not join canceled probes")
	}

	for _, state := range registry.States() {
		if state.Healthy {
			t.Fatal("stopped registry retained healthy state")
		}
	}

	if transport.peak.Load() != 1 || transport.active.Load() != 0 {
		t.Fatalf("probe peak = %d, active after shutdown = %d", transport.peak.Load(), transport.active.Load())
	}
}

// TestRegistryValidation checks unsafe targets and invalid worker settings fail at construction.
func TestRegistryValidation(t *testing.T) {
	valid, _ := url.Parse("http://localhost/api")
	options := HealthOptions{Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1}

	for _, raw := range []string{"", "ftp://localhost", "http://user:secret@localhost", "http://localhost?x=1", "http://localhost?", "http://localhost/#x"} {
		target, _ := url.Parse(raw)
		if _, err := NewRegistry([]Target{{Name: "primary", URL: target}}, nil, options); err == nil {
			t.Errorf("accepted invalid target %q", raw)
		}
	}

	for _, targets := range [][]Target{
		{{Name: "primary"}}, {{URL: valid}}, {{Name: "same", URL: valid}, {Name: "same", URL: valid}},
	} {
		if _, err := NewRegistry(targets, nil, options); err == nil {
			t.Error("accepted missing URL/name or duplicate name")
		}
	}

	for _, invalid := range []HealthOptions{
		{Timeout: time.Second, Failures: 1, Successes: 1},
		{Interval: time.Second, Failures: 1, Successes: 1},
		{Interval: time.Second, Timeout: time.Second, Successes: 1},
		{Interval: time.Second, Timeout: time.Second, Failures: 1},
	} {
		if _, err := NewRegistry(nil, nil, invalid); err == nil {
			t.Error("accepted zero health setting")
		}
	}
}

// TestRegistryCopiesURL checks caller mutation cannot redirect a constructed registry.
func TestRegistryCopiesURL(t *testing.T) {
	target, _ := url.Parse("http://localhost/api")

	registry, err := NewRegistry([]Target{{Name: "primary", URL: target}}, nil,
		HealthOptions{Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1})
	if err != nil {
		t.Fatal(err)
	}

	target.Host = "changed"

	if registry.entries[0].healthURL != "http://localhost/api/health" {
		t.Fatal("target mutation changed health URL")
	}
}

// TestRegistryConnectionFailure checks transport failures remain unhealthy without exposing diagnostics.
func TestRegistryConnectionFailure(t *testing.T) {
	registry := testRegistry(t, "http://localhost", HealthOptions{
		Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1,
	})

	registry.client.Transport = failingTransport{}
	if registry.probe(t.Context(), registry.entries[0].healthURL) {
		t.Fatal("transport failure reported healthy")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func testRegistry(t *testing.T, raw string, options HealthOptions) *Registry {
	t.Helper()

	target, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := NewRegistry([]Target{{Name: "primary", URL: target}}, nil, options)
	if err != nil {
		t.Fatal(err)
	}

	return registry
}

// TestRegistryEmpty checks no configured targets means no workers and no healthy states.
func TestRegistryEmpty(t *testing.T) {
	registry, err := NewRegistry(nil, nil, HealthOptions{
		Interval: time.Second, Timeout: time.Second, Failures: 1, Successes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	registry.Run(t.Context())

	if len(registry.States()) != 0 {
		t.Fatal("empty registry returned backends")
	}
}

type healthTransport struct {
	active atomic.Int32
	peak   atomic.Int32
	calls  atomic.Int32
}

func (h *healthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "fast" {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
	}

	current := h.active.Add(1)
	defer h.active.Add(-1)

	for previous := h.peak.Load(); current > previous; previous = h.peak.Load() {
		if h.peak.CompareAndSwap(previous, current) {
			break
		}
	}

	h.calls.Add(1)
	<-r.Context().Done()

	return nil, r.Context().Err()
}
