package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Counter uses the same backend chat parser and tokenizer as inference.
// Startup owns its transport; the caller supplies a bounded context for both calls.
type Counter struct {
	target    url.URL
	transport http.RoundTripper
	maxBytes  int64
}

// NewCounter uses an immutable copy of the upstream URL and a bounded response size.
func NewCounter(target *url.URL, transport http.RoundTripper, maxBytes int64) *Counter {
	counter := &Counter{transport: transport, maxBytes: maxBytes}
	if target != nil {
		counter.target = *target
	}

	return counter
}

// Count renders the complete request, then tokenizes with inference's special-token options.
// This is verified for text chat on llama.cpp b11681, not arbitrary inference backends.
func (c *Counter) Count(ctx context.Context, body []byte) (int64, error) {
	var rendered struct {
		Prompt *string `json:"prompt"`
	}
	if err := c.post(ctx, "apply-template", body, &rendered); err != nil {
		return 0, err
	}

	if rendered.Prompt == nil {
		return 0, fmt.Errorf("decode rendered prompt: %w", ErrUnavailable)
	}

	payload, err := json.Marshal(struct {
		Content      string `json:"content"`
		AddSpecial   bool   `json:"add_special"`
		ParseSpecial bool   `json:"parse_special"`
	}{Content: *rendered.Prompt, AddSpecial: true, ParseSpecial: true})
	if err != nil {
		return 0, fmt.Errorf("encode prompt: %w", ErrUnavailable)
	}

	var tokenized struct {
		Tokens *[]int64 `json:"tokens"`
	}
	if err := c.post(ctx, "tokenize", payload, &tokenized); err != nil {
		return 0, err
	}

	if tokenized.Tokens == nil {
		return 0, fmt.Errorf("decode prompt tokens: %w", ErrUnavailable)
	}

	for _, id := range *tokenized.Tokens {
		if id < 0 {
			return 0, fmt.Errorf("validate prompt token: %w", ErrUnavailable)
		}
	}

	return int64(len(*tokenized.Tokens)), nil
}

func (c *Counter) post(ctx context.Context, path string, body []byte, result any) error {
	if c == nil || c.transport == nil || c.target.Host == "" || c.maxBytes <= 0 {
		return fmt.Errorf("configure prompt counter: %w", ErrUnavailable)
	}

	target := c.target.JoinPath(path)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create prompt count request: %w", ErrUnavailable)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := c.transport.RoundTrip(request)
	if err != nil {
		return fmt.Errorf("count prompt tokens: %w", ErrUnavailable)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if path == "apply-template" && response.StatusCode == http.StatusBadRequest {
			return fmt.Errorf("render chat request: %w", ErrInvalidRequest)
		}

		return fmt.Errorf("prompt count response: %w", ErrUnavailable)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBytes+1))
	if err != nil || int64(len(data)) > c.maxBytes {
		return fmt.Errorf("read prompt count response: %w", ErrUnavailable)
	}

	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode prompt count response: %w", ErrUnavailable)
	}

	return nil
}
