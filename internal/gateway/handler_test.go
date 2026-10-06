package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	handler := NewHandler()
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "healthy", method: http.MethodGet, path: "/healthz", status: http.StatusOK},
		{name: "unsupported method", method: http.MethodPost, path: "/healthz", status: http.StatusMethodNotAllowed},
		{name: "unknown route", method: http.MethodGet, path: "/missing", status: http.StatusNotFound},
		{name: "health path suffix", method: http.MethodGet, path: "/healthz/extra", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, tt.path, nil)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			if tt.status == http.StatusOK {
				if body := recorder.Body.String(); body != "ok\n" {
					t.Errorf("body = %q, want %q", body, "ok\n")
				}
				if contentType := recorder.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
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
