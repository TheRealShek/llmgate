package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/metrics"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/router"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

type attemptOptions struct {
	number   int
	excluded string
	started  time.Time
}

type attemptResult struct {
	backend      string
	failure      int
	retryable    bool
	admissionErr error
}

// forward keeps validation and the request-rate charge outside the attempt loop.
// Each attempt owns a new queue assignment, token reservation, and durable ID.
func (h *chatHandler) forward(w http.ResponseWriter, r *http.Request, body []byte, chat chatRequest) {
	delivery := &deliveryWriter{ResponseWriter: w, requestID: requestlog.ID(r.Context())}
	excluded := ""
	lastFailure := 0

	for number := 1; number <= h.maxAttempts; number++ {
		options := attemptOptions{number: number, excluded: excluded}
		if number > 1 {
			options.started = time.Now()
		}

		result := h.attempt(delivery, r, body, chat, options)
		if r.Context().Err() != nil {
			return
		}

		if result.admissionErr != nil {
			if lastFailure == 0 {
				writeAdmissionError(delivery, result.admissionErr)
			} else {
				writeProxyFailure(delivery, lastFailure)
			}

			return
		}

		if result.failure == 0 {
			return
		}

		lastFailure = result.failure
		if !result.retryable || delivery.committed.Load() || number == h.maxAttempts {
			if !delivery.final.Load() {
				writeProxyFailure(delivery, lastFailure)
			}

			return
		}

		excluded = result.backend
		requestlog.Logger(r.Context()).WarnContext(
			r.Context(), "attempting chat failover", "backend", excluded, "attempt", number+1,
		)
	}
}

func (h *chatHandler) attempt(
	w *deliveryWriter,
	r *http.Request,
	body []byte,
	chat chatRequest,
	options attemptOptions,
) (result attemptResult) {
	waitStarted := time.Now()

	name, assignment, err := h.routing.Acquire(r.Context(), options.excluded)
	metrics.Admission(r.Context(), name, time.Since(waitStarted), err)

	if err != nil {
		return attemptResult{admissionErr: err}
	}

	defer assignment.Release()

	result.backend = name
	target := h.backends[name]
	metrics.SelectBackend(r.Context(), name)

	upstream := target.NonStreaming
	if chat.stream {
		upstream = target.Streaming
	}

	if upstream == nil {
		_ = api.WriteError(w, http.StatusNotImplemented, api.Error{
			Message: "streaming chat is not configured", Type: "server_error", Code: "streaming_not_supported",
		})

		return result
	}

	if r.Context().Err() != nil {
		return result
	}

	key, ok := auth.Identity(r.Context())
	if !ok || target.Quota == nil {
		writeQuotaError(w, quota.ErrUnavailable)
		return result
	}

	admission, err := target.Quota.Admit(r.Context(), key, body)
	if r.Context().Err() != nil {
		admission.Permit.Release(r.Context())
		return result
	}

	if err != nil {
		if !errors.Is(err, quota.ErrInvalidRequest) {
			var exhausted *quota.Exhausted
			if !errors.As(err, &exhausted) {
				requestlog.Logger(r.Context()).WarnContext(r.Context(), "token quota admission failed", "key_id", key.ID)
			}
		}

		writeQuotaError(w, err)

		return result
	}

	// Rebuild each attempt from the validated client payload. The previous backend's
	// capped output allowance must not become the next attempt's requested cap.
	requestCtx, cancelRequest := context.WithCancel(r.Context())
	defer cancelRequest()

	captureCtx, outcome := proxy.NewAttempt(requestCtx, options.number < h.maxAttempts && len(h.backends) > 1)
	request := r.Clone(captureCtx)
	request.Body = io.NopCloser(bytes.NewReader(admission.Body))
	request.ContentLength = int64(len(admission.Body))
	request.TransferEncoding = nil
	// Only this admitted body may be sent. Clear any inherited replay of the
	// uncapped client payload.
	request.GetBody = nil

	var model string

	_ = json.Unmarshal(chat.fields["model"], &model)

	var ownershipLost atomic.Bool

	cancelOwnership := func() {
		ownershipLost.Store(true)
		cancelRequest()
	}

	observation, err := target.Usage.Observe(w, request, usage.Request{
		Model:       model,
		Streaming:   chat.stream,
		Attempt:     options.number,
		StartedAt:   options.started,
		Reservation: admission.Permit.Reservation(),
		Cancel:      cancelOwnership,
		FirstData:   func(at time.Time) { metrics.FirstData(request.Context(), at) },
	})
	if err != nil {
		admission.Permit.Release(request.Context())

		_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
			Message: "usage recording unavailable", Type: "server_error", Code: "usage_unavailable",
		})

		return result
	}

	finishInference := metrics.BeginInference(request.Context(), chat.stream)
	returned := false

	defer func() {
		recovered := recover()
		// SSE can carry a terminal error after response commitment. JSON must abort
		// an incomplete body; writing a second JSON error would corrupt the response.
		incomplete := recovered == nil && returned && chat.stream && !observation.StreamTerminated()

		backendIncomplete := incomplete && !observation.DeliveryFailed()
		if (recovered == http.ErrAbortHandler || incomplete) && chat.stream && w.final.Load() && request.Context().Err() == nil {
			contentType, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
			if contentType == "text/event-stream" {
				returned = false
				recovered = http.ErrAbortHandler
				controller := http.NewResponseController(w)
				// Do not let a client that stopped reading pin error delivery forever.
				if h.streamErrorTimeout > 0 && controller.SetWriteDeadline(time.Now().Add(h.streamErrorTimeout)) == nil {
					// This gateway-generated event must not become backend TTFT or usage data.
					event := "\n\nevent: error\n" +
						`data: {"error":{"message":"upstream stream interrupted","type":"server_error","code":"upstream_error"}}` + "\n\n"

					_, writeErr := io.WriteString(w, event)
					if writeErr == nil && controller.Flush() == nil {
						recovered = nil
					}

					_ = controller.SetWriteDeadline(time.Time{})
				}
			}
		}

		// Classify before releasing the slot so queued work sees an opened circuit.
		record := observation.Finish(request.Context(), returned)
		if request.Context().Err() == nil {
			switch {
			case outcome.BackendFailed() || record.HTTPStatus >= 500:
				assignment.RecordOutcome(router.InferenceFailed)
			case returned && record.Status == store.UsageCompleted:
				assignment.RecordOutcome(router.InferenceSucceeded)
			case (backendIncomplete || observation.StreamFailed()) && record.HTTPStatus >= 200 && record.HTTPStatus < 300:
				assignment.RecordOutcome(router.InferenceFailed)
			}
		}

		finishInference()
		assignment.Release()

		// Classify first, then stop the completed attempt independently of the client.
		// Cancellation does not acknowledge that remote GPU work has already stopped.
		cancelRequest()

		if err := target.Usage.Write(request.Context(), record); err == nil {
			admission.Permit.Finish(request.Context(), record)
		} else {
			// A second generation may not start after losing the first known outcome.
			result.retryable = false
		}

		// Lease callbacks may run during final handoff, after proxying returned.
		result.retryable = result.retryable && !ownershipLost.Load()

		if recovered != nil {
			panic(recovered)
		}
	}()

	upstream.ServeHTTP(observation.Writer(), request)

	returned = true
	result.failure, result.retryable = outcome.Failure()
	result.retryable = result.retryable && request.Context().Err() == nil

	return result
}

func writeProxyFailure(w http.ResponseWriter, status int) {
	_ = api.WriteError(w, status, api.Error{
		Message: "upstream request failed", Type: "server_error", Code: "upstream_error",
	})
}

// deliveryWriter observes commitment without buffering responses. Even an
// informational header or explicit flush prevents retry. Flush can be concurrent
// with proxy copying, so these flags use atomic operations.
type deliveryWriter struct {
	http.ResponseWriter
	committed atomic.Bool
	requestID string
	final     atomic.Bool
}

func (w *deliveryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *deliveryWriter) WriteHeader(status int) {
	w.committed.Store(true)

	if (status >= 200 || status == http.StatusSwitchingProtocols) && !w.final.Swap(true) {
		// ReverseProxy clears the header map after forwarding informational responses.
		// Restore the gateway identity when the final response is committed.
		if w.requestID != "" {
			w.Header().Set("X-Request-ID", w.requestID)
		}
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *deliveryWriter) Write(data []byte) (int, error) {
	if !w.final.Load() {
		w.WriteHeader(http.StatusOK)
	}

	return w.ResponseWriter.Write(data)
}

func (w *deliveryWriter) FlushError() error {
	if !w.final.Load() {
		w.WriteHeader(http.StatusOK)
	}

	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *deliveryWriter) Flush() { _ = w.FlushError() }
