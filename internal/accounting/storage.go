package accounting

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/therealshek/llmgate/internal/api"
)

// StorageReady reports the last bounded background storage check without request-time I/O.
func (q *Queue) StorageReady() bool { return q.storageReady.Load() }

// Protect sheds protected inference traffic before it can grow Redis rate-limit logs.
// Probes, metrics and unrelated routes still enter their normal handlers.
func (q *Queue) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected := (r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions") ||
			(r.Method == http.MethodGet && r.URL.Path == "/v1/models")
		if protected && !q.StorageReady() {
			_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
				Message: "accounting storage unavailable", Type: "server_error", Code: "accounting_storage_unavailable",
			})

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (q *Queue) checkStorage(ctx context.Context) error {
	info, err := q.client.Info(ctx, "persistence").Result()
	if err != nil {
		q.storageReady.Store(false)
		q.cancelActive()

		return ErrUnavailable
	}

	values := make(map[string]string)

	for _, line := range strings.Split(info, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			values[key] = value
		}
	}

	size, err := strconv.ParseInt(values["aof_current_size"], 10, 64)
	q.aofBytes.Store(size)
	// Leave room for a full memory-sized rewrite and every admitted final outcome.
	reserve := q.maxMemory + int64(q.options.MaxPending)*4*slotBytes
	ready := err == nil && values["aof_enabled"] == "1" && values["aof_last_write_status"] == "ok" &&
		size >= 0 && size < q.options.MaxAOFBytes-reserve
	q.storageReady.Store(ready)

	if !ready {
		q.cancelActive()
		return ErrStorageFull
	}

	return nil
}
