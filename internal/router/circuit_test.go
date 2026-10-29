package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/queue"
)

func acquireCircuit(t *testing.T, r *Router) *Assignment {
	t.Helper()

	name, assignment, err := r.Acquire(t.Context(), "b")
	if err != nil || name != "a" {
		t.Fatalf("acquire a: %s, %v", name, err)
	}

	t.Cleanup(assignment.Release)

	return assignment
}

func requireCircuitBlocked(t *testing.T, r *Router) {
	t.Helper()

	if _, _, err := r.Acquire(t.Context(), "b"); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("open circuit admitted request: %v", err)
	}
}

// TestCircuitRecovery checks failure streaks, neutral cleanup, cooldown boundaries, and trial recovery.
func TestCircuitRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, health := newTestRouter(t, 4)
		health.set("b", false)

		for range 2 {
			a := acquireCircuit(t, r)
			a.RecordOutcome(InferenceFailed)
			a.Release()
		}

		neutral := acquireCircuit(t, r)
		neutral.Release()

		success := acquireCircuit(t, r)
		success.RecordOutcome(InferenceSucceeded)
		success.Release()

		for range 3 {
			a := acquireCircuit(t, r)
			a.RecordOutcome(InferenceFailed)
			a.Release()
		}

		requireCircuitBlocked(t, r)
		health.set("a", false)
		health.set("a", true)
		requireCircuitBlocked(t, r)
		time.Sleep(10*time.Second - time.Nanosecond)
		requireCircuitBlocked(t, r)
		time.Sleep(time.Nanosecond)

		if _, err := r.Select(); !errors.Is(err, ErrNoBackend) {
			t.Fatal("models bypassed open circuit", err)
		}

		trial := acquireCircuit(t, r)
		requireCircuitBlocked(t, r)
		trial.RecordOutcome(InferenceFailed)
		trial.Release()
		requireCircuitBlocked(t, r)
		time.Sleep(10 * time.Second)

		abandoned := acquireCircuit(t, r)
		abandoned.Release()
		requireCircuitBlocked(t, r)
		time.Sleep(10 * time.Second)

		trial = acquireCircuit(t, r)
		trial.RecordOutcome(InferenceSucceeded)
		trial.RecordOutcome(InferenceFailed)
		trial.Release()
		trial.Release()

		if name, err := r.Select(); err != nil || name != "a" {
			t.Fatal("successful trial did not close circuit", name, err)
		}

		next := acquireCircuit(t, r)
		next.Release()

		if r.assigned[0] != 0 || r.targets[0].Admission.State() != (queue.State{}) {
			t.Fatal("assignment leaked")
		}
	})
}

// TestCircuitStaleResults checks older successes cannot close an open or newly recovered circuit.
func TestCircuitStaleResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _ := newTestRouter(t, 0)
		r.targets[0].Admission, _ = queue.New(queue.Options{Slots: 4, WaitTimeout: time.Second})
		r.circuitOptions.FailureThreshold = 1
		oldSuccess := acquireCircuit(t, r)
		oldFailure := acquireCircuit(t, r)
		failing := acquireCircuit(t, r)
		failing.RecordOutcome(InferenceFailed)
		failing.Release()
		oldSuccess.RecordOutcome(InferenceSucceeded)
		oldSuccess.Release()
		requireCircuitBlocked(t, r)
		time.Sleep(10 * time.Second)

		trial := acquireCircuit(t, r)
		trial.RecordOutcome(InferenceSucceeded)
		trial.Release()
		oldFailure.RecordOutcome(InferenceFailed)
		oldFailure.Release()

		a := acquireCircuit(t, r)
		a.Release()
	})
}

// TestCircuitConcurrentTrial checks a recovery trial remains singular under concurrent routing.
func TestCircuitConcurrentTrial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _ := newTestRouter(t, 40)
		r.circuitOptions.FailureThreshold = 1
		a := acquireCircuit(t, r)
		a.RecordOutcome(InferenceFailed)
		a.Release()
		time.Sleep(10 * time.Second)

		results := make(chan *Assignment, 40)

		var workers sync.WaitGroup
		for range 40 {
			workers.Go(func() {
				_, assignment, err := r.Acquire(t.Context(), "b")
				if err != nil && !errors.Is(err, ErrNoBackend) {
					t.Error(err)
				}

				results <- assignment
			})
		}

		workers.Wait()
		close(results)

		admitted := 0

		for assignment := range results {
			if assignment != nil {
				admitted++

				assignment.Release()
			}
		}

		if admitted != 1 {
			t.Fatalf("trials = %d, want 1", admitted)
		}

		requireCircuitBlocked(t, r)
	})
}

// TestCircuitQueuedAssignments checks an opening circuit prevents queued work from starting inference.
func TestCircuitQueuedAssignments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _ := newTestRouter(t, 1)
		r.circuitOptions.FailureThreshold = 1
		active := acquireCircuit(t, r)
		done := make(chan error, 1)

		go func() {
			_, assignment, err := r.Acquire(context.Background(), "b")
			if assignment != nil {
				assignment.Release()
			}

			done <- err
		}()

		synctest.Wait()
		active.RecordOutcome(InferenceFailed)
		active.Release()

		if err := <-done; !errors.Is(err, ErrNoBackend) {
			t.Fatal("queued work entered inference", err)
		}

		if r.assigned[0] != 0 || r.targets[0].Admission.State() != (queue.State{}) {
			t.Fatal("queued assignment leaked")
		}
	})
}
