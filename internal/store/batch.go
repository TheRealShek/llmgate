package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccountingRecord carries durable admission and, when known, its immutable final outcome.
type AccountingRecord struct {
	Admission Admission
	Usage     *Usage
}

// RecordBatch pipelines inserts inside one synchronous transaction.
// An empty batch checks database availability without creating a transaction.
func (s *UsageStore) RecordBatch(ctx context.Context, records []AccountingRecord) error {
	for _, record := range records {
		if !record.Admission.Valid() {
			return fmt.Errorf("validate accounting batch admission: %w", ErrInvalidAdmission)
		}

		if u := record.Usage; u != nil {
			a := record.Admission
			if !u.Valid() || u.RequestID != a.RequestID || u.KeyID != a.KeyID ||
				u.Backend != a.Backend || u.Streaming != a.Streaming || !u.StartedAt.Equal(a.StartedAt) ||
				(u.Model == nil) != (a.Model == nil) || (u.Model != nil && *u.Model != *a.Model) {
				return fmt.Errorf("validate accounting batch outcome: %w", ErrInvalidUsage)
			}
		}
	}

	if s == nil || s.pool == nil {
		return fmt.Errorf("persist accounting batch: %w", ErrDatabaseUnavailable)
	}

	if len(records) == 0 {
		return s.pool.Ping(ctx)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin accounting batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = on"); err != nil {
		return fmt.Errorf("configure durable accounting batch: %w", err)
	}

	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(admissionInsert, admissionArgs(record.Admission)...)

		if record.Usage != nil {
			batch.Queue(usageInsert, usageArgs(*record.Usage)...)
		}
	}

	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	for range batch.Len() {
		result, err := results.Exec()
		if err != nil {
			return fmt.Errorf("write accounting batch: %w", err)
		}

		if result.RowsAffected() != 1 {
			return fmt.Errorf("write accounting batch: %w", ErrUsageConflict)
		}
	}

	if err := results.Close(); err != nil {
		return fmt.Errorf("finish accounting batch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit accounting batch: %w", err)
	}

	return nil
}
