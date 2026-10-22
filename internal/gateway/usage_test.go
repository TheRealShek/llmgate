package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

type captureUsage func(context.Context, store.Usage) error

func (f captureUsage) RecordUsage(ctx context.Context, record store.Usage) error {
	return f(ctx, record)
}

// TestUsageScope checks only validated inference attempts receive records, preserving request bodies.
func TestUsageScope(t *testing.T) {
	for _, tt := range []struct {
		name, path, body string
		status, records  int
	}{
		{name: "models", path: "/v1/models", status: 200},
		{name: "malformed JSON", path: "/v1/chat/completions", body: `{`, status: 400},
		{name: "invalid token limit", path: "/v1/chat/completions", body: `{"max_tokens":0}`, status: 400},
		{name: "unsupported stream", path: "/v1/chat/completions", body: `{"stream":true}`, status: 501},
		{name: "inference", path: "/v1/chat/completions", body: `{"model":"test-model","messages":[],"max_tokens":8}`, status: 200, records: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			count := 0
			recorder := usage.NewRecorder(usage.Options{
				Store: captureUsage(func(_ context.Context, record store.Usage) error {
					count++

					if record.Model == nil || *record.Model != "test-model" || record.KeyID != 1 {
						t.Error("validated metadata did not reach usage recording")
					}

					return nil
				}), Backend: "test", WriteTimeout: time.Second, MaxBytes: 1024,
			})
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/v1/chat/completions" && string(body) != tt.body {
					t.Error("recording changed the request body")
				}

				_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
			})
			handler := NewHandler(HandlerOptions{
				Authentication: testAuthenticator(), RequestLimits: testRequestLimits(),
				NonStreaming: backend, Usage: recorder, Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})

			method := http.MethodPost
			if tt.path == "/v1/models" {
				method = http.MethodGet
			}

			r := authenticatedRequest(method, tt.path, strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if count != tt.records || w.Code != tt.status {
				t.Fatal("usage scope or HTTP validation changed", count, w.Code)
			}
		})
	}
}

// TestMissingUsageRecorder checks incomplete construction cannot silently omit inference recording.
func TestMissingUsageRecorder(t *testing.T) {
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing recorder admitted inference") })
	handler := NewHandler(HandlerOptions{
		Authentication: testAuthenticator(), RequestLimits: testRequestLimits(),
		NonStreaming: backend, Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	r := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "usage_unavailable") {
		t.Fatal("missing recorder did not fail closed", w.Code)
	}
}
