package store

import (
	"time"
)

// Admission identifies an inference attempt before generation, without retaining its body.
// Empty reservation identity means no quota reservation was obtained.
type Admission struct {
	RequestID      string
	KeyID          int64
	Backend        string
	Model          *string
	Streaming      bool
	StartedAt      time.Time
	ReservationID  string
	ReservationDay int64
	PromptTokens   int64
	OutputTokens   int64
}

// Valid reports whether admission identity and reservation fields can be persisted.
func (record Admission) Valid() bool {
	if !validUsageText(record.RequestID, 128) || record.KeyID <= 0 ||
		!validUsageText(record.Backend, 128) || record.StartedAt.IsZero() ||
		(record.Model != nil && !validUsageText(*record.Model, 256)) {
		return false
	}

	if record.ReservationID == "" {
		return record.ReservationDay == 0 && record.PromptTokens == 0 && record.OutputTokens == 0
	}

	return validUsageText(record.ReservationID, 128) && record.ReservationDay >= 0 &&
		record.PromptTokens >= 0 && record.OutputTokens > 0
}

const admissionInsert = `
		INSERT INTO inference_admissions (request_id, key_id, backend, model, streaming,
		    created_at, reservation_id, reservation_day, reserved_prompt_tokens, reserved_output_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (request_id) DO UPDATE SET request_id = EXCLUDED.request_id
		WHERE (inference_admissions.key_id, inference_admissions.backend,
		    inference_admissions.model, inference_admissions.streaming, inference_admissions.created_at,
		    inference_admissions.reservation_id, inference_admissions.reservation_day,
		    inference_admissions.reserved_prompt_tokens, inference_admissions.reserved_output_tokens)
		IS NOT DISTINCT FROM
		    (EXCLUDED.key_id, EXCLUDED.backend, EXCLUDED.model, EXCLUDED.streaming, EXCLUDED.created_at,
		    EXCLUDED.reservation_id, EXCLUDED.reservation_day,
		    EXCLUDED.reserved_prompt_tokens, EXCLUDED.reserved_output_tokens)`

func admissionArgs(record Admission) []any {
	var (
		reservation         *string
		day, prompt, output *int64
	)

	if record.ReservationID != "" {
		reservation = &record.ReservationID
		day, prompt, output = &record.ReservationDay, &record.PromptTokens, &record.OutputTokens
	}

	return []any{record.RequestID, record.KeyID, record.Backend, record.Model, record.Streaming,
		record.StartedAt, reservation, day, prompt, output}
}
