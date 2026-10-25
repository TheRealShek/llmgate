package accounting

import (
	"context"
	"log/slog"
	"time"
)

func (q *Queue) prune(ctx context.Context) error {
	if !q.StorageReady() {
		return ErrStorageFull
	}
	// Each transaction is bounded; a full batch continues without waiting another interval.
	for ctx.Err() == nil {
		writeCtx, cancel := context.WithTimeout(ctx, q.options.Timeout)
		count, err := q.sink.PruneAccounting(writeCtx, time.Now().Add(-q.options.Retention), q.options.BatchSize)

		cancel()

		if err != nil {
			return err
		}

		if count < int64(q.options.BatchSize) {
			return nil
		}
	}

	return ctx.Err()
}

func (q *Queue) maintainRetention(ctx context.Context) {
	ticker := time.NewTicker(q.options.PruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := q.prune(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "accounting retention failed")
			}
		}
	}
}
