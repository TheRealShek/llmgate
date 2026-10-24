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
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/store"
	"github.com/therealshek/llmgate/internal/usage"
)

// TestChatForwarding checks that validation preserves the original body, optional fields, and
// backend response.
func TestChatForwarding(t *testing.T) {
	for _, stream := range []string{"", `,"stream":false`} {
		body := ` {"model":"test","messages":[{"role":"user","content":"hello"}],` +
			`"custom":123` + stream + `} `
		upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := io.ReadAll(r.Body)
			if err != nil || string(got) != body || r.ContentLength != int64(len(body)) {
				t.Errorf("forwarded body or length changed: %q, %d, %v", got, r.ContentLength, err)
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"choices":[]}`))
		})
		request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json; charset=utf-8")

		recorder := httptest.NewRecorder()
		NewHandler(HandlerOptions{
			NonStreaming:   upstream,
			Authentication: testAuthenticator(),
			RequestLimits:  testRequestLimits(),
			Quota:          testTokenQuota(),
			Usage:          testUsageRecorder(),
			Limits:         ChatLimits{MaxBodyBytes: 1024},
		}).ServeHTTP(recorder, request)

		if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"choices":[]}` {
			t.Fatalf("unexpected chat response: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}

// TestChatReadTimeout checks a stalled upload fails within the server deadline without reaching
// the backend.
func TestChatReadTimeout(t *testing.T) {
	upstream := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("incomplete upload reached upstream")
	})
	server := httptest.NewUnstartedServer(NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024},
	}))
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

	_, err = fmt.Fprint(
		conn,
		"POST /v1/chat/completions HTTP/1.1\r\n"+
			"Host: gateway\r\n"+
			"Authorization: Bearer test-key\r\n"+
			"Content-Type: application/json\r\n"+
			"Content-Length: 10\r\n\r\n{",
	)
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
		{
			name:        "invalid stream type",
			body:        `{"stream":"false"}`,
			contentType: "application/json",
			status:      400,
		},
		{name: "null stream", body: `{"stream":null}`, contentType: "application/json", status: 400},
		{
			name:        "streaming not configured",
			body:        `{"stream":true}`,
			contentType: "application/json",
			status:      501,
		},
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

			request := authenticatedRequest(method, "/v1/chat/completions", strings.NewReader(tt.body))
			request.Header.Set("Content-Type", tt.contentType)
			request.ContentLength = -1
			recorder := httptest.NewRecorder()
			NewHandler(HandlerOptions{
				NonStreaming:   upstream,
				Authentication: testAuthenticator(),
				RequestLimits:  testRequestLimits(),
				Quota:          testTokenQuota(),
				Usage:          testUsageRecorder(),
				Limits:         ChatLimits{MaxBodyBytes: 64},
			}).ServeHTTP(recorder, request)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.status, recorder.Body.String())
			}

			if tt.status != 405 && recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatal("rejection did not return a JSON error")
			}

			if tt.name == "streaming not configured" &&
				!strings.Contains(recorder.Body.String(), "streaming chat is not configured") {
				t.Fatalf("missing streaming backend returned an obsolete message: %s", recorder.Body.String())
			}
		})
	}
}

// TestChatBodyBoundary checks that a body exactly at the byte limit is accepted.
func TestChatBodyBoundary(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	body := "{}" + strings.Repeat(" ", 62)
	request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	NewHandler(HandlerOptions{
		NonStreaming:   upstream,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 64},
	}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("body at size limit was rejected: %d", recorder.Code)
	}
}

// TestChatProxyIntegration checks the real backend receives chat bodies and preserves its response.
func TestChatProxyIntegration(t *testing.T) {
	body := `{"model":"test","messages":[],"stream":false,"extra":123}`

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil ||
			string(got) != body ||
			r.Method != http.MethodPost ||
			r.URL.Path != "/api/v1/chat/completions" {
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

	nonStreaming := proxy.NewNonStreamingHandler(target, transport, time.Second)
	handler := NewHandler(HandlerOptions{
		NonStreaming:   nonStreaming,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024},
	})
	recorder := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
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
	nonStreaming := proxy.NewNonStreamingHandler(target, transport, 5*time.Second)
	handler := NewHandler(HandlerOptions{
		NonStreaming:   nonStreaming,
		Authentication: testAuthenticator(),
		RequestLimits:  testRequestLimits(),
		Quota:          testTokenQuota(),
		Usage:          testUsageRecorder(),
		Limits:         ChatLimits{MaxBodyBytes: 1024},
	})
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithCancel(t.Context())

	var workers sync.WaitGroup

	t.Cleanup(func() { cancel(); workers.Wait() })

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"stream":false}`),
	)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Authorization", "Bearer test-key")
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
			handler := NewHandler(HandlerOptions{
				NonStreaming:   upstream,
				Authentication: testAuthenticator(),
				RequestLimits:  testRequestLimits(),
				Quota:          testTokenQuota(),
				Usage:          testUsageRecorder(),
				Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})
			request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.status || called != (tt.status == http.StatusNoContent) {
				t.Fatalf("status = %d, upstream called = %v; want %d", recorder.Code, called, tt.status)
			}
		})
	}
}

// TestChatTokenLimitOmitted checks default insertion preserves existing JSON bytes and updates
// body length.
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
					t.Errorf(
						"default token request = %q, length %d, error %v; want %q",
						got,
						r.ContentLength,
						err,
						tt.want,
					)
				}

				w.WriteHeader(http.StatusNoContent)
			})
			handler := NewHandler(HandlerOptions{
				NonStreaming:   upstream,
				Authentication: testAuthenticator(),
				RequestLimits:  testRequestLimits(),
				Quota:          testTokenQuota(),
				Usage:          testUsageRecorder(),
				Limits:         ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})
			request := authenticatedRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(tt.body),
			)
			request.Header.Set("Content-Type", "application/json")

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("omitted max_tokens was rejected: %d", recorder.Code)
			}
		})
	}
}

type captureUsage func(context.Context, store.Usage) error

func (f captureUsage) RecordUsage(ctx context.Context, record store.Usage) error {
	return f(ctx, record)
}

// TestUsageScope checks only validated inference attempts receive records, preserving request bodies.
func TestUsageScope(t *testing.T) {
	for _, tt := range []struct {
		name, path, body string
		status, records  int
	}{
		{name: "models", path: "/v1/models", status: 200},
		{name: "malformed JSON", path: "/v1/chat/completions", body: `{`, status: 400},
		{name: "invalid token limit", path: "/v1/chat/completions", body: `{"max_tokens":0}`, status: 400},
		{name: "unsupported stream", path: "/v1/chat/completions", body: `{"stream":true}`, status: 501},
		{name: "inference", path: "/v1/chat/completions", body: `{"model":"test-model","messages":[],"max_tokens":8}`, status: 200, records: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			count := 0
			recorder := usage.NewRecorder(usage.Options{
				Store: captureUsage(func(_ context.Context, record store.Usage) error {
					count++

					if record.Model == nil || *record.Model != "test-model" || record.KeyID != 1 {
						t.Error("validated metadata did not reach usage recording")
					}

					return nil
				}), Backend: "test", WriteTimeout: time.Second, MaxBytes: 1024,
			})
			backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/v1/chat/completions" && string(body) != tt.body {
					t.Error("recording changed the request body")
				}

				_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
			})
			handler := NewHandler(HandlerOptions{
				Authentication: testAuthenticator(), RequestLimits: testRequestLimits(),
				NonStreaming: backend, Quota: testTokenQuota(), Usage: recorder, Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
			})

			method := http.MethodPost
			if tt.path == "/v1/models" {
				method = http.MethodGet
			}

			r := authenticatedRequest(method, tt.path, strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			if count != tt.records || w.Code != tt.status {
				t.Fatal("usage scope or HTTP validation changed", count, w.Code)
			}
		})
	}
}

// TestMissingUsageRecorder checks incomplete construction cannot silently omit inference recording.
func TestMissingUsageRecorder(t *testing.T) {
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing recorder admitted inference") })
	handler := NewHandler(HandlerOptions{
		Authentication: testAuthenticator(), RequestLimits: testRequestLimits(),
		NonStreaming: backend, Quota: testTokenQuota(), Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
	})
	r := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "usage_unavailable") {
		t.Fatal("missing recorder did not fail closed", w.Code)
	}
}

// discardUsage keeps route, authentication, and logging tests independent of persistence.
type discardUsage struct{}

func (discardUsage) RecordUsage(context.Context, store.Usage) error { return nil }

func testUsageRecorder() *usage.Recorder {
	return usage.NewRecorder(usage.Options{
		Store: discardUsage{}, Backend: "test", WriteTimeout: time.Second, MaxBytes: 64 << 10,
	})
}

type tokenQuotaFunc func(context.Context, store.Key, []byte) (quota.Admission, error)

func (f tokenQuotaFunc) Admit(ctx context.Context, key store.Key, body []byte) (quota.Admission, error) {
	return f(ctx, key, body)
}

// TestChatQuotaRejections checks quota errors stop inference with safe status and retry guidance.
func TestChatQuotaRejections(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
		retry  string
	}{
		{name: "exhausted", err: &quota.Exhausted{RetryAfter: 1500 * time.Millisecond}, status: 429, retry: "2"},
		{name: "unsupported", err: quota.ErrInvalidRequest, status: 400},
		{name: "Redis failure", err: quota.ErrUnavailable, status: 503},
		{name: "invalid key limit", err: quota.ErrInvalidLimit, status: 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rejected quota reached backend") })
			handler := NewHandler(HandlerOptions{
				NonStreaming: backend, Authentication: testAuthenticator(), RequestLimits: testRequestLimits(),
				Usage: testUsageRecorder(), Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8},
				Quota: tokenQuotaFunc(func(context.Context, store.Key, []byte) (quota.Admission, error) { return quota.Admission{}, tt.err }),
			})
			request := authenticatedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			if w.Code != tt.status || w.Header().Get("Retry-After") != tt.retry || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w.Code, w.Header())
			}
		})
	}
}

// TestMissingTokenQuota checks omitted construction fails closed for chat while models remain usable.
func TestMissingTokenQuota(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Error("missing quota admitted inference")
		}

		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandler(HandlerOptions{NonStreaming: backend, Authentication: testAuthenticator(), RequestLimits: testRequestLimits(), Usage: testUsageRecorder(), Limits: ChatLimits{MaxBodyBytes: 1024, MaxTokens: 8}})

	for _, path := range []string{"/v1/chat/completions", "/v1/models"} {
		method := http.MethodPost
		want := http.StatusServiceUnavailable

		if path == "/v1/models" {
			method = http.MethodGet
			want = http.StatusNoContent
		}

		request := authenticatedRequest(method, path, strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)

		if w.Code != want {
			t.Fatal(path, w.Code)
		}
	}
}
