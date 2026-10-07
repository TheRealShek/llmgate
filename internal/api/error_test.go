package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteError checks error statuses, the JSON envelope, and escaping of message text.
func TestWriteError(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			detail := Error{
				Message: "backend said \"unavailable\"\ntry again",
				Type:    "server_error",
				Code:    "upstream_error",
			}
			recorder := httptest.NewRecorder()
			if err := WriteError(recorder, status, detail); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != status || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected response status or content type: %d %v", recorder.Code, recorder.Header())
			}
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not valid JSON: %v", err)
			}
			if body.Error.Message != detail.Message || body.Error.Type != detail.Type || body.Error.Code != detail.Code {
				t.Fatalf("error response did not preserve fields: %+v", body)
			}
		})
	}
}

type failedWriter struct {
	*httptest.ResponseRecorder
}

func (failedWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// TestWriteErrorFailure checks that a response-write failure reaches the caller without a second status.
func TestWriteErrorFailure(t *testing.T) {
	w := failedWriter{httptest.NewRecorder()}
	err := WriteError(w, http.StatusServiceUnavailable, Error{Message: "no backend configured", Type: "server_error", Code: "no_backend"})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error = %v, want closed pipe", err)
	}
	if w.Code != http.StatusServiceUnavailable || w.Body.Len() != 0 {
		t.Fatalf("unexpected failed-write response: %d %q", w.Code, w.Body.String())
	}
}
