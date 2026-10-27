// Package router selects healthy backends and owns local assignment lifetimes.
package router

import (
	"context"
	"sync"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/queue"
)

// Health supplies independently monitored backend health snapshots.
type Health interface {
	States() []backend.State
}

// Target connects a configured backend name to its local inference queue.
type Target struct {
	Name      string
	Admission *queue.Queue
}

// Router protects assignment counts and tie rotation with one mutex.
// Counts include preparation, inference, and waiting, but exclude final accounting.
type Router struct {
	mu       sync.Mutex
	targets  []Target
	health   Health
	assigned []int
	next     int
}

// New copies targets with validated, unique names. Health must be shared with readiness.
// The router creates no workers. Only Acquire callers may use its target queues.
func New(targets []Target, health Health) *Router {
	return &Router{targets: append([]Target(nil), targets...), health: health, assigned: make([]int, len(targets))}
}

// Select chooses a healthy backend for model listing without taking inference capacity.
func (r *Router) Select() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	index, err := r.choose(false)
	if err != nil {
		return "", err
	}

	return r.targets[index].Name, nil
}

// Acquire assigns a request before waiting on its queue, so concurrent selections
// see the assignment immediately. Callers must defer the idempotent release.
func (r *Router) Acquire(ctx context.Context) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}

	r.mu.Lock()

	index, err := r.choose(true)
	if err != nil {
		r.mu.Unlock()
		return "", nil, err
	}

	r.assigned[index]++
	r.mu.Unlock()

	release, err := r.targets[index].Admission.Acquire(ctx)
	if err != nil {
		r.unassign(index)
		return r.targets[index].Name, nil, err
	}

	return r.targets[index].Name, sync.OnceFunc(func() {
		// Keep the selection lock through both releases so a new assignment cannot
		// observe a free router place while its queue still owns the old request.
		r.mu.Lock()
		defer r.mu.Unlock()

		release()

		r.assigned[index]--
	}), nil
}

func (r *Router) unassign(index int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.assigned[index]--
}

func (r *Router) choose(bounded bool) (int, error) {
	if r.health == nil {
		return -1, ErrNoBackend
	}

	states := r.health.States()
	selected := -1
	healthy := false

	for offset := range r.targets {
		index := (r.next + offset) % len(r.targets)
		target := r.targets[index]
		available := false

		for _, state := range states {
			if state.Name == target.Name && state.Healthy {
				available = true
				break
			}
		}

		if !available {
			continue
		}

		healthy = true

		if bounded && target.Admission != nil && r.assigned[index] >= target.Admission.Limit() {
			continue
		}

		if selected < 0 || r.assigned[index] < r.assigned[selected] {
			selected = index
		}
	}

	if selected < 0 {
		if healthy {
			return -1, queue.ErrFull
		}

		return -1, ErrNoBackend
	}

	r.next = (selected + 1) % len(r.targets)

	return selected, nil
}
