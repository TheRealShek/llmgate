package accounting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/store"
)

type retentionSink struct {
	prune func(context.Context, time.Time, int) (int64, error)
}

func (s retentionSink) RecordBatch(context.Context, []store.AccountingRecord) error { return nil }

func (s retentionSink) PruneAccounting(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return s.prune(ctx, cutoff, limit)
}

// TestRetentionDrain checks full batches continue immediately and failures preserve the next sweep.
func TestRetentionDrain(t *testing.T) {
	failure := errors.New("storage unavailable")
	calls := 0
	q := &Queue{options: queueOptions()}
	q.storageReady.Store(true)
	q.sink = retentionSink{prune: func(_ context.Context, cutoff time.Time, limit int) (int64, error) {
		calls++

		if limit != q.options.BatchSize || time.Since(cutoff) < q.options.Retention {
			t.Fatal("retention limits or cutoff changed", cutoff, limit)
		}

		if calls == 2 {
			return 0, failure
		}

		if calls == 4 {
			return 1, nil
		}

		return int64(limit), nil
	}}

	if err := q.prune(t.Context()); !errors.Is(err, failure) || calls != 2 {
		t.Fatal("failed batch did not stop the sweep", err, calls)
	}

	if err := q.prune(t.Context()); err != nil || calls != 4 {
		t.Fatal("next sweep did not drain full batches", err, calls)
	}
}

// TestRetentionCancellation checks an active sweep stops and its owner can join it.
func TestRetentionCancellation(t *testing.T) {
	entered := make(chan struct{})
	q := &Queue{options: queueOptions()}
	q.options.PruneInterval = time.Millisecond
	q.storageReady.Store(true)
	q.sink = retentionSink{prune: func(ctx context.Context, _ time.Time, _ int) (int64, error) {
		close(entered)
		<-ctx.Done()

		return 0, ctx.Err()
	}}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)

		q.maintainRetention(ctx)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retention sweep did not start")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention sweep did not stop")
	}
}
