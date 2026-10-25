// Package usage records inference admission, observes responses, and writes final accounting.
package usage

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// Store persists admission before inference and final accounting afterward.
type Store interface {
	RecordAdmission(context.Context, store.Admission, context.CancelFunc) error
	RecordUsage(context.Context, store.Usage) error
}

// Options names the usage dependency, backend identity, and resource bounds.
type Options struct {
	Store        Store
	Backend      string
	WriteTimeout time.Duration
	MaxBytes     int
}

// Recorder owns immutable dependencies; each Observation owns its response state.
type Recorder struct {
	options Options
}

// NewRecorder uses the shared store and validated limits assembled by startup.
func NewRecorder(options Options) *Recorder {
	return &Recorder{options: options}
}

// Request describes validated chat metadata without retaining the prompt body.
type Request struct {
	Model       string
	Streaming   bool
	FirstData   func(time.Time) // Optional notification for the first complete SSE data event.
	Reservation quota.Reservation
	Cancel      context.CancelFunc // Stops this request if accounting ownership is lost.
}

// Observation owns the response writer and accounting state for one inference attempt.
type Observation struct {
	writer *responseWriter
	record store.Usage
}

// Observe durably hands off admission before returning a writer that may enter inference.
func (r *Recorder) Observe(w http.ResponseWriter, request *http.Request, metadata Request) (*Observation, error) {
	key, ok := auth.Identity(request.Context())
	if !ok || r == nil || r.options.Store == nil || r.options.Backend == "" ||
		r.options.WriteTimeout <= 0 || r.options.MaxBytes <= 0 {
		return nil, ErrUnavailable
	}

	started := requestlog.Started(request.Context())
	if started.IsZero() {
		started = time.Now()
	}

	id := requestlog.ID(request.Context())
	if id == "" {
		id = rand.Text()
	}

	record := store.Usage{
		RequestID: id, KeyID: key.ID, Backend: r.options.Backend,
		Streaming: metadata.Streaming, StartedAt: started,
	}
	if metadata.Model != "" && len(metadata.Model) <= 256 && !strings.ContainsRune(metadata.Model, 0) {
		record.Model = &metadata.Model
	}

	reservation := metadata.Reservation
	if reservation.ID != "" && reservation.KeyID != key.ID {
		return nil, ErrUnavailable
	}

	admitCtx, cancel := context.WithTimeout(request.Context(), r.options.WriteTimeout)
	defer cancel()

	err := r.options.Store.RecordAdmission(admitCtx, store.Admission{
		RequestID: record.RequestID, KeyID: record.KeyID, Backend: record.Backend,
		Model: record.Model, Streaming: record.Streaming, StartedAt: record.StartedAt,
		ReservationID: reservation.ID, ReservationDay: reservation.Day,
		PromptTokens: reservation.PromptTokens, OutputTokens: reservation.OutputTokens,
	}, metadata.Cancel)
	if err != nil || admitCtx.Err() != nil {
		requestlog.Logger(request.Context()).ErrorContext(request.Context(), "inference admission write failed", "key_id", key.ID)
		return nil, ErrUnavailable
	}

	observed := &observer{
		streaming: metadata.Streaming, maxBytes: r.options.MaxBytes,
		firstLine: true, first: metadata.FirstData,
	}

	return &Observation{
		writer: &responseWriter{ResponseWriter: w, observer: observed},
		record: record,
	}, nil
}

// Writer forwards response bytes while observing only the bytes accepted by the client writer.
func (o *Observation) Writer() http.ResponseWriter {
	return o.writer
}

// Finish classifies the response after proxying returns or panics; it performs no external writes.
func (o *Observation) Finish(ctx context.Context, returned bool) store.Usage {
	record := o.record
	writer := o.writer
	observed := writer.observer

	record.Duration = time.Since(record.StartedAt)

	if !record.Streaming && !observed.overflow {
		observed.inspectJSON(observed.body)
	}

	record.HTTPStatus = writer.status
	record.ObservedSSEChunks = observed.chunks

	record.PromptTokens, record.CompletionTokens = observed.prompt, observed.completion
	if observed.firstData != nil {
		ttft := observed.firstData.Sub(record.StartedAt)
		record.TTFT = &ttft
	}

	switch {
	case ctx.Err() != nil:
		record.Status = store.UsageCanceled
	case !returned || writer.writeErr != nil:
		record.Status = store.UsageInterrupted
	case writer.status < 200 || writer.status >= 300 || observed.failed:
		record.Status = store.UsageFailed
	case record.Streaming && !observed.done:
		record.Status = store.UsageInterrupted
	default:
		record.Status = store.UsageCompleted
	}

	return record
}

// Write durably hands off the final outcome independently of client cancellation.
// A failed handoff must leave uncertain quota charged rather than refunding it.
func (r *Recorder) Write(ctx context.Context, record store.Usage) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.WriteTimeout)
	defer cancel()

	if err := r.options.Store.RecordUsage(ctx, record); err != nil {
		requestlog.Logger(ctx).ErrorContext(ctx, "usage record write failed", "key_id", record.KeyID)
		return err
	}

	return nil
}
