package backend

import (
	"net/http"

	"github.com/therealshek/llmgate/internal/api"
)

// NewReadinessHandler serves the registry's health snapshot without upstream work.
// It neither reserves inference capacity nor gates chat traffic.
func NewReadinessHandler(registry *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, state := range registry.States() {
			if state.Healthy {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok\n"))

				return
			}
		}

		_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
			Message: "backend is not ready",
			Type:    "server_error",
			Code:    "backend_not_ready",
		})
	})
}
