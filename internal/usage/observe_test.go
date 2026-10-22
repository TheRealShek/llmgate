package usage

import (
	"strings"
	"testing"
	"time"
)

// TestSSEFraming checks observation is independent of HTTP writes and all SSE line endings.
func TestSSEFraming(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, size := range []int{1, 7, 4096} {
			t.Run(ending+"/"+time.Duration(size).String(), func(t *testing.T) {
				body := "\xef\xbb\xbf: prefill\n\ndata: {\"choices\":[]}\n\n" +
					"data: {\"usage\":\ndata: {\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n" +
					"data: [DONE]\n\n"
				body = strings.ReplaceAll(body, "\n", ending)

				o := observer{streaming: true, maxBytes: 1024, firstLine: true}
				for offset := 0; offset < len(body); offset += size {
					o.feed([]byte(body[offset:min(offset+size, len(body))]), time.Now())
				}

				if o.chunks != 2 || !o.done || o.firstData == nil || o.failed ||
					o.prompt == nil || *o.prompt != 7 || o.completion == nil || *o.completion != 2 {
					t.Fatalf("SSE framing changed observation: %+v", o)
				}
			})
		}
	}
}

// TestSSETermination checks comments and terminal markers are not counted as generated chunks.
func TestSSETermination(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		chunks     int64
		done, fail bool
	}{
		{name: "comments", body: ": waiting\n\n"},
		{name: "terminal only", body: "data: [DONE]\n\n", done: true},
		{name: "incomplete event", body: "data: {}\n"},
		{name: "ordinary event", body: "data: {}\n\n", chunks: 1},
		{name: "empty data event", body: "data\n\n", chunks: 1},
		{name: "JSON error", body: "data: {\"error\":{}}\n\n", chunks: 1, fail: true},
		{name: "typed error", body: "event: error\ndata: unavailable\n\n", chunks: 1, fail: true},
		{name: "after terminal", body: "data: [DONE]\n\ndata: {}\n\n", done: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := observer{streaming: true, maxBytes: 1024, firstLine: true}
			o.feed([]byte(tt.body), time.Now())

			if o.chunks != tt.chunks || o.done != tt.done || o.failed != tt.fail ||
				(o.firstData != nil) != (tt.chunks > 0) {
				t.Fatalf("unexpected terminal observation: %+v", o)
			}
		})
	}
}

// TestUnknownTokenCounts checks missing, partial, malformed, negative, and overflowing usage stays unknown.
func TestUnknownTokenCounts(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"usage":null}`, `{"usage":{}}`, `{"usage":{"prompt_tokens":1}}`,
		`{"usage":{"prompt_tokens":-1,"completion_tokens":2}}`,
		`{"usage":{"prompt_tokens":1,"completion_tokens":2.5}}`,
		`{"usage":{"prompt_tokens":1,"completion_tokens":9223372036854775808}}`,
		`{"usage":{"prompt_tokens":"1","completion_tokens":2}}`, `not JSON`,
	} {
		t.Run(body, func(t *testing.T) {
			o := observer{}
			o.inspectJSON([]byte(body))

			if o.prompt != nil || o.completion != nil {
				t.Fatal("invalid usage became a token charge")
			}
		})
	}

	o := observer{}
	o.inspectJSON([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`))

	if o.prompt == nil || o.completion == nil || *o.prompt != 0 || *o.completion != 0 {
		t.Fatal("explicit zero counts were confused with missing usage")
	}
}

// TestObservationBounds checks oversized payloads cannot grow observation state or block later events.
func TestObservationBounds(t *testing.T) {
	const bound = 128

	stream := observer{streaming: true, maxBytes: bound, firstLine: true}
	for range 1000 {
		stream.feed([]byte("data: "+strings.Repeat("x", 1024)+"\n\n"), time.Now())

		if len(stream.line) > bound || len(stream.data) > bound {
			t.Fatal("SSE observation exceeded its configured bound")
		}
	}

	stream.feed([]byte("data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"+
		"data: [DONE]\n\n"), time.Now())

	if stream.chunks != 1001 || !stream.done || stream.prompt == nil || *stream.prompt != 7 {
		t.Fatal("oversized event prevented later usage observation")
	}

	json := observer{maxBytes: bound}
	for range 1000 {
		json.feed([]byte(strings.Repeat("x", 1024)), time.Now())
	}

	if !json.overflow || len(json.body) != 0 {
		t.Fatal("oversized JSON response was retained")
	}
}

// TestUnobservableUsageClearsCounts checks unreadable later events cannot leave stale usage looking final.
func TestUnobservableUsageClearsCounts(t *testing.T) {
	for _, body := range []string{"data: invalid JSON\n\n", "data: " + strings.Repeat("x", 256) + "\n\n"} {
		o := observer{streaming: true, maxBytes: 128, firstLine: true}
		o.feed([]byte("data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"), time.Now())
		o.feed([]byte(body), time.Now())
		o.feed([]byte("data: [DONE]\n\n"), time.Now())

		if o.prompt != nil || o.completion != nil {
			t.Fatal("unreadable later event left earlier counts looking final")
		}
	}
}

// TestTruncatedSSEFields checks a tiny observation bound cannot invent data or error fields.
func TestTruncatedSSEFields(t *testing.T) {
	o := observer{streaming: true, maxBytes: 4, firstLine: true}
	o.feed([]byte("dataIsNotAField\n\n"), time.Now())

	if o.chunks != 0 || o.firstData != nil {
		t.Fatal("truncated field name became a data event")
	}

	o = observer{streaming: true, maxBytes: len("event: error"), firstLine: true}
	o.feed([]byte("event: errorWithSuffix\ndata: {}\n\n"), time.Now())

	if o.failed {
		t.Fatal("truncated event type became a backend error")
	}
}
