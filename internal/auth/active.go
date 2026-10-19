package auth

import (
	"context"
	"fmt"

	"github.com/therealshek/llmgate/internal/store"
)

// activeRequest holds one HTTP request's cancellation function, which reaches the backend.
type activeRequest struct {
	cancel context.CancelFunc
}

// RevokeKey persists revocation, then signals this process's requests for id.
// A successful update still triggers cancellation if ctx has since been canceled.
func (a *Authenticator) RevokeKey(ctx context.Context, id int64) error {
	if a == nil || a.keys == nil {
		return fmt.Errorf("revoke API key: %w", store.ErrDatabaseUnavailable)
	}

	if err := a.keys.RevokeKey(ctx, id); err != nil {
		return fmt.Errorf("revoke API key %d: %w", id, err)
	}

	// Every successful revocation advances the counter. In-flight lookups use it
	// to detect whether their database result needs rechecking.
	a.mu.Lock()
	a.generation++

	requests := make([]*activeRequest, 0, len(a.active[id]))
	for request := range a.active[id] {
		requests = append(requests, request)
	}
	a.mu.Unlock()

	// Use the copy outside the lock: exiting handlers need the mutex to remove
	// their entries. Cancellation signals them without waiting for their exit.
	for _, request := range requests {
		request.cancel()
	}

	return nil
}

// lookupAndRegister authenticates and tracks a request without letting concurrent
// revocation miss it. After success, the authentication handler must defer remove.
func (a *Authenticator) lookupAndRegister(
	ctx context.Context, secret string, request *activeRequest,
) (store.Key, error) {
	// Retry if revocation overlaps a lookup. All attempts share ctx's deadline.
	for {
		if err := ctx.Err(); err != nil {
			return store.Key{}, err
		}

		// Read the counter safely, then unlock so database I/O cannot block the registry.
		a.mu.Lock()
		generation := a.generation
		a.mu.Unlock()

		key, err := a.keys.LookupKey(ctx, secret)
		if err != nil {
			return store.Key{}, err
		}

		// Compare and register under one lock. Revocation must either find this
		// request in its snapshot or change the counter and force a retry.
		a.mu.Lock()
		if err := ctx.Err(); err != nil {
			a.mu.Unlock()
			return store.Key{}, err
		}

		if generation != a.generation {
			a.mu.Unlock()
			// Another key may have been revoked. Recheck Postgres rather than reject ours.
			continue
		}

		if a.active[key.ID] == nil {
			a.active[key.ID] = make(map[*activeRequest]struct{})
		}

		// The inner map is a set of requests belonging to this key.
		a.active[key.ID][request] = struct{}{}
		a.mu.Unlock()

		return key, nil
	}
}

// remove releases tracking when a protected handler exits, including cancellation or panic.
func (a *Authenticator) remove(id int64, request *activeRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.active[id], request)

	if len(a.active[id]) == 0 {
		delete(a.active, id)
	}
}
