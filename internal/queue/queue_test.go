package queue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestQueueCapacity checks slot limits, full rejection, zero waiting, and idempotent release.
func TestQueueCapacity(t *testing.T) {
	for _, slots := range []int{1, 3} {
		q := newTestQueue(t, Options{Slots: slots, Capacity: 0, WaitTimeout: time.Second})

		releases := make([]func(), slots)
		for i := range releases {
			release, err := q.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			releases[i] = release
		}

		if _, err := q.Acquire(t.Context()); !errors.Is(err, ErrFull) {
			t.Fatalf("full queue error = %v", err)
		}

		for _, release := range releases {
			release()
			release()
		}

		if got := q.State(); got != (State{}) {
			t.Fatalf("released queue state = %+v", got)
		}
	}
}

// TestQueueFIFO checks bounded waiting and ordered slot transfer without new callers overtaking.
func TestQueueFIFO(t *testing.T) {
	q := newTestQueue(t, Options{Slots: 1, Capacity: 3, WaitTimeout: time.Second})

	release, err := q.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	pending := make([]<-chan acquireResult, 3)
	for i := range pending {
		pending[i] = acquireAsync(t, t.Context(), q)
		waitState(t, q, State{Active: 1, Waiting: i + 1})
	}

	if _, err := q.Acquire(t.Context()); !errors.Is(err, ErrFull) {
		t.Fatalf("full waiting queue error = %v", err)
	}

	release()

	for i, result := range pending {
		acquired := receive(t, result)
		if acquired.err != nil {
			t.Fatal(acquired.err)
		}

		if got := q.State(); got != (State{Active: 1, Waiting: len(pending) - i - 1}) {
			t.Fatalf("FIFO state = %+v", got)
		}

		acquired.release()
	}

	waitState(t, q, State{})
}

// TestQueueWaitFailures checks timeout and cancellation remove waiters and free waiting capacity.
func TestQueueWaitFailures(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		q := newTestQueue(t, Options{Slots: 1, Capacity: 1, WaitTimeout: 30 * time.Millisecond})

		release, err := q.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		pending := acquireAsync(t, ctx, q)
		waitState(t, q, State{Active: 1, Waiting: 1})

		if canceled {
			cancel()
		}

		result := receive(t, pending)

		cancel()

		want := ErrTimeout
		if canceled {
			want = context.Canceled
		}

		if !errors.Is(result.err, want) || result.release != nil {
			t.Fatalf("wait result = %v, want %v", result.err, want)
		}

		waitState(t, q, State{Active: 1})
		release()
		waitState(t, q, State{})
	}
}

// TestQueueCancelGrantRace checks cancellation racing slot transfer never leaks or doubles a slot.
func TestQueueCancelGrantRace(t *testing.T) {
	for range 100 {
		q := newTestQueue(t, Options{Slots: 1, Capacity: 1, WaitTimeout: time.Second})

		release, err := q.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		pending := acquireAsync(t, ctx, q)
		waitState(t, q, State{Active: 1, Waiting: 1})

		var workers sync.WaitGroup
		workers.Go(cancel)
		workers.Go(release)
		workers.Wait()

		result := receive(t, pending)
		if result.err == nil {
			result.release()
		} else if !errors.Is(result.err, context.Canceled) {
			t.Fatal(result.err)
		}

		waitState(t, q, State{})
	}
}

// TestQueueUnavailable checks absent configuration and already canceled calls consume no capacity.
func TestQueueUnavailable(t *testing.T) {
	var missing *Queue
	if _, err := missing.Acquire(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing queue error = %v", err)
	}

	q := newTestQueue(t, Options{Slots: 1, Capacity: 1, WaitTimeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := q.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v", err)
	}

	if q.State() != (State{}) {
		t.Fatal("canceled call consumed capacity")
	}
}

// TestQueueConfiguration checks unsupported capacity and timeout values fail construction.
func TestQueueConfiguration(t *testing.T) {
	for _, options := range []Options{
		{Slots: 0, Capacity: 1, WaitTimeout: time.Second},
		{Slots: -1, Capacity: 1, WaitTimeout: time.Second},
		{Slots: 1, Capacity: -1, WaitTimeout: time.Second},
		{Slots: 1, Capacity: 1, WaitTimeout: 0},
		{Slots: 1, Capacity: 1, WaitTimeout: -time.Second},
	} {
		if _, err := New(options); err == nil {
			t.Fatalf("accepted invalid options: %+v", options)
		}
	}
}

type acquireResult struct {
	release func()
	err     error
}

func newTestQueue(t *testing.T, options Options) *Queue {
	t.Helper()

	q, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	return q
}

func acquireAsync(t *testing.T, ctx context.Context, q *Queue) <-chan acquireResult {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)
	result := make(chan acquireResult, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)

		release, err := q.Acquire(ctx)
		result <- acquireResult{release: release, err: err}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return result
}

func receive(t *testing.T, result <-chan acquireResult) acquireResult {
	t.Helper()

	select {
	case acquired := <-result:
		return acquired
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not finish")

		return acquireResult{}
	}
}

func waitState(t *testing.T, q *Queue, want State) {
	t.Helper()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()

	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()

	for q.State() != want {
		select {
		case <-deadline.C:
			t.Fatalf("queue state = %+v, want %+v", q.State(), want)
		case <-poll.C:
		}
	}
}

// TestQueueExpiredGrant checks a slot granted after the wait deadline is returned before rejection.
func TestQueueExpiredGrant(t *testing.T) {
	q := newTestQueue(t, Options{Slots: 1, Capacity: 1, WaitTimeout: 20 * time.Millisecond})

	_, err := q.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	pending := acquireAsync(t, t.Context(), q)
	waitState(t, q, State{Active: 1, Waiting: 1})

	q.mu.Lock()
	<-time.After(q.options.WaitTimeout)
	q.releaseLocked()
	q.mu.Unlock()

	result := receive(t, pending)
	if !errors.Is(result.err, ErrTimeout) || result.release != nil {
		t.Fatalf("expired grant error = %v", result.err)
	}

	waitState(t, q, State{})
}
