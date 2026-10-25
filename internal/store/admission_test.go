package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func admissionRecord() Admission {
	model := "test-model"

	return Admission{
		RequestID: "admission-test", KeyID: 1, Backend: "primary", Model: &model,
		Streaming: true, StartedAt: time.Now().UTC().Truncate(time.Microsecond),
		ReservationID: "reservation", ReservationDay: 20000, PromptTokens: 7, OutputTokens: 20,
	}
}

// TestAdmissionValidation checks invalid identities and partial reservations never reach Postgres.
func TestAdmissionValidation(t *testing.T) {
	for _, change := range []func(*Admission){
		func(r *Admission) { r.RequestID = "" },
		func(r *Admission) { r.KeyID = 0 },
		func(r *Admission) { r.Backend = "" },
		func(r *Admission) { r.StartedAt = time.Time{} },
		func(r *Admission) { r.ReservationID = "" },
		func(r *Admission) { r.ReservationDay = -1 },
		func(r *Admission) { r.PromptTokens = -1 },
		func(r *Admission) { r.OutputTokens = 0 },
	} {
		record := admissionRecord()
		change(&record)

		if err := NewUsageStore(nil).RecordAdmission(t.Context(), record); !errors.Is(err, ErrInvalidAdmission) {
			t.Fatal("invalid admission reached storage", err)
		}
	}

	if err := NewUsageStore(nil).RecordAdmission(t.Context(), admissionRecord()); !errors.Is(err, ErrDatabaseUnavailable) {
		t.Fatal("missing storage did not fail closed", err)
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
			if err := usage.RecordAdmission(t.Context(), record); err != nil {
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

		if err := usage.RecordAdmission(t.Context(), changed); !errors.Is(err, ErrUsageConflict) {
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
	if err := usage.RecordAdmission(ctx, record); !errors.Is(err, context.Canceled) {
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
	if err := NewUsageStore(async).RecordAdmission(t.Context(), record); err != nil {
		t.Fatal(err)
	}

	var setting string
	if err := async.QueryRow(t.Context(), "SHOW synchronous_commit").Scan(&setting); err != nil || setting != "off" {
		t.Fatal("transaction setting leaked into the pool", setting, err)
	}
}
