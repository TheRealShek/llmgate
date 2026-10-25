package store

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"testing"
	"time"
)

// TestAccountingBatch checks all-or-nothing persistence and identical retries across admission and final usage.
func TestAccountingBatch(t *testing.T) {
	pool := usageTestPool(t)

	key, _, err := NewKeyStore(pool).CreateKey(t.Context(), CreateKeyParams{
		Name: "batch", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	a := admissionRecord()
	a.KeyID = key.ID
	u := usageRecord()
	u.RequestID, u.KeyID, u.Model, u.Streaming, u.StartedAt = a.RequestID, a.KeyID, a.Model, a.Streaming, a.StartedAt
	usage := NewUsageStore(pool)

	batch := []AccountingRecord{{Admission: a, Usage: &u}}
	if err := usage.RecordBatch(t.Context(), batch); err != nil {
		t.Fatal(err)
	}

	if err := usage.RecordBatch(t.Context(), batch); err != nil {
		t.Fatal("identical batch retry failed", err)
	}

	other := a
	other.RequestID = "must-rollback"
	conflict := u

	conflict.HTTPStatus = 503
	if err := usage.RecordBatch(t.Context(), []AccountingRecord{
		{Admission: other}, {Admission: a, Usage: &conflict},
	}); !errors.Is(err, ErrUsageConflict) {
		t.Fatal("conflicting batch acknowledged", err)
	}

	var count int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM inference_admissions").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed batch partially committed", count, err)
	}

	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM usage").Scan(&count); err != nil || count != 1 {
		t.Fatal("batch replay duplicated usage", count, err)
	}

	conflict = u

	conflict.RequestID = "other-identity"
	if err := usage.RecordBatch(t.Context(), []AccountingRecord{{Admission: a, Usage: &conflict}}); !errors.Is(err, ErrInvalidUsage) {
		t.Fatal("mismatched admission and outcome accepted", err)
	}
}

// TestAdmissionPersistence checks concurrent delivery, immutable fields, and survival without final usage.
func TestAdmissionPersistence(t *testing.T) {
	pool := usageTestPool(t)

	key, _, err := NewKeyStore(pool).CreateKey(t.Context(), CreateKeyParams{
		Name: "admission", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	record := admissionRecord()
	record.KeyID = key.ID
	usage := NewUsageStore(pool)

	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			if err := usage.RecordBatch(t.Context(), []AccountingRecord{{Admission: record}}); err != nil {
				t.Error(err)
			}
		})
	}

	workers.Wait()

	for _, change := range []func(*Admission){
		func(r *Admission) { r.Backend = "other" },
		func(r *Admission) { r.Model = nil },
		func(r *Admission) { r.Streaming = false },
		func(r *Admission) { r.StartedAt = r.StartedAt.Add(time.Second) },
		func(r *Admission) { r.ReservationID = "other" },
		func(r *Admission) { r.ReservationDay++ },
		func(r *Admission) { r.PromptTokens++ },
		func(r *Admission) { r.OutputTokens++ },
	} {
		changed := record
		change(&changed)

		if err := usage.RecordBatch(t.Context(), []AccountingRecord{{Admission: changed}}); !errors.Is(err, ErrUsageConflict) {
			t.Fatal("conflicting admission accepted", err)
		}
	}

	var (
		saved Admission
		count int
	)

	err = pool.QueryRow(t.Context(), `SELECT request_id, key_id, backend, model, streaming, created_at,
	    reservation_id, reservation_day, reserved_prompt_tokens, reserved_output_tokens
	    FROM inference_admissions`).Scan(&saved.RequestID, &saved.KeyID, &saved.Backend,
		&saved.Model, &saved.Streaming, &saved.StartedAt, &saved.ReservationID,
		&saved.ReservationDay, &saved.PromptTokens, &saved.OutputTokens)
	if err != nil || saved.RequestID != record.RequestID || saved.ReservationID != record.ReservationID ||
		saved.ReservationDay != record.ReservationDay || saved.PromptTokens != 7 || saved.OutputTokens != 20 {
		t.Fatal("admission lost reservation identity", saved, err)
	}

	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM usage").Scan(&count); err != nil || count != 0 {
		t.Fatal("admission fabricated final usage", count, err)
	}

	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM inference_admissions").Scan(&count); err != nil || count != 1 {
		t.Fatal("retries duplicated admission", count, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	record.RequestID = "canceled"
	if err := usage.RecordBatch(ctx, []AccountingRecord{{Admission: record}}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled admission accepted", err)
	}
}

// TestAdmissionSynchronousCommit checks admission overrides asynchronous connection defaults locally.
func TestAdmissionSynchronousCommit(t *testing.T) {
	pool := usageTestPool(t)

	key, _, err := NewKeyStore(pool).CreateKey(t.Context(), CreateKeyParams{
		Name: "synchronous", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = pool.Exec(t.Context(), `
	    CREATE FUNCTION require_sync() RETURNS trigger LANGUAGE plpgsql AS $$
	    BEGIN
	        IF current_setting('synchronous_commit') <> 'on' THEN
	            RAISE EXCEPTION 'admission was asynchronous';
	        END IF;
	        RETURN NEW;
	    END $$;
	    CREATE TRIGGER require_sync BEFORE INSERT ON inference_admissions
	    FOR EACH ROW EXECUTE FUNCTION require_sync();`)
	if err != nil {
		t.Fatal(err)
	}

	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	cfg.MaxConns = 1

	async, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(async.Close)

	record := admissionRecord()

	record.KeyID = key.ID
	if err := NewUsageStore(async).RecordBatch(t.Context(), []AccountingRecord{{Admission: record}}); err != nil {
		t.Fatal(err)
	}

	var setting string
	if err := async.QueryRow(t.Context(), "SHOW synchronous_commit").Scan(&setting); err != nil || setting != "off" {
		t.Fatal("transaction setting leaked into the pool", setting, err)
	}
}
