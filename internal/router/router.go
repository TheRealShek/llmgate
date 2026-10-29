// Package router selects healthy backends and owns local assignment lifetimes.
package router

import (
	"context"
	"sync"
	"time"

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
	mu             sync.Mutex
	targets        []Target
	health         Health
	assigned       []int
	next           int
	circuits       []circuitState
	circuitOptions CircuitOptions
}

// New copies targets with validated, unique names. Health must be shared with readiness.
// The router creates no workers. Only Acquire callers may use its target queues.
func New(targets []Target, health Health, options CircuitOptions) *Router {
	if options.FailureThreshold <= 0 {
		options.FailureThreshold = 3
	}

	if options.Cooldown <= 0 {
		options.Cooldown = 10 * time.Second
	}

	return &Router{
		targets: append([]Target(nil), targets...), health: health,
		assigned: make([]int, len(targets)), circuits: make([]circuitState, len(targets)),
		circuitOptions: options,
	}
}

// Select chooses a healthy backend for model listing without taking inference capacity.
func (r *Router) Select() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	index, err := r.choose(false, "")
	if err != nil {
		return "", err
	}

	return r.targets[index].Name, nil
}

// Acquire assigns a request before waiting on its queue, so concurrent selections
// see the assignment immediately. Excluded is a previously failed backend name,
// or empty on the first attempt. Callers must defer Assignment.Release and
// report inference outcomes before release.
func (r *Router) Acquire(ctx context.Context, excluded string) (string, *Assignment, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}

	r.mu.Lock()

	index, err := r.choose(true, excluded)
	if err != nil {
		r.mu.Unlock()
		return "", nil, err
	}

	r.assigned[index]++

	assignment := &Assignment{router: r, index: index, generation: r.circuits[index].generation}
	if !r.circuits[index].openedUntil.IsZero() {
		r.circuits[index].trial = true
	}
	r.mu.Unlock()

	release, err := r.targets[index].Admission.Acquire(ctx)
	if err != nil {
		assignment.Release()
		return r.targets[index].Name, nil, err
	}

	r.mu.Lock()
	assignment.releaseSlot = release
	stale := assignment.generation != r.circuits[index].generation
	r.mu.Unlock()

	if stale {
		assignment.Release()
		return r.targets[index].Name, nil, ErrNoBackend
	}

	return r.targets[index].Name, assignment, nil
}

func (r *Router) choose(bounded bool, excluded string) (int, error) {
	if r.health == nil {
		return -1, ErrNoBackend
	}

	states := r.health.States()
	selected := -1
	healthy := false

	for offset := range r.targets {
		index := (r.next + offset) % len(r.targets)

		target := r.targets[index]
		if target.Name == excluded {
			continue
		}

		available := false

		for _, state := range states {
			if state.Name == target.Name && state.Healthy {
				available = true
				break
			}
		}

		if !available || !r.circuits[index].available(time.Now(), bounded) {
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
