package api

import (
	"encoding/json"
	"net/http"
)

// Error describes an OpenAI-compatible API error.
type Error struct {
	// Message describes the failure to the caller.
	Message string `json:"message"`
	// Type identifies the category of error.
	Type string `json:"type"`
	// Code identifies the specific failure.
	Code string `json:"code"`
}

type errorResponse struct {
	Error Error `json:"error"`
}

// WriteError writes the status and JSON error envelope.
// A returned write error cannot be replaced with another HTTP status.
func WriteError(w http.ResponseWriter, status int, detail Error) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(errorResponse{Error: detail})
}
