package admin

import (
	"bytes"
	"context"
	"errors"
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
