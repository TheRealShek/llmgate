package accounting

import (
	"context"
	"crypto/rand"
	_ "embed"
	"time"

	"github.com/therealshek/llmgate/internal/store"
)

//go:embed owner.lua
var ownerSource string

//go:embed expired.lua
var expiredSource string

//go:embed recover.lua
var recoverSource string

type activeRequest struct {
	owner  string
	cancel context.CancelFunc
}

func (q *Queue) ownerKeys(owner string) []string {
	keys := append([]string(nil), q.keys...)
	return append(keys, q.options.Namespace+":owner:"+owner, q.options.Namespace+":requests:"+owner)
}

func (q *Queue) currentOwner() string {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.owner
}

func (q *Queue) startOwnership(ctx context.Context) error {
	owner := rand.Text()
	if err := q.write(ctx, ownerSource, q.ownerKeys(owner), "create", owner, q.options.LeaseTimeout.Milliseconds()); err != nil {
		return err
	}

	q.mu.Lock()
	q.owner = owner
	q.mu.Unlock()

	return nil
}

func (q *Queue) register(id, owner string, cancel context.CancelFunc) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.owner == "" || q.owner != owner || cancel == nil {
		return ErrOwnershipLost
	}

	if _, exists := q.active[id]; !exists {
		if len(q.active) >= q.options.MaxPending {
			return ErrFull
		}

		q.active[id] = activeRequest{owner: q.owner, cancel: cancel}
	}

	return nil
}

func (q *Queue) forget(id, owner string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if request, ok := q.active[id]; ok && request.owner == owner {
		delete(q.active, id)
	}
}

func (q *Queue) loseOwnership() {
	q.mu.Lock()
	requests := q.active
	q.active = make(map[string]activeRequest)
	q.owner = ""
	q.mu.Unlock()

	for _, request := range requests {
		request.cancel()
	}
}

func (q *Queue) cancelActive() {
	q.mu.Lock()

	requests := make([]context.CancelFunc, 0, len(q.active))
	for _, request := range q.active {
		requests = append(requests, request.cancel)
	}
	q.mu.Unlock()

	for _, cancel := range requests {
		cancel()
	}
}

func (q *Queue) maintainOwnership(ctx context.Context) {
	ticker := time.NewTicker(q.options.LeaseTimeout / 3)
	defer ticker.Stop()
	defer q.loseOwnership()

	last := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !q.StorageReady() {
				q.cancelActive()
				continue
			}
			// A suspended process cannot revive a lease after its safe renewal window.
			if time.Since(last) >= q.options.LeaseTimeout {
				q.loseOwnership()
			}

			owner := q.currentOwner()

			var err error
			if owner == "" {
				err = q.startOwnership(ctx)
			} else {
				err = q.write(ctx, ownerSource, q.ownerKeys(owner), "renew", owner, q.options.LeaseTimeout.Milliseconds())
			}

			if err != nil {
				q.loseOwnership()
			} else {
				last = time.Now()
			}
		}
	}
}

func (q *Queue) recoverExpired(ctx context.Context) error {
	values, err := q.client.Eval(ctx, expiredSource, q.keys, q.options.Namespace, q.options.BatchSize).StringSlice()
	if err != nil {
		return ErrUnavailable
	}

	if len(values) < 2 {
		return nil
	}

	owner := values[0]
	args := []any{owner}

	now, err := q.client.Time(ctx).Result()
	if err != nil {
		return ErrUnavailable
	}

	for _, id := range values[1:] {
		fields, err := q.client.HMGet(ctx, q.keys[0], "a:"+id, "f:"+id).Result()
		if err != nil || len(fields) != 2 || fields[0] == nil || fields[1] == nil {
			return ErrUnavailable
		}

		admission, aOK := fields[0].(string)

		final, fOK := fields[1].(string)
		if !aOK || !fOK {
			return ErrUnavailable
		}

		var record store.Admission
		if err := decodeAdmission(admission, &record); err != nil {
			return err
		}

		outcome, err := padded(store.Usage{
			RequestID: record.RequestID, KeyID: record.KeyID, Backend: record.Backend,
			Model: record.Model, Streaming: record.Streaming, StartedAt: record.StartedAt,
			Duration: max(0, now.Sub(record.StartedAt)), Status: store.UsageInterrupted, Recovered: true,
		})
		if err != nil {
			return err
		}

		args = append(args, id, admission, final, outcome)
	}

	return q.write(ctx, recoverSource, q.ownerKeys(owner), args...)
}
