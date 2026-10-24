// Package usage observes inference responses and writes final accounting records.
package usage

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// Store persists the final accounting record under a caller-owned deadline.
type Store interface {
	RecordUsage(context.Context, store.Usage) error
}

// Options names the usage dependency, backend identity, and resource bounds.
type Options struct {
	Store        Store
	Backend      string
	WriteTimeout time.Duration
	MaxBytes     int
}

// Recorder owns immutable dependencies; each Record call owns its observation state.
type Recorder struct {
	options Options
}

// NewRecorder uses the shared store and validated limits assembled by startup.
func NewRecorder(options Options) *Recorder {
	return &Recorder{options: options}
}

// Request describes validated chat metadata without retaining the prompt body.
type Request struct {
	Model     string
	Streaming bool
	Quota     *quota.Permit
}

// Record forwards one validated inference request and attempts one final usage write.
// It preserves panics so ReverseProxy can abort incomplete responses as before.
func (r *Recorder) Record(w http.ResponseWriter, request *http.Request, next http.Handler, metadata Request) {
	key, ok := auth.Identity(request.Context())
	if !ok || r == nil || r.options.Store == nil || r.options.Backend == "" ||
		r.options.WriteTimeout <= 0 || r.options.MaxBytes <= 0 {
		metadata.Quota.Finish(request.Context(), store.Usage{})

		_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
			Message: "usage recording unavailable", Type: "server_error", Code: "usage_unavailable",
		})

		return
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

	observed := &observer{streaming: metadata.Streaming, maxBytes: r.options.MaxBytes, firstLine: true}
	writer := &responseWriter{ResponseWriter: w, observer: observed}
	returned := false

	defer func() {
		record.Duration = time.Since(started)

		if !metadata.Streaming && !observed.overflow {
			observed.inspectJSON(observed.body)
		}

		record.HTTPStatus = writer.status
		record.ObservedSSEChunks = observed.chunks

		record.PromptTokens, record.CompletionTokens = observed.prompt, observed.completion
		if observed.firstData != nil {
			ttft := observed.firstData.Sub(started)
			record.TTFT = &ttft
		}

		switch {
		case request.Context().Err() != nil:
			record.Status = store.UsageCanceled
		case !returned || writer.writeErr != nil:
			record.Status = store.UsageInterrupted
		case writer.status < 200 || writer.status >= 300 || observed.failed:
			record.Status = store.UsageFailed
		case metadata.Streaming && !observed.done:
			record.Status = store.UsageInterrupted
		default:
			record.Status = store.UsageCompleted
		}

		// Reconcile Redis independently of the best-effort Postgres insert below.
		metadata.Quota.Finish(request.Context(), record)

		// Proxy copying is finished, possibly by panic or client cancellation. Retain
		// request metadata but detach its cancellation for this bounded, synchronous write.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), r.options.WriteTimeout)
		defer cancel()

		if err := r.options.Store.RecordUsage(ctx, record); err != nil {
			requestlog.Logger(ctx).ErrorContext(ctx, "usage record write failed", "key_id", key.ID)
		}
	}()

	next.ServeHTTP(writer, request)

	returned = true
}
