package usage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

type usageFunc func(context.Context, store.Usage) error

func (usageFunc) RecordAdmission(context.Context, store.Admission, context.CancelFunc) error {
	return nil
}

func (f usageFunc) RecordUsage(ctx context.Context, record store.Usage) error { return f(ctx, record) }

type admissionStore struct {
	usageFunc
	admit func(context.Context, store.Admission) error
}

func (s admissionStore) RecordAdmission(ctx context.Context, record store.Admission, _ context.CancelFunc) error {
	return s.admit(ctx, record)
}

// TestAdmissionDeadline checks failed, expired, and canceled admission cannot return an inference writer.
func TestAdmissionDeadline(t *testing.T) {
	for _, scenario := range []string{"failure", "deadline", "late success", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				recorder := NewRecorder(Options{
					Store: admissionStore{
						usageFunc: func(context.Context, store.Usage) error {
							t.Error("admission wrote final usage")
							return nil
						},
						admit: func(ctx context.Context, record store.Admission) error {
							calls++

							if _, ok := ctx.Deadline(); !ok || record.KeyID != 11 || record.Model == nil {
								t.Error("admission lost deadline or identity")
							}

							if scenario != "failure" {
								<-ctx.Done()
							}

							if scenario == "late success" {
								return nil
							}

							return errors.New("private-database-diagnostics")
						},
					}, Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
				})
				a := auth.NewAuthenticator(keyLookup{}, auth.Options{
					LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 8,
				})
				handler := a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if scenario == "canceled" {
						ctx, cancel := context.WithCancel(r.Context())
						cancel()

						r = r.WithContext(ctx)
					}

					observation, err := recorder.Observe(w, r, Request{Model: "model"})
					if !errors.Is(err, ErrUnavailable) || observation != nil {
						t.Error("failed admission returned an inference writer", err)
					}
				}))
				handler.ServeHTTP(httptest.NewRecorder(), inferenceRequest())

				if calls != 1 {
					t.Fatal("admission was skipped or retried", calls)
				}
			})
		})
	}
}

type keyLookup struct{}

func (keyLookup) LookupKey(context.Context, string) (store.Key, error) { return store.Key{ID: 11}, nil }
func (keyLookup) RevokeKey(context.Context, int64) error               { return nil }

func authenticated(recorder *Recorder, next http.Handler, streaming bool) http.Handler {
	a := auth.NewAuthenticator(keyLookup{}, auth.Options{
		LookupTimeout: time.Second, CacheTTL: time.Minute, CacheCapacity: 8,
	})

	return a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observation, err := recorder.Observe(w, r, Request{Model: "test-model", Streaming: streaming})
		if err != nil {
			panic(err)
		}

		returned := false
		defer func() { recorder.Write(r.Context(), observation.Finish(r.Context(), returned)) }()

		next.ServeHTTP(observation.Writer(), r)

		returned = true
	}))
}

func inferenceRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("private-prompt"))
	r.Header.Set("Authorization", "Bearer private-key")

	return r
}

// TestUsageOutcomes checks final records describe JSON, SSE, HTTP failures, and incomplete responses.
func TestUsageOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		stream     bool
		httpStatus int
		status     store.UsageStatus
		chunks     int64
		known      bool
	}{
		{name: "JSON usage", body: `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`, httpStatus: 200, status: store.UsageCompleted, known: true},
		{name: "JSON missing usage", body: `{"choices":[]}`, httpStatus: 200, status: store.UsageCompleted},
		{name: "SSE usage", body: "data: {}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", stream: true, httpStatus: 200, status: store.UsageCompleted, chunks: 2, known: true},
		{name: "interrupted SSE", body: "data: {}\n\n", stream: true, httpStatus: 200, status: store.UsageInterrupted, chunks: 1},
		{name: "HTTP error", body: `{"error":{}}`, httpStatus: 503, status: store.UsageFailed},
		{name: "SSE error", body: "data: {\"error\":{}}\n\n", stream: true, httpStatus: 200, status: store.UsageFailed, chunks: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var records []store.Usage

			recorder := NewRecorder(Options{
				Store: usageFunc(func(ctx context.Context, record store.Usage) error {
					if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
						t.Error("usage write has no live bounded context")
					}

					records = append(records, record)

					return nil
				}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
			})
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.httpStatus)
				_, _ = io.WriteString(w, tt.body)
			})

			var logs bytes.Buffer

			handler := requestlog.NewHandler(authenticated(recorder, next, tt.stream),
				slog.New(slog.NewJSONHandler(&logs, nil)))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, inferenceRequest())

			if w.Code != tt.httpStatus || w.Body.String() != tt.body || len(records) != 1 {
				t.Fatal("recording changed the response or wrote more than once")
			}

			record := records[0]
			if record.Status != tt.status || record.HTTPStatus != tt.httpStatus || record.KeyID != 11 ||
				record.Model == nil || *record.Model != "test-model" || record.Backend != "primary" ||
				record.RequestID != w.Header().Get("X-Request-ID") || record.StartedAt.IsZero() ||
				record.ObservedSSEChunks != tt.chunks || record.Duration < 0 {
				t.Fatalf("incorrect final usage record: %+v", record)
			}

			if (record.PromptTokens != nil) != tt.known || (record.CompletionTokens != nil) != tt.known {
				t.Fatal("missing token usage was confused with known counts")
			}

			if tt.known && (*record.PromptTokens != 7 || *record.CompletionTokens != 2) {
				t.Fatal("backend counts were changed")
			}

			if (record.TTFT != nil) != (tt.chunks > 0) || (record.TTFT != nil && *record.TTFT > record.Duration) {
				t.Fatal("TTFT was invented or exceeded total time")
			}

			if strings.Contains(logs.String(), "private-prompt") || strings.Contains(logs.String(), "private-key") {
				t.Fatal("accounting leaked private request data")
			}
		})
	}
}

// TestUsageAfterCancellation checks final writes detach client cancellation without extending inference.
func TestUsageAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	recorder := NewRecorder(Options{
		Store: usageFunc(func(ctx context.Context, record store.Usage) error {
			calls++

			if ctx.Err() != nil || record.Status != store.UsageCanceled || record.PromptTokens != nil {
				t.Error("client cancellation lost or canceled final accounting")
			}

			return nil
		}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		cancel()
	})
	w := httptest.NewRecorder()
	authenticated(recorder, next, true).ServeHTTP(w, inferenceRequest().WithContext(ctx))

	if calls != 1 || !w.Flushed {
		t.Fatal("cancellation lost accounting or flush support")
	}
}

// TestUsageAfterAbort checks proxy abort panics still escape after one final interrupted record.
func TestUsageAfterAbort(t *testing.T) {
	calls := 0
	recorder := NewRecorder(Options{
		Store: usageFunc(func(_ context.Context, record store.Usage) error {
			calls++

			if record.Status != store.UsageInterrupted || record.ObservedSSEChunks != 1 {
				t.Error("aborted copy appeared successful")
			}

			return nil
		}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {}\n\n")

		panic(http.ErrAbortHandler)
	})

	defer func() {
		if got := recover(); got != http.ErrAbortHandler || calls != 1 {
			t.Fatal("usage swallowed abort behavior or failed to record", got, calls)
		}
	}()

	authenticated(recorder, next, true).ServeHTTP(httptest.NewRecorder(), inferenceRequest())
}

// TestUsageWriteFailure checks recording errors are visible without replacing a completed inference response.
func TestUsageWriteFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		recorder := NewRecorder(Options{
			Store: usageFunc(func(ctx context.Context, _ store.Usage) error {
				calls++

				<-ctx.Done()

				return errors.New("private-database-diagnostics")
			}), Backend: "primary", WriteTimeout: 2 * time.Second, MaxBytes: 1024,
		})
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"choices":[]}`)
		})

		var logs bytes.Buffer

		handler := requestlog.NewHandler(authenticated(recorder, next, false),
			slog.New(slog.NewJSONHandler(&logs, nil)))
		w := httptest.NewRecorder()
		started := time.Now()

		handler.ServeHTTP(w, inferenceRequest())

		if time.Since(started) != 2*time.Second || calls != 1 || w.Code != 200 || w.Body.String() != `{"choices":[]}` {
			t.Fatal("usage failure changed response, retried, or escaped its deadline")
		}

		if !strings.Contains(logs.String(), "usage record write failed") || strings.Contains(logs.String(), "private-database") {
			t.Fatal("usage failure was hidden or exposed driver diagnostics")
		}
	})
}

// TestUsageArrivalTiming checks TTFT includes work before recording and excludes the final database write.
func TestUsageArrivalTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder := NewRecorder(Options{
			Store: usageFunc(func(_ context.Context, record store.Usage) error {
				if record.TTFT == nil || *record.TTFT != 50*time.Millisecond || record.Duration != 60*time.Millisecond {
					t.Error("timing did not start at gateway arrival", record.TTFT, record.Duration)
				}

				time.Sleep(time.Second)

				return nil
			}), Backend: "primary", WriteTimeout: 2 * time.Second, MaxBytes: 1024,
		})
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(30 * time.Millisecond)

			_, _ = io.WriteString(w, "data: {}\n\n")

			time.Sleep(10 * time.Millisecond)

			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		})
		inference := authenticated(recorder, next, true)
		handler := requestlog.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(20 * time.Millisecond)
			inference.ServeHTTP(w, r)
		}), slog.New(slog.NewJSONHandler(io.Discard, nil)))
		handler.ServeHTTP(httptest.NewRecorder(), inferenceRequest())
	})
}

type partialWriter struct{ *httptest.ResponseRecorder }

func (w partialWriter) Write(p []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(p[:len("data: {}\n\n")])
	return n, io.ErrClosedPipe
}

// TestUsagePartialWrite checks undelivered suffixes cannot supply fabricated usage or completion.
func TestUsagePartialWrite(t *testing.T) {
	calls := 0
	recorder := NewRecorder(Options{
		Store: usageFunc(func(_ context.Context, record store.Usage) error {
			calls++

			if record.Status != store.UsageInterrupted || record.PromptTokens != nil || record.ObservedSSEChunks != 1 {
				t.Error("undelivered data changed accounting")
			}

			return nil
		}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(w, "data: {}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Error("response write error was swallowed", err)
		}
	})
	w := partialWriter{httptest.NewRecorder()}
	authenticated(recorder, next, true).ServeHTTP(w, inferenceRequest())

	if calls != 1 || w.Body.String() != "data: {}\n\n" {
		t.Fatal("partial response changed or lost its final record")
	}
}

// TestConcurrentUsageRequests checks one recorder has no shared per-request observation state.
func TestConcurrentUsageRequests(t *testing.T) {
	var mu sync.Mutex

	ids := make(map[string]bool)
	recorder := NewRecorder(Options{
		Store: usageFunc(func(_ context.Context, record store.Usage) error {
			mu.Lock()
			defer mu.Unlock()

			if ids[record.RequestID] || record.Status != store.UsageCompleted || record.ObservedSSEChunks != 1 {
				t.Error("concurrent records shared identity or observation state")
			}

			ids[record.RequestID] = true

			return nil
		}), Backend: "primary", WriteTimeout: time.Second, MaxBytes: 1024,
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {}\n\ndata: [DONE]\n\n")
	})
	handler := requestlog.NewHandler(authenticated(recorder, next, true), slog.New(slog.NewJSONHandler(io.Discard, nil)))

	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { handler.ServeHTTP(httptest.NewRecorder(), inferenceRequest()) })
	}

	workers.Wait()

	if len(ids) != 32 {
		t.Fatal("concurrent requests lost usage records", len(ids))
	}
}
