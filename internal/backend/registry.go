// Package backend monitors inference server health independently of client requests.
package backend

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Target identifies a configured inference backend. Names must be unique.
type Target struct {
	Name string
	URL  *url.URL
}

// HealthOptions bounds probe work and defines consecutive-result thresholds.
type HealthOptions struct {
	Interval  time.Duration
	Timeout   time.Duration
	Failures  int
	Successes int
}

// State is a copy of one backend's current health, initially unhealthy.
type State struct {
	Name    string
	Healthy bool
}

type entry struct {
	state     State
	healthURL string
	successes int
	failures  int
}

// Registry owns health state shared by probe workers and readiness callers.
// Configuration is immutable; the mutex protects health and result streaks.
type Registry struct {
	mu      sync.RWMutex
	entries []entry
	client  *http.Client
	options HealthOptions
}

// NewRegistry copies validated targets so caller URL changes cannot redirect probes.
// The caller owns transport and must run and join Run once for this registry.
func NewRegistry(targets []Target, transport http.RoundTripper, options HealthOptions) (*Registry, error) {
	if options.Interval <= 0 || options.Timeout <= 0 || options.Failures <= 0 || options.Successes <= 0 {
		return nil, fmt.Errorf("configure backend health: durations and thresholds must be positive")
	}

	registry := &Registry{
		options: options,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	names := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target.Name == "" || names[target.Name] {
			return nil, fmt.Errorf("configure backend health: names must be nonempty and unique")
		}

		if target.URL == nil {
			return nil, fmt.Errorf("configure backend health: requires a backend URL")
		}

		address := target.URL
		if (address.Scheme != "http" && address.Scheme != "https") || address.Hostname() == "" ||
			address.User != nil || address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
			return nil, fmt.Errorf("configure backend health: requires HTTP(S) URLs without credentials, query, or fragment")
		}

		names[target.Name] = true
		registry.entries = append(registry.entries, entry{
			state:     State{Name: target.Name},
			healthURL: target.URL.JoinPath("health").String(),
		})
	}

	return registry, nil
}

// States returns independent snapshots in configuration order.
func (r *Registry) States() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	states := make([]State, len(r.entries))
	for i := range r.entries {
		states[i] = r.entries[i].state
	}

	return states
}

// Run probes immediately, then periodically, with one worker per configured backend.
// Slow probes never overlap for the same backend or delay another backend's probes.
// Cancellation stops in-flight HTTP work and joins every worker before returning.
func (r *Registry) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for i := range r.entries {
		workers.Go(func() { r.monitor(ctx, i) })
	}

	workers.Wait()
}

func (r *Registry) monitor(ctx context.Context, index int) {
	ticker := time.NewTicker(r.options.Interval)
	defer ticker.Stop()

	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()

		r.entries[index].state.Healthy = false
		r.entries[index].successes = 0
		r.entries[index].failures = 0
	}()

	for ctx.Err() == nil {
		healthy := r.probe(ctx, r.entries[index].healthURL)
		if ctx.Err() != nil {
			return
		}

		r.record(index, healthy)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Registry) probe(ctx context.Context, healthURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, r.options.Timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}

	response, err := r.client.Do(request)
	if err != nil {
		return false
	}

	// Health depends only on headers. Never buffer or wait for an arbitrary body.
	_ = response.Body.Close()

	return response.StatusCode == http.StatusOK
}

func (r *Registry) record(index int, healthy bool) {
	r.mu.Lock()

	backend := &r.entries[index]
	previous := backend.state.Healthy

	if healthy {
		backend.failures = 0
		if backend.successes < r.options.Successes {
			backend.successes++
		}

		if backend.successes >= r.options.Successes {
			backend.state.Healthy = true
		}
	} else {
		backend.successes = 0
		if backend.failures < r.options.Failures {
			backend.failures++
		}

		if backend.failures >= r.options.Failures {
			backend.state.Healthy = false
		}
	}

	state := backend.state
	r.mu.Unlock()

	if previous != state.Healthy {
		slog.Info("backend health changed", "backend", state.Name, "healthy", state.Healthy)
	}
}
