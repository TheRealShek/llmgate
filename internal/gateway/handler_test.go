package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandler checks the health response and routing so unsupported methods and
// unrelated paths cannot silently pass health checks.
func TestHandler(t *testing.T) {
	handler := NewHandler(HandlerOptions{
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Usage:          testUsageRecorder(),
	})

	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "healthy", method: http.MethodGet, path: "/v1/healthz", status: http.StatusOK},
		{
			name:   "unsupported method",
			method: http.MethodPost,
			path:   "/v1/healthz",
			status: http.StatusMethodNotAllowed,
		},
		{name: "unknown route", method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
		{
			name:   "health path suffix",
			method: http.MethodGet,
			path:   "/v1/healthz/extra",
			status: http.StatusNotFound,
		},
		{
			name:   "unversioned health",
			method: http.MethodGet,
			path:   "/healthz",
			status: http.StatusNotFound,
		},
		{
			name:   "unversioned models",
			method: http.MethodGet,
			path:   "/models",
			status: http.StatusNotFound,
		},
		{
			name:   "unsupported API version",
			method: http.MethodGet,
			path:   "/v2/healthz",
			status: http.StatusNotFound,
		},
		{
			name:   "no backend",
			method: http.MethodGet,
			path:   "/v1/models",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "readiness without backend",
			method: http.MethodGet,
			path:   "/v1/readyz",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "readiness unsupported method",
			method: http.MethodPost,
			path:   "/v1/readyz",
			status: http.StatusMethodNotAllowed,
		},
		{
			name:   "unversioned readiness",
			method: http.MethodGet,
			path:   "/readyz",
			status: http.StatusNotFound,
		},
		{
			name:   "chat without backend",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			status: http.StatusServiceUnavailable,
		},
		{
			name:   "models unsupported method",
			method: http.MethodPost,
			path:   "/v1/models",
			status: http.StatusMethodNotAllowed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := authenticatedRequest(tt.method, tt.path, nil)
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}

			if tt.status == http.StatusOK {
				if body := recorder.Body.String(); body != "ok\n" {
					t.Errorf("body = %q, want %q", body, "ok\n")
				}

				contentType := recorder.Header().Get("Content-Type")
				if contentType != "text/plain; charset=utf-8" {
					t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", contentType)
				}
			}

			if tt.status == http.StatusMethodNotAllowed {
				if allow := recorder.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Errorf("Allow = %q, want GET, HEAD", allow)
				}
			}
		})
	}
}

// TestHandlerModels checks model route integration without affecting the health route.
func TestHandlerModels(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("models"))
	})
	handler := NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/models", nil))

	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "models" {
		t.Fatalf("model route response: %d %s", recorder.Code, recorder.Body.String())
	}
}
