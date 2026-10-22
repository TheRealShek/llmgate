package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func usageTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := testPool(t)

	migration, err := os.ReadFile("../../migrations/000002_usage.up.sql")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}

	return pool
}

func usageRecord() Usage {
	return Usage{
		RequestID: "usage-test", KeyID: 1, Backend: "primary", Duration: time.Second,
		HTTPStatus: 200, Status: UsageCompleted, StartedAt: time.Now(),
	}
}

// TestUsageValidation checks invalid records fail before accessing storage.
func TestUsageValidation(t *testing.T) {
	negative := int64(-1)
	zero := int64(0)
	negativeTime := -time.Second
	late := 2 * time.Second
	invalidModel := "bad\x00model"

	for _, change := range []func(*Usage){
		func(r *Usage) { r.RequestID = "" },
		func(r *Usage) { r.KeyID = 0 },
		func(r *Usage) { r.Backend = "" },
		func(r *Usage) { r.Duration = -time.Second },
		func(r *Usage) { r.Status = "unknown" },
		func(r *Usage) { r.HTTPStatus = 199 },
		func(r *Usage) { r.ObservedSSEChunks = 1 },
		func(r *Usage) { r.Streaming = true; r.ObservedSSEChunks = -1 },
		func(r *Usage) { r.StartedAt = time.Time{} },
		func(r *Usage) { r.Model = &invalidModel },
		func(r *Usage) { r.PromptTokens = &zero },
		func(r *Usage) { r.PromptTokens = &negative; r.CompletionTokens = &zero },
		func(r *Usage) { r.Streaming = true; r.TTFT = &negativeTime },
		func(r *Usage) { r.Streaming = true; r.TTFT = &late },
	} {
		record := usageRecord()
		change(&record)

		if err := NewUsageStore(nil).RecordUsage(t.Context(), record); !errors.Is(err, ErrInvalidUsage) {
			t.Fatal("invalid usage reached storage", err)
		}
	}

	if err := NewUsageStore(nil).RecordUsage(t.Context(), usageRecord()); !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatal("missing usage storage was not reported", err)
	}
}

// TestUsagePersistence checks nullable counts, submillisecond timings, stable key identity, and uniqueness.
func TestUsagePersistence(t *testing.T) {
	pool := usageTestPool(t)
	keys := NewKeyStore(pool)

	key, _, err := keys.CreateKey(t.Context(), CreateKeyParams{
		Name: "usage", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	prompt, completion := int64(7), int64(2)
	model := "test-model"
	ttft := 250 * time.Microsecond
	record := usageRecord()
	record.KeyID = key.ID
	record.Model = &model
	record.Streaming = true
	record.PromptTokens, record.CompletionTokens = &prompt, &completion
	record.ObservedSSEChunks = 3
	record.TTFT = &ttft
	record.Duration = 1500 * time.Microsecond
	usage := NewUsageStore(pool)

	if err := usage.RecordUsage(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	var (
		storedPrompt, storedCompletion *int64
		storedTTFT, total              float64
		keyID, chunks                  int64
	)
	if err := pool.QueryRow(t.Context(), `SELECT key_id, prompt_tokens, completion_tokens,
	    observed_sse_chunks, ttft_ms, total_ms FROM usage WHERE request_id=$1`, record.RequestID).
		Scan(&keyID, &storedPrompt, &storedCompletion, &chunks, &storedTTFT, &total); err != nil {
		t.Fatal(err)
	}

	if keyID != key.ID || storedPrompt == nil || *storedPrompt != 7 ||
		storedCompletion == nil || *storedCompletion != 2 || chunks != 3 || storedTTFT != 0.25 || total != 1.5 {
		t.Fatal("usage values changed in storage")
	}

	var constraint *pgconn.PgError
	if err := usage.RecordUsage(t.Context(), record); !errors.As(err, &constraint) || constraint.Code != "23505" {
		t.Fatal("duplicate request record was accepted or lost its cause", err)
	}

	if err := keys.RevokeKey(t.Context(), key.ID); err != nil {
		t.Fatal(err)
	}

	unknown := usageRecord()
	unknown.RequestID = "unknown-counts"
	unknown.KeyID = key.ID
	unknown.Status = UsageCanceled

	unknown.HTTPStatus = 0
	if err := usage.RecordUsage(t.Context(), unknown); err != nil {
		t.Fatal("revocation prevented final usage from being recorded", err)
	}

	var storedUnknown, noTTFT bool
	if err := pool.QueryRow(t.Context(), `SELECT prompt_tokens IS NULL AND completion_tokens IS NULL,
	    ttft_ms IS NULL FROM usage WHERE request_id=$1`, unknown.RequestID).Scan(&storedUnknown, &noTTFT); err != nil {
		t.Fatal(err)
	}

	if !storedUnknown || !noTTFT {
		t.Fatal("unknown counts or absent TTFT became zero")
	}

	if _, err := pool.Exec(t.Context(), "DELETE FROM api_keys WHERE id=$1", key.ID); err == nil {
		t.Fatal("key deletion orphaned persisted usage")
	}
}

// TestUsageStorageCancellation checks a canceled write preserves its cause and does not insert a row.
func TestUsageStorageCancellation(t *testing.T) {
	pool := usageTestPool(t)

	key, _, err := NewKeyStore(pool).CreateKey(t.Context(), CreateKeyParams{
		Name: "canceled", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	record := usageRecord()
	record.KeyID = key.ID
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := NewUsageStore(pool).RecordUsage(ctx, record); !errors.Is(err, context.Canceled) {
		t.Fatal("write cancellation lost its cause", err)
	}

	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM usage").Scan(&count); err != nil || count != 0 {
		t.Fatal("canceled write inserted a record", count, err)
	}
}

// TestUsageMigrationRollback checks migrations roll back in dependency order and can be reapplied.
func TestUsageMigrationRollback(t *testing.T) {
	pool := usageTestPool(t)

	for _, path := range []string{
		"000002_usage.down.sql", "000001_api_keys.down.sql",
		"000001_api_keys.up.sql", "000002_usage.up.sql",
	} {
		migration, err := os.ReadFile("../../migrations/" + path)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
			t.Fatal("migration failed", path, err)
		}
	}

	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM usage").Scan(&count); err != nil || count != 0 {
		t.Fatal("usage migration did not recreate an empty table", count, err)
	}
}
