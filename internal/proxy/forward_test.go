package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestGatewayCredentialsStayPrivate checks both proxy modes strip credentials without mutating input.
func TestGatewayCredentialsStayPrivate(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) != 0 {
			t.Error("gateway credential reached backend")
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	for _, tt := range []struct {
		name    string
		handler http.Handler
	}{
		{
			name:    "non-streaming",
			handler: NewNonStreamingHandler(target, transport, time.Second),
		},
		{name: "streaming", handler: NewStreamingHandler(target, transport, time.Second)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", "Bearer gateway-secret")

			w := httptest.NewRecorder()
			tt.handler.ServeHTTP(w, r)

			if w.Code != http.StatusNoContent {
				t.Fatalf("proxy status = %d", w.Code)
			}

			if r.Header.Get("Authorization") != "Bearer gateway-secret" {
				t.Error("proxy mutated inbound credential")
			}
		})
	}
}
