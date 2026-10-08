package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
)

type chatHandler struct {
	upstream     http.Handler
	maxBodyBytes int64
}

func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeChatError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "unsupported_media_type")
		return
	}

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

	stream, err := chatStreamRequested(body)
	if err != nil {
		writeChatError(w, http.StatusBadRequest, "request must be a JSON object with a boolean stream field when present", "invalid_json")
		return
	}
	if stream {
		_ = api.WriteError(w, http.StatusNotImplemented, api.Error{
			Message: "streaming chat is not implemented yet",
			Type:    "server_error",
			Code:    "streaming_not_supported",
		})
		return
	}

	request := r.Clone(r.Context())
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.TransferEncoding = nil
	h.upstream.ServeHTTP(w, request)
}

func chatStreamRequested(body []byte) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false, err
	}
	if fields == nil {
		return false, errors.New("request is not a JSON object")
	}
	var stream bool
	if raw, exists := fields["stream"]; exists {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false, errors.New("stream is not a boolean")
		}
		if err := json.Unmarshal(raw, &stream); err != nil {
			return false, err
		}
	}
	return stream, nil
}

func writeChatError(w http.ResponseWriter, status int, message, code string) {
	_ = api.WriteError(w, status, api.Error{
		Message: message,
		Type:    "invalid_request_error",
		Code:    code,
	})
}
