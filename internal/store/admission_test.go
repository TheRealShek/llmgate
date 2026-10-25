package store

import (
	"testing"
	"time"
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

		if record.Valid() {
			t.Fatal("invalid admission passed validation")
		}
	}
}
