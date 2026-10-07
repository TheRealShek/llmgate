package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// TestHandlerForwarding checks target paths, query forwarding, headers, bodies, and backend statuses.
func TestHandlerForwarding(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/api/v1/models?limit=2" || r.Method != http.MethodGet {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Host == "gateway.example" {
					t.Error("upstream received the gateway host")
				}
				if r.Header.Get("X-Forwarded-For") != "" {
					t.Error("upstream received a spoofed forwarding header")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"data":[]}`))
			}))
			defer backend.Close()
			target, err := url.Parse(backend.URL + "/api")
			if err != nil {
				t.Fatal(err)
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			request := httptest.NewRequest(http.MethodGet, "http://gateway.example/v1/models?limit=2", nil)
			request.Header.Set("X-Forwarded-For", "spoofed")
			recorder := httptest.NewRecorder()
			NewHandler(target, transport, time.Second).ServeHTTP(recorder, request)
			if recorder.Code != status || recorder.Body.String() != `{"data":[]}` || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected proxy response: %d %s %s", recorder.Code, recorder.Header(), recorder.Body.String())
			}
		})
	}
}

// TestHandlerResponseBodyDeadline checks that a backend cannot stall indefinitely after sending headers.
func TestHandlerResponseBodyDeadline(t *testing.T) {
	stopped := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
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
	gateway := httptest.NewServer(NewHandler(target, transport, 100*time.Millisecond))
	t.Cleanup(gateway.Close)
	client := gateway.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Get(gateway.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("stalled response body unexpectedly completed")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("body deadline did not stop upstream work")
	}
}

// TestHandlerConnectionFailure checks that an unreachable backend returns a safe gateway error.
func TestHandlerConnectionFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	recorder := httptest.NewRecorder()
	NewHandler(target, transport, time.Second).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "{\"error\":{\"message\":\"upstream request failed\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n" {
		t.Fatalf("unexpected connection failure response: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestHandlerCancellation checks that client cancellation and a gateway deadline both stop upstream work.
func TestHandlerCancellation(t *testing.T) {
	for _, clientCancel := range []bool{true, false} {
		name := "gateway deadline"
		if clientCancel {
			name = "client cancellation"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			timeout := 100 * time.Millisecond
			if clientCancel {
				timeout = 5 * time.Second
			}
			gateway := httptest.NewServer(NewHandler(target, transport, timeout))
			t.Cleanup(gateway.Close)
			ctx, cancel := context.WithCancel(t.Context())
			var workers sync.WaitGroup
			t.Cleanup(func() { cancel(); workers.Wait() })
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL+"/v1/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{}, 1)
			workers.Go(func() {
				response, err := gateway.Client().Do(request)
				if clientCancel {
					if err == nil {
						t.Error("canceled client request unexpectedly succeeded")
					}
				} else if err != nil {
					t.Errorf("deadline request: %v", err)
				} else if response.StatusCode != http.StatusGatewayTimeout {
					t.Errorf("deadline status = %d, want 504", response.StatusCode)
				}
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
				done <- struct{}{}
			})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream request did not start")
			}
			if clientCancel {
				cancel()
			}
			for _, event := range []<-chan struct{}{stopped, done} {
				select {
				case <-event:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not finish after cancellation")
				}
			}
		})
	}
}
