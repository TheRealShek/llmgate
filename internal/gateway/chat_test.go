package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/proxy"
)

// TestChatForwarding checks that validation preserves the original body, optional fields, and backend response.
func TestChatForwarding(t *testing.T) {
	for _, stream := range []string{"", `,"stream":false`} {
		body := ` {"model":"test","messages":[{"role":"user","content":"hello"}],"custom":123` + stream + `} `
		upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != body || r.ContentLength != int64(len(body)) {
				t.Errorf("forwarded body or length changed: %q, %d, %v", got, r.ContentLength, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"choices":[]}`))
		})
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
		recorder := httptest.NewRecorder()
		NewHandlerWithChat(upstream, 1024).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"choices":[]}` {
			t.Fatalf("unexpected chat response: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}

// TestChatReadTimeout checks a stalled upload fails within the server deadline without reaching the backend.
func TestChatReadTimeout(t *testing.T) {
	upstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("incomplete upload reached upstream")
	})
	server := httptest.NewUnstartedServer(NewHandlerWithChat(upstream, 1024))
	server.Config.ReadTimeout = 50 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprint(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: gateway\r\nContent-Type: application/json\r\nContent-Length: 10\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled upload did not receive an error response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("stalled upload status = %d, want 400", response.StatusCode)
	}
}

// TestChatRejections checks invalid requests never reach the backend and receive JSON errors.
func TestChatRejections(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        string
		contentType string
		status      int
	}{
		{name: "empty body", contentType: "application/json", status: 400},
		{name: "malformed JSON", body: `{`, contentType: "application/json", status: 400},
		{name: "multiple JSON values", body: `{} {}`, contentType: "application/json", status: 400},
		{name: "array", body: `[]`, contentType: "application/json", status: 400},
		{name: "null", body: `null`, contentType: "application/json", status: 400},
		{name: "invalid stream type", body: `{"stream":"false"}`, contentType: "application/json", status: 400},
		{name: "null stream", body: `{"stream":null}`, contentType: "application/json", status: 400},
		{name: "streaming", body: `{"stream":true}`, contentType: "application/json", status: 501},
		{name: "too large", body: strings.Repeat("x", 65), contentType: "application/json", status: 413},
		{name: "missing content type", body: `{}`, status: 415},
		{name: "wrong content type", body: `{}`, contentType: "text/plain", status: 415},
		{name: "GET chat", contentType: "application/json", status: 405},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("rejected request reached upstream")
			})
			method := http.MethodPost
			if tt.name == "GET chat" {
				method = http.MethodGet
			}
			request := httptest.NewRequest(method, "/v1/chat/completions", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", tt.contentType)
			request.ContentLength = -1
			recorder := httptest.NewRecorder()
			NewHandlerWithChat(upstream, 64).ServeHTTP(recorder, request)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.status, recorder.Body.String())
			}
			if tt.status != 405 && recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatal("rejection did not return a JSON error")
			}
		})
	}
}

// TestChatBodyBoundary checks that a body exactly at the byte limit is accepted.
func TestChatBodyBoundary(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	body := "{}" + strings.Repeat(" ", 62)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	NewHandlerWithChat(upstream, 64).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("body at size limit was rejected: %d", recorder.Code)
	}
}

// TestChatProxyIntegration checks the real backend receives chat bodies and preserves its response.
func TestChatProxyIntegration(t *testing.T) {
	body := `{"model":"test","messages":[],"stream":false,"extra":123}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body || r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("unexpected backend request: %s %s %q %v", r.Method, r.URL.Path, got, err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	handler := NewHandlerWithChat(proxy.NewHandler(target, transport, time.Second), 1024)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"choices":[]}` {
		t.Fatalf("unexpected backend response: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestChatCancellation checks validation and buffering preserve client cancellation at the backend.
func TestChatCancellation(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	gateway := httptest.NewServer(NewHandlerWithChat(proxy.NewHandler(target, transport, 5*time.Second), 1024))
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	workers.Go(func() {
		response, err := gateway.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			t.Error("canceled request unexpectedly succeeded")
		}
		close(done)
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend request did not start")
	}
	cancel()
	for _, event := range []<-chan struct{}{stopped, done} {
		select {
		case <-event:
		case <-time.After(5 * time.Second):
			t.Fatal("chat request did not stop after cancellation")
		}
	}
}
