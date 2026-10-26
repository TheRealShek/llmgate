// Package queue bounds active inference and waiting requests for one backend.
package queue

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"
)

// Options defines local backend capacity. Capacity may be zero to disable waiting.
type Options struct {
	Slots       int
	Capacity    int
	WaitTimeout time.Duration
}

// State is a snapshot of occupied slots and waiting requests.
type State struct {
	Active  int
	Waiting int
}

type waiter struct {
	ready   chan struct{}
	granted bool
}

// Queue owns a FIFO waiting list and slot count, protected by one mutex.
// It creates no worker goroutines and retains no request bodies or contexts.
type Queue struct {
	mu      sync.Mutex
	options Options
	active  int
	waiting list.List
}

// New validates capacity and constructs an empty queue for one backend.
func New(options Options) (*Queue, error) {
	if options.Slots <= 0 || options.Capacity < 0 || options.WaitTimeout <= 0 {
		return nil, fmt.Errorf("configure inference queue: positive slots and timeout, and nonnegative capacity required")
	}

	return &Queue{options: options}, nil
}

// State returns the current local load without exposing mutable queue state.
func (q *Queue) State() State {
	q.mu.Lock()
	defer q.mu.Unlock()

	return State{Active: q.active, Waiting: q.waiting.Len()}
}

// Acquire waits for a slot or rejects when full, timed out, or canceled.
// Successful callers own an idempotent release function and must defer it.
func (q *Queue) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if q == nil {
		return nil, ErrUnavailable
	}

	q.mu.Lock()
	if q.active < q.options.Slots {
		q.active++
		q.mu.Unlock()

		return sync.OnceFunc(q.release), nil
	}

	if q.waiting.Len() >= q.options.Capacity {
		q.mu.Unlock()

		return nil, ErrFull
	}

	pending := &waiter{ready: make(chan struct{})}
	element := q.waiting.PushBack(pending)
	deadline := time.Now().Add(q.options.WaitTimeout)
	q.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	var err error

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = ErrTimeout
	case <-pending.ready:
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// A grant can race cancellation or expiry. Return its slot before rejecting,
	// so a disconnected or expired waiter cannot consume backend capacity.
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if !time.Now().Before(deadline) {
		err = ErrTimeout
	}

	if err != nil {
		if pending.granted {
			q.releaseLocked()
		} else {
			q.waiting.Remove(element)
		}

		return nil, err
	}

	return sync.OnceFunc(q.release), nil
}

func (q *Queue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.releaseLocked()
}

func (q *Queue) releaseLocked() {
	q.active--
	if next := q.waiting.Front(); next != nil {
		pending := q.waiting.Remove(next).(*waiter)
		pending.granted = true
		q.active++

		close(pending.ready)
	}
}
