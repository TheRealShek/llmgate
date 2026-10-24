package quota

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

type counterFunc func(context.Context, []byte) (int64, error)

func (f counterFunc) Count(ctx context.Context, body []byte) (int64, error) { return f(ctx, body) }

type reservationStub struct {
	reserve func(context.Context, Budget) (Reservation, error)
	settle  func(context.Context, Reservation, int64) error
}

func (s reservationStub) Reserve(ctx context.Context, b Budget) (Reservation, error) {
	return s.reserve(ctx, b)
}
func (s reservationStub) Settle(ctx context.Context, r Reservation, n int64) error {
	return s.settle(ctx, r, n)
}

const chatBody = `{"model":"test","messages":[{"role":"user","content":"Hello"}],"max_tokens":200}`

// TestQuotaPreparation checks safe aliases, usage requests, and unsupported accounting inputs.
func TestQuotaPreparation(t *testing.T) {
	for _, tt := range []struct {
		name, extra string
		want        int64
		invalid     bool
	}{
		{name: "plain", want: 200},
		{name: "aliases capped", extra: `,"n_predict":150,"max_completion_tokens":100`, want: 100},
		{name: "usage requested", extra: `,"stream":true,"stream_options":{"include_usage":false}`, want: 200},
		{name: "multiple completions", extra: `,"n":2`, invalid: true},
		{name: "native multiple completions", extra: `,"n_cmpl":2`, invalid: true},
		{name: "unlimited alias", extra: `,"n_predict":-1`, invalid: true},
		{name: "null alias", extra: `,"max_completion_tokens":null`, invalid: true},
		{name: "invalid stream options", extra: `,"stream":true,"stream_options":[]`, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.TrimSuffix(chatBody, "}") + tt.extra + "}"

			fields, output, err := prepare([]byte(body))
			if tt.invalid {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatal(err)
				}

				return
			}

			if err != nil || output != tt.want {
				t.Fatal(output, err)
			}

			if _, ok := fields["n_predict"]; ok {
				t.Fatal("native limit retained")
			}

			if _, ok := fields["max_completion_tokens"]; ok {
				t.Fatal("alternative limit retained")
			}

			if strings.Contains(tt.extra, `"stream":true`) && !bytes.Contains(fields["stream_options"], []byte(`"include_usage":true`)) {
				t.Fatal("final stream usage not requested")
			}
		})
	}

	for _, body := range []string{
		`{}`, `{"messages":[],"max_tokens":1}`,
		`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"private"}}]}],"max_tokens":1}`,
	} {
		if _, _, err := prepare([]byte(body)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatal("unsupported prompt admitted", err)
		}
	}
}

// TestQuotaAdmission checks exact input, rounded grace, body capping, and preserved unknown fields.
func TestQuotaAdmission(t *testing.T) {
	counter := counterFunc(func(ctx context.Context, body []byte) (int64, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("counting has no deadline")
		}

		return 50, nil
	})
	reservations := reservationStub{reserve: func(ctx context.Context, b Budget) (Reservation, error) {
		if b != (Budget{KeyID: 7, Limit: 1001, HardLimit: 1101, PromptTokens: 50, MaxOutputTokens: 200}) {
			t.Error(b)
		}

		return Reservation{ID: "admitted", KeyID: 7, Day: 1, PromptTokens: 50, OutputTokens: 75}, nil
	}}
	limiter := NewLimiter(counter, reservations, Options{CountTimeout: time.Second, RedisTimeout: time.Second, GracePercent: 10})
	body := strings.TrimSuffix(chatBody, "}") + `,"custom":{"keep":9007199254740993}}`

	admission, err := limiter.Admit(t.Context(), store.Key{ID: 7, TokensPerDay: 1001}, []byte(body))
	if err != nil || admission.Permit == nil {
		t.Fatal(err)
	}

	if !bytes.Contains(admission.Body, []byte(`"max_tokens":75`)) || !bytes.Contains(admission.Body, []byte(`"keep":9007199254740993`)) {
		t.Fatal("cap or raw field changed", string(admission.Body))
	}

	if t.Context().Err() != nil {
		t.Fatal("admission canceled inference context")
	}
}

// TestQuotaDeadlines checks counting and reservation waits cannot consume unbounded time.
func TestQuotaDeadlines(t *testing.T) {
	for _, stage := range []string{"counter", "Redis"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				counter := counterFunc(func(ctx context.Context, _ []byte) (int64, error) {
					if stage == "counter" {
						<-ctx.Done()
						return 0, ErrUnavailable
					}

					return 10, nil
				})
				reservations := reservationStub{reserve: func(ctx context.Context, _ Budget) (Reservation, error) {
					<-ctx.Done()
					return Reservation{}, ctx.Err()
				}}
				limiter := NewLimiter(counter, reservations, Options{CountTimeout: time.Second, RedisTimeout: time.Second})
				started := time.Now()

				if _, err := limiter.Admit(t.Context(), store.Key{ID: 1, TokensPerDay: 100}, []byte(chatBody)); !errors.Is(err, ErrUnavailable) {
					t.Fatal(err)
				}

				if time.Since(started) != time.Second {
					t.Fatal("quota deadline not bounded")
				}
			})
		})
	}
}

// TestPermitAccounting checks cancellation, unknown usage, mismatches, and a bounded detached settlement.
func TestPermitAccounting(t *testing.T) {
	for _, tt := range []struct {
		name               string
		status             store.UsageStatus
		prompt, completion *int64
		want               int64
	}{
		{name: "completed", status: store.UsageCompleted, prompt: ptr(10), completion: ptr(3), want: 13},
		{name: "zero output", status: store.UsageCompleted, prompt: ptr(10), completion: ptr(0), want: 10},
		{name: "unknown", status: store.UsageCompleted, want: -1},
		{name: "canceled", status: store.UsageCanceled, prompt: ptr(10), completion: ptr(3), want: -1},
		{name: "failed", status: store.UsageFailed, want: -1},
		{name: "interrupted", status: store.UsageInterrupted, want: -1},
		{name: "undercounted prompt", status: store.UsageCompleted, prompt: ptr(9), completion: ptr(3), want: 30},
		{name: "excess usage", status: store.UsageCompleted, prompt: ptr(10), completion: ptr(30), want: 40},
		{name: "overflow", status: store.UsageCompleted, prompt: ptr(maxExact), completion: ptr(1), want: -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			permit := &Permit{reservation: Reservation{ID: "r", KeyID: 1, Day: 1, PromptTokens: 10, OutputTokens: 20}, timeout: time.Second,
				reservations: reservationStub{settle: func(ctx context.Context, r Reservation, n int64) error {
					calls++

					if ctx.Err() != nil || n != tt.want {
						t.Error("wrong charge or canceled accounting", n, ctx.Err())
					}

					if _, ok := ctx.Deadline(); !ok {
						t.Error("unbounded settlement")
					}

					return nil
				}}}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			permit.Finish(ctx, store.Usage{Status: tt.status, PromptTokens: tt.prompt, CompletionTokens: tt.completion})

			if calls != 1 {
				t.Fatal("settlement skipped")
			}
		})
	}
}

// TestSettlementFailure checks failed Redis reconciliation is logged without changing the response.
func TestSettlementFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer

		permit := &Permit{reservation: Reservation{ID: "r", KeyID: 1}, timeout: time.Second,
			reservations: reservationStub{settle: func(ctx context.Context, _ Reservation, _ int64) error { <-ctx.Done(); return ctx.Err() }}}
		handler := requestlog.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			permit.Finish(r.Context(), store.Usage{})
			w.WriteHeader(http.StatusCreated)
		}), slog.New(slog.NewJSONHandler(&logs, nil)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", nil))

		if w.Code != http.StatusCreated || !strings.Contains(logs.String(), "token quota settlement failed") {
			t.Fatal("settlement failure was hidden or changed response")
		}
	})
}

func ptr(n int64) *int64 { return &n }

// TestQuotaConstruction checks absent dependencies and unsafe integer limits fail closed.
func TestQuotaConstruction(t *testing.T) {
	for _, limiter := range []*Limiter{nil, NewLimiter(nil, nil, Options{})} {
		if _, err := limiter.Admit(t.Context(), store.Key{ID: 1, TokensPerDay: 10}, []byte(chatBody)); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}

	counter := counterFunc(func(context.Context, []byte) (int64, error) { t.Error("invalid quota reached backend"); return 0, nil })

	limiter := NewLimiter(counter, reservationStub{}, Options{CountTimeout: time.Second, RedisTimeout: time.Second, GracePercent: 100})
	for _, limit := range []int64{0, -1, maxExact, maxExact + 1} {
		if _, err := limiter.Admit(t.Context(), store.Key{ID: 1, TokensPerDay: limit}, []byte(chatBody)); !errors.Is(err, ErrInvalidLimit) {
			t.Fatal(limit, err)
		}
	}
}

// TestQuotaDenial checks exhausted budgets retain Redis retry guidance.
func TestQuotaDenial(t *testing.T) {
	limiter := NewLimiter(counterFunc(func(context.Context, []byte) (int64, error) { return 3, nil }),
		reservationStub{reserve: func(context.Context, Budget) (Reservation, error) { return Reservation{RetryAfter: time.Hour}, nil }},
		Options{CountTimeout: time.Second, RedisTimeout: time.Second})
	_, err := limiter.Admit(t.Context(), store.Key{ID: 1, TokensPerDay: 10}, []byte(chatBody))

	var exhausted *Exhausted
	if !errors.As(err, &exhausted) || exhausted.RetryAfter != time.Hour {
		t.Fatal(err)
	}
}
