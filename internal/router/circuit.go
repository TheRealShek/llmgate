package router

import (
	"log/slog"
	"time"
)

// CircuitOptions controls consecutive inference failures and the recovery delay.
// Nonpositive values use three failures and a ten-second cooldown.
type CircuitOptions struct {
	FailureThreshold int
	Cooldown         time.Duration
}

// InferenceOutcome describes a backend result, excluding client and local failures.
type InferenceOutcome int

const (
	// InferenceSucceeded resets the failure streak or closes a recovery trial.
	InferenceSucceeded InferenceOutcome = iota + 1
	// InferenceFailed advances the failure streak or reopens a recovery trial.
	InferenceFailed
)

// circuitState is protected by the router mutex. An open circuit permits one
// chat assignment after cooldown; model listing cannot consume a recovery trial.
type circuitState struct {
	failures    int
	generation  uint64
	openedUntil time.Time
	trial       bool
}

func (c *circuitState) available(now time.Time, inference bool) bool {
	return c.openedUntil.IsZero() || (inference && !c.trial && !now.Before(c.openedUntil))
}

// Assignment owns one queue slot and the circuit generation selected before waiting.
// Report at most one inference outcome before Release. Releasing without an outcome
// is neutral: admission failures and canceled clients do not count against a backend.
// All methods are safe for concurrent calls and repeated cleanup.
type Assignment struct {
	router      *Router
	index       int
	generation  uint64
	releaseSlot func()
	recorded    bool
	released    bool
}

// RecordOutcome updates the circuit only while this assignment still owns its generation.
// Results from requests selected before the circuit opened cannot change its recovery state.
func (a *Assignment) RecordOutcome(outcome InferenceOutcome) {
	r := a.router
	r.mu.Lock()
	defer r.mu.Unlock()

	if a.recorded || a.released || (outcome != InferenceSucceeded && outcome != InferenceFailed) {
		return
	}

	a.recorded = true

	c := &r.circuits[a.index]
	if a.generation != c.generation {
		return
	}

	if outcome == InferenceSucceeded {
		if c.trial {
			c.generation++

			slog.Info("backend circuit closed", "backend", r.targets[a.index].Name)
		}

		c.failures = 0
		c.trial = false
		c.openedUntil = time.Time{}

		return
	}

	c.failures++
	if c.trial || c.failures >= r.circuitOptions.FailureThreshold {
		a.openCircuit()
	}
}

func (a *Assignment) openCircuit() {
	r := a.router
	c := &r.circuits[a.index]
	c.generation++
	c.failures = 0
	c.trial = false
	c.openedUntil = time.Now().Add(r.circuitOptions.Cooldown)
	slog.Warn("backend circuit opened", "backend", r.targets[a.index].Name, "cooldown", r.circuitOptions.Cooldown)
}

// Release frees capacity once. An abandoned recovery trial waits another cooldown
// without incrementing the backend failure streak. No timers or workers are created.
func (a *Assignment) Release() {
	r := a.router
	r.mu.Lock()
	defer r.mu.Unlock()

	if a.released {
		return
	}

	a.released = true

	c := &r.circuits[a.index]
	if !a.recorded && a.generation == c.generation && c.trial {
		a.openCircuit()
	}

	if a.releaseSlot != nil {
		a.releaseSlot()
	}

	r.assigned[a.index]--
}
