package accounting

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/store"
)

// TestExpiredOwnershipRecovery checks live owners survive scans and expired owners produce immutable unknown outcomes.
func TestExpiredOwnershipRecovery(t *testing.T) {
	var recovered *store.Usage

	sink := sinkFunc(func(_ context.Context, records []store.AccountingRecord) error {
		for _, record := range records {
			if record.Usage != nil {
				recovered = record.Usage
			}
		}

		return nil
	})
	q := openQueue(t, sink)
	a, final := queueRecord("abandoned")

	r, err := quota.NewRedisStore(q.client).Reserve(t.Context(), quota.Budget{KeyID: 19, Limit: 1000, HardLimit: 1000, PromptTokens: 7, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = q.client.Del(context.Background(), "llmgate:tokens:v1:19").Err() })

	a.KeyID, final.KeyID = 19, 19
	a.ReservationID, a.ReservationDay = r.ID, r.Day

	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	other, err := Open(t.Context(), q.client, sink, q.options)
	if err != nil {
		t.Fatal(err)
	}

	if recovered != nil || q.client.HLen(t.Context(), q.keys[0]).Val() != 2 {
		t.Fatal("live request recovered or its final slot freed")
	}

	owner := q.currentOwner()
	if err := q.client.Del(t.Context(), q.ownerKeys(owner)[6]).Err(); err != nil {
		t.Fatal(err)
	}

	if err := q.client.ZAdd(t.Context(), q.keys[4], redis.Z{Score: 0, Member: owner}).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := other.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if recovered == nil || recovered.Status != store.UsageInterrupted || !recovered.Recovered || recovered.PromptTokens != nil || recovered.CompletionTokens != nil {
		t.Fatal("abandoned request did not retain unknown usage", recovered)
	}

	if err := q.RecordUsage(t.Context(), final); !errors.Is(err, ErrOwnershipLost) {
		t.Fatal("expired owner replaced recovery", err)
	}

	if used, err := q.client.HGet(t.Context(), "llmgate:tokens:v1:19", "used").Int64(); err != nil || used != 27 {
		t.Fatal("unknown consumption was refunded", used, err)
	}

	if q.client.HExists(t.Context(), "llmgate:tokens:v1:19", "r:"+r.ID).Val() {
		t.Fatal("recovered marker retained")
	}

	if q.client.HLen(t.Context(), q.keys[0]).Val() != 0 {
		t.Fatal("recovered request retained capacity")
	}
}

// TestOwnershipLossCancelsRequests checks heartbeat failure cancels local inference before a replacement epoch starts.
func TestOwnershipLossCancelsRequests(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	var canceled atomic.Bool

	a, _ := queueRecord("cancel-on-loss")
	if err := q.RecordAdmission(t.Context(), a, func() { canceled.Store(true) }); err != nil {
		t.Fatal(err)
	}

	owner := q.currentOwner()
	if err := q.client.Del(t.Context(), q.ownerKeys(owner)[6]).Err(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() { defer close(done); q.maintainOwnership(ctx) }()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()

	for !canceled.Load() {
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("lease loss did not cancel inference")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-done

	if q.currentOwner() != "" {
		t.Fatal("lost epoch remained usable")
	}
}
