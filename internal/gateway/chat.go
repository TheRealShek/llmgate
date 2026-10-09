package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/therealshek/llmgate/internal/api"
)

// chatHandler validates chat completion requests and forwards valid payloads
// to the configured streaming or non-streaming backend handler.
type chatHandler struct {
	nonStreaming http.Handler
	streaming    http.Handler
	maxBodyBytes int64
	maxTokens    int64
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The OpenAI chat completions API requires JSON payloads.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeChatError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "unsupported_media_type")
		return
	}

	// Cap the bytes read from the client to prevent unbounded memory growth.
	limited := http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer limited.Close()
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeChatError(w, http.StatusRequestEntityTooLarge, "request body exceeds the size limit", "body_too_large")
		} else {
			writeChatError(w, http.StatusBadRequest, "could not read request body", "invalid_body")
		}
		return
	}

	// Check whether the client requested streaming or a single JSON response.
	options, err := parseChatRequest(body)
	if err != nil {
		writeChatError(w, http.StatusBadRequest, "request must be a JSON object with a boolean stream field when present", "invalid_json")
		return
	}
	upstream := h.nonStreaming
	if options.stream {
		upstream = h.streaming
	}
	if upstream == nil {
		// Constructors without a streaming proxy keep their existing 501 response.
		_ = api.WriteError(w, http.StatusNotImplemented, api.Error{
			Message: "streaming chat is not implemented yet",
			Type:    "server_error",
			Code:    "streaming_not_supported",
		})
		return
	}

	// Validate the client's token count, or add the configured default when omitted,
	// before the buffered request reaches the proxy.
	body, err = limitChatTokens(body, options.fields, h.maxTokens)
	if err != nil {
		writeChatError(w, http.StatusBadRequest, "max_tokens must be a positive integer within the configured limit", "invalid_max_tokens")
		return
	}

	// Reading r.Body drained the stream. Clone the request and attach a new reader
	// for the buffered bytes so the upstream proxy can read the body.
	request := r.Clone(r.Context())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.TransferEncoding = nil // Use Content-Length instead of chunked transfer.
	// Send the cloned request to the selected response mode. Its inherited context
	// lets a client disconnect stop either the JSON response or the SSE stream.
	upstream.ServeHTTP(w, request)
}

type chatRequest struct {
	fields map[string]json.RawMessage
	stream bool
}

// parseChatRequest unmarshals only top-level keys so validation can inspect options
// without rebuilding the client's model payload or dropping backend-specific fields.
func parseChatRequest(body []byte) (chatRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return chatRequest{}, err
	}
	if fields == nil {
		return chatRequest{}, errors.New("request is not a JSON object")
	}
	var stream bool
	if raw, exists := fields["stream"]; exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return chatRequest{}, errors.New("stream is not a boolean")
		}
		if err := json.Unmarshal(raw, &stream); err != nil {
			return chatRequest{}, err
		}
	}
	return chatRequest{fields: fields, stream: stream}, nil
}

func limitChatTokens(body []byte, fields map[string]json.RawMessage, limit int64) ([]byte, error) {
	if limit == 0 {
		return body, nil
	}
	raw, exists := fields["max_tokens"]
	if !exists {
		return addDefaultChatTokens(body, len(fields), limit), nil
	}
	var tokens int64
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return nil, err
	}
	if tokens <= 0 || tokens > limit {
		return nil, errors.New("max_tokens is outside the configured limit")
	}
	return body, nil
}

// addDefaultChatTokens inserts one field into an already validated JSON object.
// Keep the client's existing bytes instead of re-encoding unknown backend fields.
func addDefaultChatTokens(body []byte, fieldCount int, limit int64) []byte {
	closingBrace := len(bytes.TrimRight(body, " \t\r\n")) - 1
	tokenField := strconv.AppendInt([]byte(`"max_tokens":`), limit, 10)
	result := make([]byte, 0, len(body)+len(tokenField)+1)
	result = append(result, body[:closingBrace]...)
	if fieldCount > 0 {
		result = append(result, ',')
	}
	result = append(result, tokenField...)
	result = append(result, body[closingBrace:]...)
	return result
}

// writeChatError writes an invalid_request_error in OpenAI JSON format.
func writeChatError(w http.ResponseWriter, status int, message, code string) {
	_ = api.WriteError(w, status, api.Error{
		Message: message,
		Type:    "invalid_request_error",
		Code:    code,
	})
}
