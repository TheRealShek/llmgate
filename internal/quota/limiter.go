// Package quota bounds daily input and output tokens before inference starts.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// PromptCounter counts the fully rendered prompt using the inference backend.
type PromptCounter interface {
	Count(context.Context, []byte) (int64, error)
}

// ReservationStore coordinates admission and final accounting across gateway instances.
type ReservationStore interface {
	Reserve(context.Context, Budget) (Reservation, error)
	Settle(context.Context, Reservation, int64) error
}

// Options bounds token counting, Redis operations, and shared daily grace.
type Options struct {
	CountTimeout time.Duration
	RedisTimeout time.Duration
	GracePercent int64
}

// Limiter holds immutable dependencies; each admitted request owns one Permit.
type Limiter struct {
	counter      PromptCounter
	reservations ReservationStore
	options      Options
}

// NewLimiter uses the shared backend transport and Redis store supplied by startup.
func NewLimiter(counter PromptCounter, reservations ReservationStore, options Options) *Limiter {
	return &Limiter{counter: counter, reservations: reservations, options: options}
}

// Admission carries a capped request body and its pending accounting ownership.
type Admission struct {
	Body   []byte
	Permit *Permit
}

// Permit owns one reservation until the usage recorder finishes or the UTC day expires.
// It retains no request body, credential, or request context.
type Permit struct {
	reservation  Reservation
	reservations ReservationStore
	timeout      time.Duration
}

// Admit counts input and atomically reserves it plus the output available to this request.
// Its child deadlines end before the original context enters inference.
func (l *Limiter) Admit(ctx context.Context, key store.Key, body []byte) (Admission, error) {
	if l == nil || l.counter == nil || l.reservations == nil || l.options.CountTimeout <= 0 ||
		l.options.RedisTimeout <= 0 || l.options.GracePercent < 0 || l.options.GracePercent > 100 {
		return Admission{}, fmt.Errorf("configure daily quota: %w", ErrUnavailable)
	}

	if key.ID <= 0 || key.TokensPerDay <= 0 || key.TokensPerDay > maxExact {
		return Admission{}, fmt.Errorf("validate daily quota: %w", ErrInvalidLimit)
	}

	grace := key.TokensPerDay/100*l.options.GracePercent + (key.TokensPerDay%100*l.options.GracePercent)/100
	if grace > maxExact-key.TokensPerDay {
		return Admission{}, fmt.Errorf("validate grace ceiling: %w", ErrInvalidLimit)
	}

	fields, output, err := prepare(body)
	if err != nil {
		return Admission{}, err
	}

	normalized, err := json.Marshal(fields)
	if err != nil {
		return Admission{}, fmt.Errorf("encode quota request: %w", ErrInvalidRequest)
	}

	countCtx, cancel := context.WithTimeout(ctx, l.options.CountTimeout)
	prompt, err := l.counter.Count(countCtx, normalized)
	countErr := countCtx.Err()

	cancel()

	if err != nil {
		return Admission{}, err
	}

	if countErr != nil {
		return Admission{}, fmt.Errorf("count deadline: %w", ErrUnavailable)
	}

	reserveCtx, cancel := context.WithTimeout(ctx, l.options.RedisTimeout)
	reservation, err := l.reservations.Reserve(reserveCtx, Budget{
		KeyID: key.ID, Limit: key.TokensPerDay, HardLimit: key.TokensPerDay + grace, PromptTokens: prompt, MaxOutputTokens: output,
	})
	reserveErr := reserveCtx.Err()

	cancel()

	if err != nil || reserveErr != nil {
		return Admission{}, fmt.Errorf("reserve daily quota: %w", ErrUnavailable)
	}

	if reservation.ID == "" {
		return Admission{}, &Exhausted{RetryAfter: reservation.RetryAfter}
	}
	// Remove alternative generation limits so backend precedence cannot override this cap.
	fields["max_tokens"] = json.RawMessage(fmt.Sprint(reservation.OutputTokens))

	capped, err := json.Marshal(fields)
	if err != nil {
		return Admission{}, fmt.Errorf("encode quota cap: %w", ErrUnavailable)
	}

	return Admission{Body: capped, Permit: &Permit{reservation: reservation, reservations: l.reservations, timeout: l.options.RedisTimeout}}, nil
}

// Finish reconciles completed responses; uncertain outcomes retain the full reservation.
// The independent deadline lets disconnects and revocation stop inference without skipping accounting.
func (p *Permit) Finish(ctx context.Context, record store.Usage) {
	if p == nil {
		return
	}

	charge := int64(-1)
	if record.Status == store.UsageCompleted && record.PromptTokens != nil && record.CompletionTokens != nil &&
		*record.PromptTokens >= 0 && *record.CompletionTokens >= 0 && *record.PromptTokens <= maxExact-*record.CompletionTokens {
		charge = *record.PromptTokens + *record.CompletionTokens
		if *record.PromptTokens != p.reservation.PromptTokens || *record.CompletionTokens > p.reservation.OutputTokens {
			requestlog.Logger(ctx).ErrorContext(ctx, "quota backend usage exceeded or differed from reservation", "key_id", p.reservation.KeyID)
			charge = max(charge, p.reservation.PromptTokens+p.reservation.OutputTokens)
		}
	}

	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()

	if err := p.reservations.Settle(finishCtx, p.reservation, charge); err != nil {
		requestlog.Logger(finishCtx).ErrorContext(finishCtx, "token quota settlement failed", "key_id", p.reservation.KeyID)
	}
}

func prepare(body []byte) (map[string]json.RawMessage, int64, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, 0, ErrInvalidRequest
	}

	for _, name := range []string{"n", "n_cmpl"} {
		if raw, exists := fields[name]; exists {
			var n int64
			if json.Unmarshal(raw, &n) != nil || n != 1 {
				return nil, 0, fmt.Errorf("require one completion: %w", ErrInvalidRequest)
			}
		}
	}

	var output int64
	if json.Unmarshal(fields["max_tokens"], &output) != nil || output <= 0 || output > maxExact {
		return nil, 0, ErrInvalidRequest
	}

	for _, alias := range []string{"max_completion_tokens", "n_predict"} {
		if raw, exists := fields[alias]; exists {
			var value int64
			if json.Unmarshal(raw, &value) != nil || value <= 0 || value > maxExact {
				return nil, 0, ErrInvalidRequest
			}

			output = min(output, value)

			delete(fields, alias)
		}
	}
	// Text-only content shares the exact tokenizer path. Media needs backend-specific accounting.
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 {
		return nil, 0, ErrInvalidRequest
	}

	for _, message := range messages {
		var text *string
		if len(message.Content) == 0 || json.Unmarshal(message.Content, &text) == nil {
			continue
		}

		var parts []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			return nil, 0, ErrInvalidRequest
		}

		for _, part := range parts {
			if part.Type != "text" || part.Text == nil {
				return nil, 0, ErrInvalidRequest
			}
		}
	}

	var stream bool

	_ = json.Unmarshal(fields["stream"], &stream)
	if stream {
		options := map[string]json.RawMessage{}
		if raw, exists := fields["stream_options"]; exists {
			if json.Unmarshal(raw, &options) != nil || options == nil {
				return nil, 0, ErrInvalidRequest
			}
		}

		options["include_usage"] = json.RawMessage("true")
		fields["stream_options"], _ = json.Marshal(options)
	}

	fields["max_tokens"] = json.RawMessage(fmt.Sprint(output))

	return fields, output, nil
}
