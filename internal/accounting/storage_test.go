package accounting

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/therealshek/llmgate/internal/store"
)

// TestStorageBudget checks exhaustion stops protected traffic before Redis credits and retains final-write headroom.
func TestStorageBudget(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	var canceled atomic.Bool

	a, u := queueRecord("storage-budget")
	if err := q.RecordAdmission(t.Context(), a, func() { canceled.Store(true) }); err != nil {
		t.Fatal(err)
	}

	if err := q.checkStorage(t.Context()); err != nil {
		t.Fatal(err)
	}

	q.options.MaxAOFBytes = q.maxMemory + int64(q.options.MaxPending)*4*slotBytes + q.aofBytes.Load()
	if err := q.checkStorage(t.Context()); !errors.Is(err, ErrStorageFull) || !canceled.Load() {
		t.Fatal("storage boundary did not shed work", err)
	}

	calls := 0
	handler := q.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(200) }))

	for _, path := range []string{"/v1/chat/completions", "/v1/models", "/v1/healthz"} {
		method := http.MethodGet
		if path == "/v1/chat/completions" {
			method = http.MethodPost
		}

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))

		want := 503
		if path == "/v1/healthz" {
			want = 200
		}

		if w.Code != want {
			t.Fatal(path, w.Code)
		}
	}

	if calls != 1 {
		t.Fatal("protected work crossed the storage guard", calls)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal("storage guard consumed final-result headroom", err)
	}

	q.options.MaxAOFBytes = 256 << 20
	if _, err := q.Flush(t.Context()); err != nil || !q.StorageReady() {
		t.Fatal("storage recovery did not resume delivery", err)
	}
}
