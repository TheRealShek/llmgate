// Package admin serves operator-only key management on the private listener.
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/therealshek/llmgate/internal/api"
	"github.com/therealshek/llmgate/internal/requestlog"
	"github.com/therealshek/llmgate/internal/store"
)

// KeyCreator persists new keys and returns their secrets only after insertion.
type KeyCreator interface {
	CreateKey(context.Context, store.CreateKeyParams) (store.Key, string, error)
}

// KeyRevoker persists revocation and cancels the key's local active requests.
type KeyRevoker interface {
	RevokeKey(context.Context, int64) error
}

// HandlerOptions supplies private-route credentials, storage, and resource limits.
type HandlerOptions struct {
	Keys          KeyCreator
	Revoker       KeyRevoker
	Token         string
	MaxBodyBytes  int64
	CreateTimeout time.Duration
	RevokeTimeout time.Duration
}

// NewHandler registers key creation and revocation for the private listener only.
// Startup validates configuration; missing dependencies fail closed here as well.
func NewHandler(options HandlerOptions) http.Handler {
	handler := &keyHandler{
		keys:          options.Keys,
		revoker:       options.Revoker,
		tokenHash:     sha256.Sum256([]byte(options.Token)),
		configured:    options.Token != "",
		maxBodyBytes:  options.MaxBodyBytes,
		createTimeout: options.CreateTimeout,
		revokeTimeout: options.RevokeTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/keys", handler.createKey)
	mux.HandleFunc("DELETE /v1/admin/keys/{id}", handler.revokeKey)

	return mux
}

type keyHandler struct {
	keys          KeyCreator
	revoker       KeyRevoker
	tokenHash     [sha256.Size]byte
	configured    bool
	maxBodyBytes  int64
	createTimeout time.Duration
	revokeTimeout time.Duration
}

func (h *keyHandler) authorize(w http.ResponseWriter, r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		writeUnauthorized(w)
		return false
	}

	parts := strings.Split(values[0], " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") ||
		parts[1] == "" || strings.ContainsAny(parts[1], "\t\r\n,") {
		writeUnauthorized(w)
		return false
	}

	tokenHash := sha256.Sum256([]byte(parts[1]))
	if subtle.ConstantTimeCompare(tokenHash[:], h.tokenHash[:]) != 1 {
		writeUnauthorized(w)
		return false
	}

	return true
}

func (h *keyHandler) createKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if !h.configured || h.keys == nil || h.maxBodyBytes <= 0 || h.createTimeout <= 0 {
		writeError(w, http.StatusServiceUnavailable, "admin key creation unavailable", "admin_unavailable")
		return
	}

	if !h.authorize(w, r) {
		return
	}

	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType,
			"Content-Type must be application/json", "unsupported_media_type")

		return
	}

	limited := http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer limited.Close()

	body, err := io.ReadAll(limited)

	if r.Context().Err() != nil {
		return
	}

	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the size limit", "body_too_large")
		} else {
			writeError(w, http.StatusBadRequest, "could not read request body", "invalid_body")
		}

		return
	}

	var input struct {
		Name           string `json:"name"`
		RequestsPerMin int64  `json:"requests_per_min"`
		TokensPerDay   int64  `json:"tokens_per_day"`
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key creation JSON", "invalid_json")
		return
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value", "invalid_json")
		return
	}

	params := store.CreateKeyParams{
		Name:           strings.TrimSpace(input.Name),
		RequestsPerMin: input.RequestsPerMin,
		TokensPerDay:   input.TokensPerDay,
	}
	if params.Name == "" || len(params.Name) > 128 ||
		params.RequestsPerMin <= 0 || params.TokensPerDay <= 0 {
		writeError(w, http.StatusBadRequest, "invalid key name or limits", "invalid_key_options")
		return
	}

	// The private route supplies client cancellation. Only the storage operation
	// gains a deadline; this handler owns and releases it before writing the result.
	createCtx, cancel := context.WithTimeout(r.Context(), h.createTimeout)
	defer cancel()

	key, secret, err := h.keys.CreateKey(createCtx, params)
	createErr := createCtx.Err()

	cancel()

	if r.Context().Err() != nil {
		return
	}

	if err != nil || createErr != nil {
		if createErr == nil && errors.Is(err, store.ErrInvalidKeyOptions) {
			writeError(w, http.StatusBadRequest, "invalid key name or limits", "invalid_key_options")
			return
		}

		requestlog.Logger(r.Context()).WarnContext(r.Context(), "admin key creation failed")
		writeError(w, http.StatusServiceUnavailable, "admin key creation unavailable", "admin_unavailable")

		return
	}

	requestlog.Logger(r.Context()).InfoContext(r.Context(), "admin key created", "key_id", key.ID)

	result := struct {
		ID             int64     `json:"id"`
		Name           string    `json:"name"`
		APIKey         string    `json:"api_key"`
		RequestsPerMin int64     `json:"requests_per_min"`
		TokensPerDay   int64     `json:"tokens_per_day"`
		CreatedAt      time.Time `json:"created_at"`
	}{
		ID:             key.ID,
		Name:           key.Name,
		APIKey:         secret,
		RequestsPerMin: key.RequestsPerMin,
		TokensPerDay:   key.TokensPerDay,
		CreatedAt:      key.CreatedAt,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(result); err != nil {
		// The insertion already committed; log a fixed message and never retry it.
		requestlog.Logger(r.Context()).WarnContext(r.Context(),
			"created admin key response could not be delivered")
	}
}

func (h *keyHandler) revokeKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if !h.configured || h.revoker == nil || h.revokeTimeout <= 0 {
		writeError(w, http.StatusServiceUnavailable, "admin key revocation unavailable", "admin_unavailable")
		return
	}

	if !h.authorize(w, r) {
		return
	}

	rawID := r.PathValue("id")

	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rawID {
		writeError(w, http.StatusBadRequest, "key ID must be a positive integer", "invalid_key_id")
		return
	}

	// The private DELETE request supplies cancellation. The revoker owns the
	// database update and then signals local inference requests before returning.
	revokeCtx, cancel := context.WithTimeout(r.Context(), h.revokeTimeout)
	defer cancel()

	err = h.revoker.RevokeKey(revokeCtx, id)
	revokeErr := revokeCtx.Err()

	cancel()

	if r.Context().Err() != nil {
		return
	}

	if err != nil || revokeErr != nil {
		if revokeErr == nil && errors.Is(err, store.ErrKeyNotFound) {
			writeError(w, http.StatusNotFound, "API key not found", "key_not_found")
			return
		}

		requestlog.Logger(r.Context()).WarnContext(r.Context(), "admin key revocation failed", "key_id", id)
		writeError(w, http.StatusServiceUnavailable, "admin key revocation unavailable", "admin_unavailable")

		return
	}

	requestlog.Logger(r.Context()).InfoContext(r.Context(), "admin key revoked", "key_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	_ = api.WriteError(w, http.StatusUnauthorized, api.Error{
		Message: "invalid admin credential", Type: "authentication_error", Code: "invalid_admin_token",
	})
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	errorType := "invalid_request_error"
	if status >= 500 {
		errorType = "server_error"
	}

	_ = api.WriteError(w, status, api.Error{
		Message: message, Type: errorType, Code: code,
	})
}
