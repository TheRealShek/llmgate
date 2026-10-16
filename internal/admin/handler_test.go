package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

const testToken = "operator-token-with-at-least-32-characters"
const validBody = `{"name":"client","requests_per_min":60,"tokens_per_day":100000}`

type creatorFunc func(context.Context, store.CreateKeyParams) (store.Key, string, error)

func (f creatorFunc) CreateKey(ctx context.Context, params store.CreateKeyParams) (store.Key, string, error) {
	return f(ctx, params)
}

type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("unauthorized body was read")
	return 0, io.EOF
}
func (b unreadBody) Close() error { return nil }

// TestAdminAuthentication checks rejected credentials reach neither body parsing nor storage.
func TestAdminAuthentication(t *testing.T) {
	keys := creatorFunc(func(context.Context, store.CreateKeyParams) (store.Key, string, error) {
		t.Error("unauthorized request reached storage")
		return store.Key{}, "", nil
	})

	for _, tt := range []struct {
		name    string
		headers []string
	}{
		{name: "missing"},
		{name: "wrong scheme", headers: []string{"Basic " + testToken}},
		{name: "empty bearer", headers: []string{"Bearer "}},
		{name: "wrong token", headers: []string{"Bearer incorrect-token"}},
		{name: "inference key", headers: []string{"Bearer lg_inference-key"}},
		{
			name:    "duplicate",
			headers: []string{"Bearer " + testToken, "Bearer " + testToken},
		},
		{name: "combined", headers: []string{"Bearer " + testToken + ", Bearer other"}},
		{name: "extra field", headers: []string{"Bearer " + testToken + " other"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)

			r.Body = unreadBody{t: t}
			for _, value := range tt.headers {
				r.Header.Add("Authorization", value)
			}

			w := httptest.NewRecorder()
			NewHandler(HandlerOptions{Keys: keys, Token: testToken, MaxBodyBytes: 128, CreateTimeout: time.Second}).ServeHTTP(w, r)

			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("authentication response = %d", w.Code)
			}

			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
				t.Fatal("missing private JSON response headers")
			}
		})
	}
}

// TestAdminInputValidation checks malformed and oversized requests never create a key.
func TestAdminInputValidation(t *testing.T) {
	for _, tt := range []struct {
		name, body, contentType string
		limit                   int64
		status                  int
	}{
		{name: "missing content type", body: validBody, status: 415},
		{
			name:        "wrong content type",
			body:        validBody,
			contentType: "text/plain",
			status:      415,
		},
		{name: "malformed", body: `{`, contentType: "application/json", status: 400},
		{name: "null", body: `null`, contentType: "application/json", status: 400},
		{name: "array", body: `[]`, contentType: "application/json", status: 400},
		{
			name:        "unknown field",
			body:        `{"surprise":true}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "trailing value",
			body:        validBody + `{}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "trailing junk",
			body:        validBody + `x`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "missing limits",
			body:        `{"name":"client"}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "empty name",
			body:        `{"name":" ","requests_per_min":1,"tokens_per_day":1}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "negative limit",
			body:        `{"name":"client","requests_per_min":-1,"tokens_per_day":1}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "fractional limit",
			body:        `{"name":"client","requests_per_min":1.5,"tokens_per_day":1}`,
			contentType: "application/json",
			status:      400,
		},
		{
			name:        "oversized",
			body:        validBody,
			contentType: "application/json",
			limit:       int64(len(validBody) - 1),
			status:      413,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limit := int64(1024)
			if tt.limit != 0 {
				limit = tt.limit
			}

			keys := creatorFunc(func(context.Context, store.CreateKeyParams) (store.Key, string, error) {
				t.Error("invalid input reached storage")
				return store.Key{}, "", nil
			})
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", strings.NewReader(tt.body))
			r.Header.Set("Authorization", "Bearer "+testToken)
			r.Header.Set("Content-Type", tt.contentType)

			w := httptest.NewRecorder()
			NewHandler(HandlerOptions{Keys: keys, Token: testToken, MaxBodyBytes: limit, CreateTimeout: time.Second}).ServeHTTP(w, r)

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

// TestAdminCreation checks named limits, one secret response, logging, and public-route isolation.
func TestAdminCreation(t *testing.T) {
	calls := 0

	var operationCtx context.Context

	keys := creatorFunc(func(ctx context.Context, params store.CreateKeyParams) (store.Key, string, error) {
		calls++
		operationCtx = ctx

		if params != (store.CreateKeyParams{Name: "client", RequestsPerMin: 60, TokensPerDay: 100000}) {
			t.Errorf("creation params = %+v", params)
		}

		if _, ok := ctx.Deadline(); !ok {
			t.Error("storage has no deadline")
		}

		if requestlog.ID(ctx) == "" {
			t.Error("storage lost request metadata")
		}

		return store.Key{ID: 7, Name: params.Name, RequestsPerMin: params.RequestsPerMin, TokensPerDay: params.TokensPerDay}, "lg_issued-secret", nil
	})

	var logs bytes.Buffer

	handler := requestlog.NewHandler(NewHandler(HandlerOptions{
		Keys: keys, Token: testToken, MaxBodyBytes: int64(len(validBody)), CreateTimeout: time.Second,
	}), slog.New(slog.NewJSONHandler(&logs, nil)))
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", strings.NewReader(validBody))
	r.Header.Set("Authorization", "bearer "+testToken)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != 201 || calls != 1 {
		t.Fatalf("status/calls = %d/%d", w.Code, calls)
	}

	var result struct {
		ID     int64  `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if result.ID != 7 || result.APIKey != "lg_issued-secret" {
		t.Fatal("created identity or secret missing")
	}

	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
		t.Fatal("private response lost headers")
	}

	if operationCtx.Err() != context.Canceled {
		t.Error("storage deadline was not released")
	}

	for _, secret := range []string{testToken, "lg_issued-secret", validBody} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("admin logs exposed private data")
		}
	}

	public := httptest.NewRecorder()
	gateway.NewHandler(gateway.HandlerOptions{}).ServeHTTP(public, r)

	if public.Code != 404 || calls != 1 {
		t.Fatal("public router exposed admin creation")
	}
}

// TestAdminStorageFailure checks timeouts, disconnects, and database diagnostics stay contained.
func TestAdminStorageFailure(t *testing.T) {
	for _, mode := range []string{"database failure", "invalid params", "deadline", "client cancellation"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			keys := creatorFunc(func(ctx context.Context, _ store.CreateKeyParams) (store.Key, string, error) {
				close(started)

				if mode == "database failure" {
					return store.Key{}, "", errors.New("database-password-secret")
				}

				if mode == "invalid params" {
					return store.Key{}, "", store.ErrInvalidKeyOptions
				}

				<-ctx.Done()

				return store.Key{}, "", ctx.Err()
			})

			timeout := time.Second
			if mode == "deadline" {
				timeout = 20 * time.Millisecond
			}

			ctx, cancel := context.WithCancel(t.Context())

			var workers sync.WaitGroup

			t.Cleanup(func() { cancel(); workers.Wait() })

			var logs bytes.Buffer

			handler := requestlog.NewHandler(NewHandler(HandlerOptions{Keys: keys, Token: testToken, MaxBodyBytes: 1024, CreateTimeout: timeout}), slog.New(slog.NewJSONHandler(&logs, nil)))
			r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", strings.NewReader(validBody)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+testToken)
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			done := make(chan struct{})

			workers.Go(func() { handler.ServeHTTP(w, r); close(done) })

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("storage did not start")
			}

			if mode == "client cancellation" {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("creation did not finish")
			}

			if mode == "client cancellation" {
				if w.Body.Len() != 0 {
					t.Fatal("disconnected client received a response")
				}
			} else {
				want := 503
				if mode == "invalid params" {
					want = 400
				}

				if w.Code != want {
					t.Fatalf("status = %d, want %d", w.Code, want)
				}
			}

			if strings.Contains(w.Body.String()+logs.String(), "database-password-secret") {
				t.Fatal("storage diagnostics leaked")
			}
		})
	}
}

// TestAdminRouting checks the private router exposes only the implemented versioned operation.
func TestAdminRouting(t *testing.T) {
	for _, tt := range []struct {
		name, method, path string
		status             int
	}{
		{
			name: "unsupported method", method: http.MethodGet, path: "/v1/admin/keys",
			status: 405,
		},
		{
			name: "revocation deferred", method: http.MethodDelete, path: "/v1/admin/keys/1",
			status: 404,
		},
		{name: "unversioned", method: http.MethodPost, path: "/admin/keys", status: 404},
		{
			name: "inference unavailable", method: http.MethodGet, path: "/v1/models",
			status: 404,
		},
		{
			name: "missing dependency", method: http.MethodPost, path: "/v1/admin/keys",
			status: 503,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			NewHandler(HandlerOptions{}).ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))

			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
		})
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (brokenBody) Close() error             { return nil }

// TestAdminBodyFailure checks interrupted uploads never create records.
func TestAdminBodyFailure(t *testing.T) {
	keys := creatorFunc(func(context.Context, store.CreateKeyParams) (store.Key, string, error) {
		t.Error("interrupted body reached storage")
		return store.Key{}, "", nil
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	r.Body = brokenBody{}
	w := httptest.NewRecorder()
	NewHandler(HandlerOptions{Keys: keys, Token: testToken, MaxBodyBytes: 1024, CreateTimeout: time.Second}).ServeHTTP(w, r)

	if w.Code != 400 {
		t.Fatalf("interrupted body status = %d", w.Code)
	}
}

type failingWriter struct {
	header http.Header
	status int
}

func (w *failingWriter) Header() http.Header       { return w.header }
func (w *failingWriter) WriteHeader(status int)    { w.status = status }
func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("private-delivery-error") }

// TestAdminDeliveryFailure checks a committed creation is not retried or leaked when output fails.
func TestAdminDeliveryFailure(t *testing.T) {
	calls := 0
	keys := creatorFunc(func(context.Context, store.CreateKeyParams) (store.Key, string, error) {
		calls++
		return store.Key{ID: 7}, "lg_issued-secret", nil
	})

	var logs bytes.Buffer

	handler := requestlog.NewHandler(NewHandler(HandlerOptions{
		Keys: keys, Token: testToken, MaxBodyBytes: 1024, CreateTimeout: time.Second,
	}), slog.New(slog.NewJSONHandler(&logs, nil)))
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys", strings.NewReader(validBody))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")

	w := &failingWriter{header: make(http.Header)}
	handler.ServeHTTP(w, r)

	if calls != 1 || w.status != 201 {
		t.Fatalf("delivery failure changed insertion count or status: %d/%d", calls, w.status)
	}

	for _, secret := range []string{testToken, "lg_issued-secret", "private-delivery-error"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("delivery diagnostics exposed private data")
		}
	}

	if !strings.Contains(logs.String(), "created admin key response could not be delivered") {
		t.Fatal("delivery failure was not recorded")
	}
}
