package quota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCounter verifies complete request rendering, special tokens, URL prefixes, and credential isolation.
func TestCounter(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Error("wrong method or gateway credential forwarded")
		}

		switch r.URL.Path {
		case "/prefix/apply-template":
			data, _ := io.ReadAll(r.Body)
			if string(data) != chatBody {
				t.Error("counting request options changed")
			}

			_, _ = io.WriteString(w, `{"prompt":"formatted prompt"}`)
		case "/prefix/tokenize":
			var data map[string]any
			if json.NewDecoder(r.Body).Decode(&data) != nil || data["add_special"] != true || data["parse_special"] != true || data["content"] != "formatted prompt" {
				t.Error("inference tokenization options missing")
			}

			_, _ = io.WriteString(w, `{"tokens":[1,2,3]}`)
		default:
			t.Error("wrong counting URL", r.URL.Path)
		}
	}))
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL + "/prefix")
	counter := NewCounter(target, backend.Client().Transport, 1024)

	got, err := counter.Count(t.Context(), []byte(chatBody))
	if err != nil || got != 3 || calls != 2 {
		t.Fatal(got, calls, err)
	}
}

// TestCounterFailures checks malformed, excessive, failed, and hung count responses are rejected.
func TestCounterFailures(t *testing.T) {
	for _, tt := range []struct {
		name, prompt, tokens string
		status               int
		invalid              bool
	}{
		{name: "invalid request", status: 400, invalid: true},
		{name: "backend failed", status: 500},
		{name: "missing prompt", prompt: `{}`},
		{name: "bad JSON", prompt: `{`},
		{name: "too large", prompt: `{"prompt":"` + strings.Repeat("x", 128) + `"}`},
		{name: "missing tokens", prompt: `{"prompt":"x"}`, tokens: `{}`},
		{name: "negative token", prompt: `{"prompt":"x"}`, tokens: `{"tokens":[-1]}`},
		{name: "noninteger token", prompt: `{"prompt":"x"}`, tokens: `{"tokens":[1.5]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}

				if r.URL.Path == "/apply-template" {
					_, _ = io.WriteString(w, tt.prompt)
				} else {
					_, _ = io.WriteString(w, tt.tokens)
				}
			}))
			t.Cleanup(backend.Close)
			target, _ := url.Parse(backend.URL)
			_, err := NewCounter(target, backend.Client().Transport, 100).Count(t.Context(), []byte(chatBody))

			want := ErrUnavailable
			if tt.invalid {
				want = ErrInvalidRequest
			}

			if !errors.Is(err, want) {
				t.Fatal(err)
			}
		})
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	if _, err := NewCounter(target, backend.Client().Transport, 100).Count(ctx, []byte(chatBody)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// TestLlamaPromptCounts compares rendered counts with actual JSON usage on the pinned backend.
func TestLlamaPromptCounts(t *testing.T) {
	address := os.Getenv("LLMGATE_TEST_LLAMA_URL")
	if address == "" {
		t.Skip("set LLMGATE_TEST_LLAMA_URL to a b11681 GLM backend")
	}

	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	counter := NewCounter(target, transport, 8<<20)

	for _, text := range []string{"Hello", "नमस्ते, explain Go goroutines.", "Count from one to ten."} {
		body, _ := json.Marshal(map[string]any{"model": "glm-edge-4b-chat", "messages": []map[string]string{{"role": "system", "content": "Be concise."}, {"role": "user", "content": text}}, "max_tokens": 3, "temperature": 0})
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)

		count, err := counter.Count(ctx, body)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.JoinPath("v1/chat/completions").String(), strings.NewReader(string(body)))
		request.Header.Set("Content-Type", "application/json")

		response, err := transport.RoundTrip(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		var result struct {
			Usage struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			}
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			}
		}

		err = json.NewDecoder(response.Body).Decode(&result)
		_ = response.Body.Close()

		cancel()

		if err != nil || response.StatusCode != 200 || count != result.Usage.Prompt || result.Usage.Completion > 3 || len(result.Choices) != 1 || result.Choices[0].FinishReason != "length" {
			t.Fatal("backend count or cap differed", count, result, err)
		}
	}
}
