package gateway

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/proxy"
)

// TestStreamingChatFlush checks chunks arrive before backend completion and outlive the JSON-response deadline.
func TestStreamingChatFlush(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), `"max_tokens":8`) {
			t.Errorf("stream request did not receive the configured token limit: %s %v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	nonStreaming := proxy.NewNonStreamingHandler(target, transport, 10*time.Millisecond)
	streaming := proxy.NewStreamingHandler(target, transport, time.Second)
	gateway := httptest.NewServer(NewHandlerWithBackends(nonStreaming, streaming, ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8}))
	t.Cleanup(gateway.Close)
	client := gateway.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: first\n" || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("first chunk did not arrive while backend was blocked: %q %v", first, err)
	}
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "\ndata: [DONE]\n\n" {
		t.Fatalf("stream did not complete after the JSON deadline: %q %v", rest, err)
	}
}

// TestStreamingChatContinuousChunks checks new bytes reset the idle wait throughout a longer stream.
func TestStreamingChatContinuousChunks(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for range 8 {
			select {
			case <-ticker.C:
				if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	gateway := newStreamingTestGateway(t, backend, time.Second, 300*time.Millisecond)
	response, err := gateway.Client().Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != strings.Repeat("data: x\n\n", 8) {
		t.Fatalf("active stream hit a total idle deadline: %q %v", body, err)
	}
}

// TestStreamingChatFailures checks header deadlines, idle gaps, and client disconnects stop backend work.
func TestStreamingChatFailures(t *testing.T) {
	for _, mode := range []string{"header timeout", "idle timeout", "client disconnect"} {
		t.Run(mode, func(t *testing.T) {
			stopped := make(chan struct{})
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if mode != "header timeout" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: first\n\n")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(stopped)
			})
			headerTimeout, idleTimeout := time.Second, time.Second
			if mode == "header timeout" {
				headerTimeout = 50 * time.Millisecond
			}
			if mode == "idle timeout" {
				idleTimeout = 50 * time.Millisecond
			}
			gateway := newStreamingTestGateway(t, backend, headerTimeout, idleTimeout)
			response, err := gateway.Client().Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if mode == "header timeout" {
				if response.StatusCode != http.StatusGatewayTimeout {
					t.Fatalf("header timeout status = %d", response.StatusCode)
				}
			} else {
				reader := bufio.NewReader(response.Body)
				if first, err := reader.ReadString('\n'); err != nil || first != "data: first\n" {
					t.Fatalf("first chunk = %q, error %v", first, err)
				}
				if mode == "client disconnect" {
					_ = response.Body.Close()
				} else if _, err := io.ReadAll(reader); err == nil {
					t.Fatal("stalled stream ended without a truncated-response error")
				}
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("backend work did not stop")
			}
		})
	}
}

// TestStreamingChatCancellationIsolation checks one disconnected stream cannot cancel a concurrent stream.
func TestStreamingChatCancellationIsolation(t *testing.T) {
	aStopped := make(chan struct{})
	bRelease := make(chan struct{})
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("id") == "A" {
			<-r.Context().Done()
			close(aStopped)
			return
		}
		select {
		case <-bRelease:
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})
	gateway := newStreamingTestGateway(t, backend, time.Second, time.Second)
	a, err := gateway.Client().Post(gateway.URL+"/v1/chat/completions?id=A", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Body.Close()
	b, err := gateway.Client().Post(gateway.URL+"/v1/chat/completions?id=B", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Body.Close()
	for _, response := range []*http.Response{a, b} {
		if first, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil || first != "data: first\n" {
			t.Fatalf("concurrent first chunk = %q, error %v", first, err)
		}
	}
	_ = a.Body.Close()
	select {
	case <-aStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected stream remained active")
	}
	close(bRelease)
	body, err := io.ReadAll(b.Body)
	if err != nil || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("other stream was interrupted: %q %v", body, err)
	}
}

func newStreamingTestGateway(t *testing.T, backendHandler http.Handler, headerTimeout, idleTimeout time.Duration) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(backendHandler)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = headerTimeout
	t.Cleanup(transport.CloseIdleConnections)
	nonStreaming := proxy.NewNonStreamingHandler(target, transport, 10*time.Millisecond)
	streaming := proxy.NewStreamingHandler(target, transport, idleTimeout)
	gateway := httptest.NewServer(NewHandlerWithBackends(nonStreaming, streaming, ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8}))
	t.Cleanup(gateway.Close)
	gateway.Client().Timeout = 5 * time.Second
	return gateway
}
