package gateway

import "net/http"

// NewHandler builds the gateway routes without opening a listener so HTTP behavior
// can be tested independently of server startup and shutdown.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	// Liveness only checks this process; an unavailable backend belongs to readiness.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// Set headers before committing the status; later header changes are too late.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// A failed write means the response could not be delivered; there is no further work.
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
