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
			name: "missing revocation dependency", method: http.MethodDelete, path: "/v1/admin/keys/1",
			status: 503,
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

type revokerFunc func(context.Context, int64) error

func (f revokerFunc) RevokeKey(ctx context.Context, id int64) error { return f(ctx, id) }

// TestAdminRevocation checks authenticated deletion uses a bounded operation and returns an empty private reply.
func TestAdminRevocation(t *testing.T) {
	calls := 0

	var operationCtx context.Context

	revoker := revokerFunc(func(ctx context.Context, id int64) error {
		calls++
		operationCtx = ctx

		if id != 7 || requestlog.ID(ctx) == "" {
			t.Fatal("revocation lost key identity or request metadata")
		}

		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("revocation has no deadline")
		}

		return nil
	})

	var logs bytes.Buffer

	handler := requestlog.NewHandler(NewHandler(HandlerOptions{
		Revoker: revoker, Token: testToken, RevokeTimeout: time.Second,
	}), slog.New(slog.NewJSONHandler(&logs, nil)))

	for range 2 {
		r := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/7", nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Body = unreadBody{t: t}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("revocation reply = %d %q", w.Code, w.Body.String())
		}

		if operationCtx.Err() != context.Canceled {
			t.Fatal("revocation deadline was not released")
		}
	}

	if calls != 2 || !strings.Contains(logs.String(), `"key_id":7`) || strings.Contains(logs.String(), testToken) {
		t.Fatal("revocation calls or safe audit logs changed")
	}

	public := httptest.NewRecorder()
	gateway.NewHandler(gateway.HandlerOptions{}).ServeHTTP(public,
		httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/7", nil))

	if public.Code != http.StatusNotFound {
		t.Fatal("public listener exposed revocation")
	}
}

// TestAdminRevocationValidation checks credentials precede ID parsing and invalid IDs never reach storage.
func TestAdminRevocationValidation(t *testing.T) {
	for _, tt := range []struct {
		name, id string
		headers  []string
		status   int
	}{
		{name: "missing credential", id: "invalid", status: 401},
		{name: "inference key", id: "7", headers: []string{"Bearer lg_test"}, status: 401},
		{name: "duplicate credential", id: "7", headers: []string{"Bearer " + testToken, "Bearer " + testToken}, status: 401},
		{name: "wrong credential", id: "7", headers: []string{"Bearer wrong"}, status: 401},
		{name: "negative", id: "-1", headers: []string{"Bearer " + testToken}, status: 400},
		{name: "zero", id: "0", headers: []string{"Bearer " + testToken}, status: 400},
		{name: "leading zeros", id: "007", headers: []string{"Bearer " + testToken}, status: 400},
		{name: "signed", id: "+7", headers: []string{"Bearer " + testToken}, status: 400},
		{name: "non-numeric", id: "bad", headers: []string{"Bearer " + testToken}, status: 400},
		{name: "overflow", id: "9223372036854775808", headers: []string{"Bearer " + testToken}, status: 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			revoker := revokerFunc(func(context.Context, int64) error {
				t.Error("invalid request reached revocation")
				return nil
			})
			r := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/"+tt.id, nil)

			r.Body = unreadBody{t: t}
			for _, value := range tt.headers {
				r.Header.Add("Authorization", value)
			}

			w := httptest.NewRecorder()
			NewHandler(HandlerOptions{Revoker: revoker, Token: testToken, RevokeTimeout: time.Second}).ServeHTTP(w, r)

			if w.Code != tt.status || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("revocation status = %d, want %d", w.Code, tt.status)
			}

			if tt.status == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing bearer challenge")
			}
		})
	}
}

// TestAdminRevocationFailure checks not-found and database failures keep safe, distinct HTTP results.
func TestAdminRevocationFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "unknown ID", err: store.ErrKeyNotFound, status: 404},
		{name: "database failure", err: errors.New("password=private-secret"), status: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer

			handler := requestlog.NewHandler(NewHandler(HandlerOptions{
				Revoker: revokerFunc(func(context.Context, int64) error { return tt.err }),
				Token:   testToken, RevokeTimeout: time.Second,
			}), slog.New(slog.NewJSONHandler(&logs, nil)))
			r := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/7", nil)
			r.Header.Set("Authorization", "Bearer "+testToken)

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if w.Code != tt.status || strings.Contains(w.Body.String()+logs.String(), "private-secret") {
				t.Fatalf("unsafe revocation error or status %d", w.Code)
			}
		})
	}
}

// TestAdminRevocationCancellation checks operation deadlines and operator disconnects reach storage.
func TestAdminRevocationCancellation(t *testing.T) {
	for _, clientCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "operator disconnect"}[clientCancel], func(t *testing.T) {
			started := make(chan struct{})
			done := make(chan struct{})
			revoker := revokerFunc(func(ctx context.Context, _ int64) error {
				close(started)
				<-ctx.Done()

				return ctx.Err()
			})

			timeout := 10 * time.Millisecond
			if clientCancel {
				timeout = time.Second
			}

			ctx, cancel := context.WithCancel(t.Context())

			var workers sync.WaitGroup

			t.Cleanup(func() { cancel(); workers.Wait() })

			r := httptest.NewRequest(http.MethodDelete, "/v1/admin/keys/7", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+testToken)

			w := httptest.NewRecorder()
			handler := NewHandler(HandlerOptions{Revoker: revoker, Token: testToken, RevokeTimeout: timeout})

			workers.Go(func() { handler.ServeHTTP(w, r); close(done) })

			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("revocation did not start")
			}

			if clientCancel {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("revocation did not stop")
			}

			if clientCancel {
				if w.Body.Len() != 0 {
					t.Fatal("disconnected operator received a body")
				}
			} else if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("deadline response = %d", w.Code)
			}
		})
	}
}
