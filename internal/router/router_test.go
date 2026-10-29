package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/queue"
)

type testHealth struct {
	mu     sync.Mutex
	states []backend.State
}

func (h *testHealth) States() []backend.State {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]backend.State(nil), h.states...)
}
func (h *testHealth) set(name string, healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := range h.states {
		if h.states[i].Name == name {
			h.states[i].Healthy = healthy
		}
	}
}
func newTestRouter(t *testing.T, capacity int) (*Router, *testHealth) {
	t.Helper()

	targets := make([]Target, 0, 2)
	health := &testHealth{}

	for _, name := range []string{"a", "b"} {
		q, err := queue.New(queue.Options{Slots: 1, Capacity: capacity, WaitTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}

		targets = append(targets, Target{Name: name, Admission: q})
		health.states = append(health.states, backend.State{Name: name, Healthy: true})
	}

	return New(targets, health, CircuitOptions{}), health
}

// TestRoutingSelection checks least load, rotating ties, healthy exclusion, and model-list bypass.
func TestRoutingSelection(t *testing.T) {
	r, health := newTestRouter(t, 0)
	for _, want := range []string{"a", "b", "a", "b"} {
		name, assignment, err := r.Acquire(t.Context(), "")
		if err != nil || name != want {
			t.Fatalf("selection = %s, %v; want %s", name, err, want)
		}

		assignment.Release()
		assignment.Release()
	}

	name, assignmentA, err := r.Acquire(t.Context(), "")
	if err != nil || name != "a" {
		t.Fatal(name, err)
	}
	defer assignmentA.Release()

	name, assignmentB, err := r.Acquire(t.Context(), "")
	if err != nil || name != "b" {
		t.Fatal(name, err)
	}
	defer assignmentB.Release()

	if _, _, err := r.Acquire(t.Context(), ""); !errors.Is(err, queue.ErrFull) {
		t.Fatal("saturation", err)
	}

	if _, err := r.Select(); err != nil {
		t.Fatal("model listing consumed capacity", err)
	}

	assignmentA.Release()
	health.set("a", false)

	if _, _, err := r.Acquire(t.Context(), ""); !errors.Is(err, queue.ErrFull) {
		t.Fatal("unhealthy backend used", err)
	}

	health.set("b", false)

	if _, _, err := r.Acquire(t.Context(), ""); !errors.Is(err, ErrNoBackend) {
		t.Fatal("missing health", err)
	}

	health.set("a", true)

	name, assignment, err := r.Acquire(t.Context(), "")
	if err != nil || name != "a" {
		t.Fatal("recovery", name, err)
	}

	assignment.Release()
}

// TestRoutingWaitCleanup checks cancellation, expiry, and successful handoff clear assignments on either backend.
func TestRoutingWaitCleanup(t *testing.T) {
	for _, target := range []string{"a", "b"} {
		for _, outcome := range []string{"cancel", "timeout", "grant"} {
			t.Run(target+"/"+outcome, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					r, health := newTestRouter(t, 1)

					other := "a"
					if target == "a" {
						other = "b"
					}

					health.set(other, false)

					name, assignment, err := r.Acquire(t.Context(), "")
					if err != nil || name != target {
						t.Fatal(name, err)
					}
					defer assignment.Release()

					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()

					done := make(chan error, 1)

					go func() {
						_, nextAssignment, err := r.Acquire(ctx, "")
						if err == nil {
							nextAssignment.Release()
						}

						done <- err
					}()

					synctest.Wait()

					if _, _, err := r.Acquire(t.Context(), ""); !errors.Is(err, queue.ErrFull) {
						t.Fatal("waiting assignment unbounded", err)
					}

					switch outcome {
					case "cancel":
						cancel()
					case "timeout":
						time.Sleep(time.Second)
					case "grant":
						assignment.Release()
					}

					err = <-done
					if (outcome == "cancel" && !errors.Is(err, context.Canceled)) ||
						(outcome == "timeout" && !errors.Is(err, queue.ErrTimeout)) || (outcome == "grant" && err != nil) {
						t.Fatal(outcome, err)
					}

					assignment.Release()

					for i, entry := range r.targets {
						if r.assigned[i] != 0 || entry.Admission.State() != (queue.State{}) {
							t.Fatal("load leaked", entry.Name, r.assigned[i], entry.Admission.State())
						}
					}
				})
			})
		}
	}
}

// TestRoutingConcurrentBound checks concurrent selection fills both backends without exceeding capacity.
func TestRoutingConcurrentBound(t *testing.T) {
	r, _ := newTestRouter(t, 0)
	entered := make(chan string, 40)
	leave := make(chan struct{})

	var workers sync.WaitGroup
	for range 40 {
		workers.Go(func() {
			name, assignment, err := r.Acquire(t.Context(), "")
			if err != nil {
				if !errors.Is(err, queue.ErrFull) {
					t.Error(err)
				}

				entered <- ""

				return
			}

			entered <- name

			<-leave
			assignment.Release()
		})
	}

	counts := map[string]int{}
	for range 40 {
		counts[<-entered]++
	}

	close(leave)
	workers.Wait()

	if counts["a"] != 1 || counts["b"] != 1 || counts[""] != 38 {
		t.Fatal(counts)
	}

	for i, target := range r.targets {
		if r.assigned[i] != 0 || target.Admission.State() != (queue.State{}) {
			t.Fatal("concurrent cleanup leaked")
		}
	}
}

// TestRoutingUnavailable checks missing health, empty configuration, and canceled requests fail closed.
func TestRoutingUnavailable(t *testing.T) {
	r, _ := newTestRouter(t, 0)

	r.health = nil
	if _, _, err := r.Acquire(t.Context(), ""); !errors.Is(err, ErrNoBackend) {
		t.Fatal(err)
	}

	empty := New(nil, &testHealth{}, CircuitOptions{})
	if _, err := empty.Select(); !errors.Is(err, ErrNoBackend) {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, _, err := r.Acquire(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// TestRoutingExclusion checks a retry cannot return to its previously failed target.
func TestRoutingExclusion(t *testing.T) {
	r, health := newTestRouter(t, 0)

	name, assignment, err := r.Acquire(t.Context(), "a")
	if err != nil || name != "b" {
		t.Fatal(name, err)
	}

	assignment.Release()
	health.set("b", false)

	if _, _, err := r.Acquire(t.Context(), "a"); !errors.Is(err, ErrNoBackend) {
		t.Fatal("excluded target reused", err)
	}
}
