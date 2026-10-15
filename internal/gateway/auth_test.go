package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProtectedRoutes checks auth runs before validation or proxying and leaves public routes alone.
func TestProtectedRoutes(t *testing.T) {
	backendCalls := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls++

		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Streaming:      upstream,
		Readiness:      upstream,
		Authentication: testAuthenticator(),
		Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{
			name:   "models require key",
			method: http.MethodGet,
			path:   "/v1/models",
			status: 401,
		},
		{
			name:   "HEAD models require key",
			method: http.MethodHead,
			path:   "/v1/models",
			status: 401,
		},
		{
			name:   "chat authenticates before validation",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			body:   "malformed",
			status: 401,
		},
		{name: "public health", method: http.MethodGet, path: "/v1/healthz", status: 200},
		{name: "public readiness", method: http.MethodGet, path: "/v1/readyz", status: 204},
		{name: "unknown route", method: http.MethodGet, path: "/missing", status: 404},
		{
			name:   "unsupported method",
			method: http.MethodGet,
			path:   "/v1/chat/completions",
			status: 405,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))

			if w.Code != tt.status {
				t.Errorf("status %d, want %d", w.Code, tt.status)
			}
		})
	}

	if backendCalls != 1 {
		t.Fatalf("only readiness should reach backend: calls = %d", backendCalls)
	}
}
