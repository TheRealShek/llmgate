package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/admin"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/store"
)

// TestAdminRevocationAndStreams checks persisted revocation cancels only the target key's upstream stream.
func TestAdminRevocationAndStreams(t *testing.T) {
	pool, _ := authenticationTestPool(t)
	keys := store.NewKeyStore(pool)

	targetKey, targetSecret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "target", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, otherSecret, err := keys.CreateKey(t.Context(), store.CreateKeyParams{
		Name: "other", RequestsPerMin: 60, TokensPerDay: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	targetStopped := make(chan struct{})
	otherStopped := make(chan struct{})
	// The test backend receives gateway requests with credentials stripped. It
	// flushes one SSE event and waits for upstream cancellation before releasing.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("backend received gateway credentials")
		}

		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}

		_, _ = io.Copy(io.Discard, r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()

		if r.Header.Get("X-Test-Stream") == "target" {
			close(targetStopped)
		} else {
			close(otherStopped)
		}
	}))
	t.Cleanup(backend.Close)

	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)

	authentication := auth.NewAuthenticator(keys, auth.Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 1024})
	public := httptest.NewServer(gateway.NewHandler(gateway.HandlerOptions{
		Backends: []gateway.Backend{{
			Name:         "primary",
			Admission:    testInferenceQueue(),
			NonStreaming: proxy.NewNonStreamingHandler(target, transport, time.Second),
			Streaming:    proxy.NewStreamingHandler(target, transport, time.Second),
			Quota:        testTokenQuota(),
			Usage:        testUsageRecorder()}},
		Health: testBackendHealth{},

		Authentication: authentication,
		RequestLimits:  testRequestLimits(),

		Limits: gateway.ChatLimits{MaxBodyBytes: 1024, MaxTokens: 128},
	}))
	t.Cleanup(public.Close)
	client := public.Client()
	client.Timeout = 5 * time.Second
	token := strings.Repeat("a", 64)
	private := httptest.NewServer(admin.NewHandler(admin.HandlerOptions{
		Keys:          keys,
		Revoker:       authentication,
		Token:         token,
		MaxBodyBytes:  1024,
		CreateTimeout: time.Second,
		RevokeTimeout: time.Second,
	}))
	t.Cleanup(private.Close)

	responses := make(map[string]*http.Response)

	for name, secret := range map[string]string{"target": targetSecret, "other": otherSecret} {
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		r, err := http.NewRequestWithContext(ctx, http.MethodPost, public.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Test-Stream", name)

		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = response.Body.Close() })

		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d", response.StatusCode)
		}

		line, err := bufio.NewReader(response.Body).ReadString('\n')
		if err != nil || !strings.Contains(line, "hello") {
			t.Fatal("stream did not deliver first content")
		}

		responses[name] = response
	}

	deleteKey := func(base, id, credential string) int {
		t.Helper()

		r, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, base+"/v1/admin/keys/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", "Bearer "+credential)

		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()

		if response.StatusCode == http.StatusNoContent {
			body, _ := io.ReadAll(response.Body)
			if len(body) != 0 {
				t.Error("successful revocation returned a body")
			}
		}

		return response.StatusCode
	}

	id := strconv.FormatInt(targetKey.ID, 10)
	if status := deleteKey(public.URL, id, token); status != http.StatusNotFound {
		t.Fatalf("public revocation status = %d", status)
	}

	if status := deleteKey(private.URL, id, targetSecret); status != http.StatusUnauthorized {
		t.Fatalf("inference credential revocation status = %d", status)
	}

	if status := deleteKey(private.URL, id, token); status != http.StatusNoContent {
		t.Fatalf("revocation status = %d", status)
	}

	select {
	case <-targetStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not cancel target inference")
	}

	body, _ := io.ReadAll(responses["target"].Body)
	if strings.Contains(string(body), "[DONE]") {
		t.Fatal("revoked stream appeared to complete normally")
	}

	select {
	case <-otherStopped:
		t.Fatal("revocation canceled another key")
	default:
	}

	var first, second time.Time

	if err := pool.QueryRow(t.Context(), "SELECT revoked_at FROM api_keys WHERE id=$1", targetKey.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}

	if status := deleteKey(private.URL, id, token); status != http.StatusNoContent {
		t.Fatal("repeated revocation failed", status)
	}

	if err := pool.QueryRow(t.Context(), "SELECT revoked_at FROM api_keys WHERE id=$1", targetKey.ID).Scan(&second); err != nil {
		t.Fatal(err)
	}

	if !first.Equal(second) {
		t.Fatal("repeated revocation changed its timestamp")
	}

	if status := deleteKey(private.URL, "9223372036854775807", token); status != http.StatusNotFound {
		t.Fatalf("unknown key status = %d", status)
	}

	for secret, want := range map[string]int{targetSecret: http.StatusUnauthorized, otherSecret: http.StatusOK} {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", "Bearer "+secret)

		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("post-revocation model status = %d, want %d", response.StatusCode, want)
		}
	}
}
