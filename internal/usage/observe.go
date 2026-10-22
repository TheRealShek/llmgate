package usage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// responseWriter observes only bytes accepted by the client-facing writer.
// It never delays forwarding until a whole SSE event or JSON body has arrived.
type responseWriter struct {
	http.ResponseWriter
	observer *observer
	status   int
	writeErr error
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= 200 && w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.observer.feed(p[:n], time.Now())
	}

	if err != nil {
		w.writeErr = err
	}

	return n, err
}

// FlushError forwards the reverse proxy's flush to the underlying client writer.
// It changes no observation state because proxy header flushes may run concurrently.
func (w *responseWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseWriter) Flush() { _ = w.FlushError() }

type observer struct {
	streaming  bool
	maxBytes   int
	body       []byte
	overflow   bool
	line       []byte
	lineFull   bool
	data       []byte
	hasData    bool
	eventFull  bool
	eventErr   bool
	skipLF     bool
	firstLine  bool
	done       bool
	failed     bool
	chunks     int64
	firstData  *time.Time
	prompt     *int64
	completion *int64
}

func (o *observer) feed(p []byte, at time.Time) {
	if !o.streaming {
		if o.overflow {
			return
		}

		if len(p) > o.maxBytes-len(o.body) {
			o.body = nil
			o.overflow = true

			return
		}

		o.body = append(o.body, p...)

		return
	}

	for _, char := range p {
		if o.skipLF {
			o.skipLF = false

			if char == '\n' {
				continue
			}
		}

		if char == '\r' || char == '\n' {
			o.finishLine(at)
			o.skipLF = char == '\r'

			continue
		}

		if len(o.line) < o.maxBytes {
			o.line = append(o.line, char)
		} else {
			o.lineFull = true
		}
	}
}

func (o *observer) finishLine(at time.Time) {
	line := o.line
	if o.firstLine {
		line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		o.firstLine = false
	}

	defer func() { o.line = o.line[:0]; o.lineFull = false }()

	if len(line) == 0 && !o.lineFull {
		o.finishEvent(at)
		return
	}

	field, value, hasColon := bytes.Cut(line, []byte(":"))
	if o.lineFull && !hasColon {
		return
	}

	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}

	if bytes.Equal(field, []byte("data")) {
		if o.hasData && !o.eventFull {
			if len(o.data) < o.maxBytes {
				o.data = append(o.data, '\n')
			} else {
				o.eventFull = true
			}
		}

		o.hasData = true
		if o.lineFull || len(value) > o.maxBytes-len(o.data) {
			o.eventFull = true
		}

		if !o.eventFull {
			o.data = append(o.data, value...)
		}
	} else if !o.lineFull && bytes.Equal(field, []byte("event")) && bytes.Equal(value, []byte("error")) {
		o.eventErr = true
	}
}

func (o *observer) finishEvent(at time.Time) {
	defer func() {
		o.data = o.data[:0]
		o.hasData, o.eventFull, o.eventErr = false, false, false
	}()

	if !o.hasData || o.done {
		return
	}

	if !o.eventFull && bytes.Equal(bytes.TrimSpace(o.data), []byte("[DONE]")) {
		o.done = true
		return
	}

	o.chunks++
	if o.firstData == nil {
		o.firstData = &at
	}

	if o.eventErr {
		o.failed = true
	}

	if !o.eventFull {
		o.inspectJSON(o.data)
	} else {
		o.prompt, o.completion = nil, nil
	}
}

func (o *observer) inspectJSON(body []byte) {
	var response struct {
		Usage json.RawMessage `json:"usage"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		o.prompt, o.completion = nil, nil
		return
	}

	if len(response.Error) != 0 && !bytes.Equal(bytes.TrimSpace(response.Error), []byte("null")) {
		o.failed = true
	}

	if len(response.Usage) == 0 {
		return
	}

	var counts struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
	}

	o.prompt, o.completion = nil, nil

	if err := json.Unmarshal(response.Usage, &counts); err != nil || counts.Prompt == nil ||
		counts.Completion == nil || *counts.Prompt < 0 || *counts.Completion < 0 {
		return
	}

	o.prompt, o.completion = counts.Prompt, counts.Completion
}
