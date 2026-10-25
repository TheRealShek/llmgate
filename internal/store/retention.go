package store

import (
	"context"
	"fmt"
	"time"
)

// PruneAccounting removes a bounded batch of expired final records and matching admissions.
// Incomplete admissions are never discarded by retention.
func (s *UsageStore) PruneAccounting(ctx context.Context, before time.Time, limit int) (int64, error) {
	if s == nil || s.pool == nil || before.IsZero() || limit <= 0 {
		return 0, fmt.Errorf("prune accounting: %w", ErrInvalidUsage)
	}

	var removed int64

	err := s.pool.QueryRow(ctx, `
	    WITH expired AS (
	        SELECT request_id FROM usage WHERE created_at < $1
	        ORDER BY created_at, request_id LIMIT $2 FOR UPDATE SKIP LOCKED
	    ), removed_usage AS (
	        DELETE FROM usage USING expired WHERE usage.request_id = expired.request_id
	        RETURNING usage.request_id
	    ), removed_admissions AS (
	        DELETE FROM inference_admissions USING removed_usage
	        WHERE inference_admissions.request_id = removed_usage.request_id
	    ) SELECT count(*) FROM removed_usage`, before, limit).Scan(&removed)
	if err != nil {
		return 0, fmt.Errorf("prune accounting: %w", err)
	}

	return removed, nil
}
