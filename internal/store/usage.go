package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UsageStatus describes the observed response outcome, not guaranteed GPU termination or delivery.
type UsageStatus string

const (
	// UsageCompleted means the observed success response ended normally.
	UsageCompleted UsageStatus = "completed"
	// UsageFailed means the proxy or backend returned a failure response.
	UsageFailed UsageStatus = "failed"
	// UsageCanceled means client or revocation cancellation ended the request.
	UsageCanceled UsageStatus = "canceled"
	// UsageInterrupted means copying aborted or an SSE response lacked its terminal marker.
	UsageInterrupted UsageStatus = "interrupted"
)

// Usage holds one inference attempt. Nil counts and TTFT mean unknown, never zero usage.
type Usage struct {
	RequestID         string
	KeyID             int64
	Backend           string
	Model             *string
	Streaming         bool
	PromptTokens      *int64
	CompletionTokens  *int64
	ObservedSSEChunks int64
	TTFT              *time.Duration
	Duration          time.Duration
	HTTPStatus        int
	Status            UsageStatus
	StartedAt         time.Time
	Recovered         bool // Duration ends at lease recovery, not an observed inference completion.
}

// UsageStore inserts final usage records with a caller-owned Postgres pool.
type UsageStore struct {
	pool *pgxpool.Pool
}

// NewUsageStore uses pool without taking ownership of its lifetime.
func NewUsageStore(pool *pgxpool.Pool) *UsageStore {
	return &UsageStore{pool: pool}
}

// RecordUsage persists a final record or acknowledges an identical retry.
// The caller owns deadlines and retry policy; conflicting records remain unchanged.
func (s *UsageStore) RecordUsage(ctx context.Context, record Usage) error {
	if !record.Valid() {
		return fmt.Errorf("validate usage record: %w", ErrInvalidUsage)
	}

	if s == nil || s.pool == nil {
		return fmt.Errorf("record inference usage: %w", ErrDatabaseUnavailable)
	}

	// Lock and compare on conflict so retries cannot overwrite accounting.
	result, err := s.pool.Exec(ctx, usageInsert, usageArgs(record)...)
	if err != nil {
		return fmt.Errorf("record inference usage: %w", err)
	}

	if result.RowsAffected() != 1 {
		return fmt.Errorf("record inference usage: %w", ErrUsageConflict)
	}

	return nil
}

// Valid reports whether the record satisfies the persisted accounting bounds.
func (record Usage) Valid() bool {
	if record.Recovered && (record.Status != UsageInterrupted || record.PromptTokens != nil ||
		record.CompletionTokens != nil || record.TTFT != nil || record.HTTPStatus != 0) {
		return false
	}

	if !validUsageText(record.RequestID, 128) || !validUsageText(record.Backend, 128) ||
		record.KeyID <= 0 || record.StartedAt.IsZero() || record.Duration < 0 ||
		record.ObservedSSEChunks < 0 || (record.HTTPStatus != 0 && (record.HTTPStatus < 200 || record.HTTPStatus > 599)) {
		return false
	}

	if record.Model != nil && !validUsageText(*record.Model, 256) {
		return false
	}

	if (record.PromptTokens == nil) != (record.CompletionTokens == nil) ||
		(record.PromptTokens != nil && (*record.PromptTokens < 0 || *record.CompletionTokens < 0)) {
		return false
	}

	if (record.TTFT != nil && (*record.TTFT < 0 || *record.TTFT > record.Duration)) ||
		(!record.Streaming && (record.TTFT != nil || record.ObservedSSEChunks != 0)) {
		return false
	}

	switch record.Status {
	case UsageCompleted, UsageFailed, UsageCanceled, UsageInterrupted:
		return true
	default:
		return false
	}
}

func validUsageText(value string, maxBytes int) bool {
	return len(value) > 0 && len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

const usageInsert = `
		INSERT INTO usage (request_id, key_id, backend, model, streaming,
		    prompt_tokens, completion_tokens, observed_sse_chunks, ttft_ms, total_ms,
		    http_status, status, created_at, recovered)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (request_id) DO UPDATE SET request_id = EXCLUDED.request_id
		WHERE (usage.key_id, usage.backend, usage.model, usage.streaming,
		    usage.prompt_tokens, usage.completion_tokens, usage.observed_sse_chunks,
		    usage.ttft_ms, usage.total_ms, usage.http_status, usage.status, usage.created_at, usage.recovered)
		IS NOT DISTINCT FROM
		    (EXCLUDED.key_id, EXCLUDED.backend, EXCLUDED.model, EXCLUDED.streaming,
		    EXCLUDED.prompt_tokens, EXCLUDED.completion_tokens, EXCLUDED.observed_sse_chunks,
		    EXCLUDED.ttft_ms, EXCLUDED.total_ms, EXCLUDED.http_status, EXCLUDED.status, EXCLUDED.created_at, EXCLUDED.recovered)`

func usageArgs(record Usage) []any {
	var ttft *float64

	if record.TTFT != nil {
		milliseconds := float64(*record.TTFT) / float64(time.Millisecond)
		ttft = &milliseconds
	}

	return []any{record.RequestID, record.KeyID, record.Backend, record.Model, record.Streaming,
		record.PromptTokens, record.CompletionTokens, record.ObservedSSEChunks, ttft,
		float64(record.Duration) / float64(time.Millisecond), record.HTTPStatus, record.Status, record.StartedAt, record.Recovered}
}
