package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatTokenLimit checks explicit token counts are bounded before backend work starts.
func TestChatTokenLimit(t *testing.T) {
	for _, tt := range []struct {
		name   string
		value  string
		status int
	}{
		{name: "minimum", value: "1", status: http.StatusNoContent},
		{name: "at limit", value: "8", status: http.StatusNoContent},
		{name: "above limit", value: "9", status: http.StatusBadRequest},
		{name: "zero", value: "0", status: http.StatusBadRequest},
		{name: "negative", value: "-1", status: http.StatusBadRequest},
		{name: "fractional", value: "1.5", status: http.StatusBadRequest},
		{name: "null", value: "null", status: http.StatusBadRequest},
		{name: "string", value: `"8"`, status: http.StatusBadRequest},
		{name: "overflow", value: "9223372036854775808", status: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := ` {"model":"test","messages":[],"max_tokens":` + tt.value + `,"custom":{"keep":true}} `
			called := false
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != body || r.ContentLength != int64(len(body)) {
					t.Errorf("explicit max_tokens changed the forwarded body: %q %v", got, err)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			handler := NewHandlerWithNonStreamingChatLimits(upstream, ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.status || called != (tt.status == http.StatusNoContent) {
				t.Fatalf("status = %d, upstream called = %v; want %d", recorder.Code, called, tt.status)
			}
		})
	}
}

// TestChatTokenLimitOmitted checks default insertion preserves existing JSON bytes and updates body length.
func TestChatTokenLimitOmitted(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty object", body: `{}`, want: `{"max_tokens":8}`},
		{name: "whitespace", body: " { \n }\t\n", want: " { \n \"max_tokens\":8}\t\n"},
		{name: "unknown fields", body: ` {"model":"test","messages":[],"custom":{"keep":true}} `,
			want: ` {"model":"test","messages":[],"custom":{"keep":true},"max_tokens":8} `},
		{name: "preserved raw values", body: `{"extra":{"number":9007199254740993,"text":"\u0061}"}}`,
			want: `{"extra":{"number":9007199254740993,"text":"\u0061}"},"max_tokens":8}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != tt.want || r.ContentLength != int64(len(tt.want)) {
					t.Errorf("default token request = %q, length %d, error %v; want %q", got, r.ContentLength, err, tt.want)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			handler := NewHandlerWithNonStreamingChatLimits(upstream, ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("omitted max_tokens was rejected: %d", recorder.Code)
			}
		})
	}
}
