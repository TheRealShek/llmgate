package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/therealshek/llmgate/internal/admin"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/store"
)

// TestAdminCreationAndAuthentication checks HTTP provisioning creates a persisted usable inference key.
func TestAdminCreationAndAuthentication(t *testing.T) {
	pool, _ := authenticationTestPool(t)
	keys := store.NewKeyStore(pool)
	token := strings.Repeat("a", 64)
	routes := admin.NewHandler(admin.HandlerOptions{
		Keys: keys, Token: token, MaxBodyBytes: 4096, CreateTimeout: time.Second,
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/admin/keys",
		strings.NewReader(`{"name":"admin-created","requests_per_min":12,"tokens_per_day":300}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	routes.ServeHTTP(w, r)

	if w.Code != 201 {
		t.Fatalf("creation status = %d", w.Code)
	}

	var created struct {
		ID     int64  `json:"id"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	key, err := keys.LookupKey(t.Context(), created.APIKey)
	if err != nil {
		t.Fatal("created key could not be looked up")
	}

	if key.ID != created.ID || key.RequestsPerMin != 12 || key.TokensPerDay != 300 {
		t.Fatal("stored identity or limits changed")
	}

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	public := gateway.NewHandler(gateway.HandlerOptions{
		NonStreaming: backend, Authentication: auth.NewAuthenticator(keys, time.Second),
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+created.APIKey)

	response := httptest.NewRecorder()
	public.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("issued key rejected by public auth: %d", response.Code)
	}
}
