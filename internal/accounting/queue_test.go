package accounting

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/ratelimit"
	"github.com/therealshek/llmgate/internal/store"
)

type sinkFunc func(context.Context, []store.AccountingRecord) error

func (f sinkFunc) RecordBatch(ctx context.Context, records []store.AccountingRecord) error {
	return f(ctx, records)
}

func queueClient(t *testing.T) *redis.Client {
	t.Helper()

	address := os.Getenv("LLMGATE_TEST_ACCOUNTING_REDIS_URL")
	if address == "" {
		t.Skip("set LLMGATE_TEST_ACCOUNTING_REDIS_URL to an isolated AOF-always Redis")
	}

	client, err := ratelimit.OpenClient(t.Context(), address, ratelimit.ClientLimits{
		MaxConns: 8, ConnectTimeout: time.Second, CommandTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

func queueOptions() Options {
	return Options{
		Namespace: "llmgate:test:accounting:" + rand.Text(), MaxPending: 4, BatchSize: 4,
		FlushInterval: 10 * time.Millisecond, Timeout: time.Second, LeaseTimeout: 6 * time.Second, MaxAOFBytes: 256 << 20, Retention: 30 * 24 * time.Hour, PruneInterval: time.Hour,
	}
}

func openQueue(t *testing.T, sink Sink) *Queue {
	t.Helper()

	q, err := Open(t.Context(), queueClient(t), sink, queueOptions())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if err := q.client.Del(ctx, q.keys...).Err(); err != nil {
			t.Error(err)
		}
	})

	return q
}

func queueRecord(id string) (store.Admission, store.Usage) {
	started := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	a := store.Admission{
		RequestID: id, KeyID: 1, Backend: "primary", StartedAt: started,
		ReservationID: id, ReservationDay: 20751, PromptTokens: 7, OutputTokens: 20,
	}
	prompt, completion := int64(7), int64(2)
	u := store.Usage{
		RequestID: id, KeyID: 1, Backend: "primary", StartedAt: started,
		PromptTokens: &prompt, CompletionTokens: &completion, Duration: time.Second,
		HTTPStatus: 200, Status: store.UsageCompleted,
	}

	return a, u
}

// TestQueueCapacity checks concurrent admission cannot consume reserved final-result capacity.
func TestQueueCapacity(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	var (
		accepted atomic.Int64
		workers  sync.WaitGroup
	)
	for i := range 16 {
		workers.Go(func() {
			a, _ := queueRecord(strconv.Itoa(i))

			err := q.RecordAdmission(t.Context(), a, func() {})
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, ErrFull) {
				t.Error(err)
			}
		})
	}

	workers.Wait()

	if accepted.Load() != 4 {
		t.Fatal("concurrent admissions escaped capacity", accepted.Load())
	}

	ids, err := q.client.ZRange(t.Context(), q.keys[1], 0, -1).Result()
	if err != nil || len(ids) != 4 {
		t.Fatal(ids, err)
	}

	for _, id := range ids {
		a, u := queueRecord(id)
		if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
			t.Fatal("identical admission retry consumed another slot", err)
		}

		if err := q.RecordUsage(t.Context(), u); err != nil {
			t.Fatal("full admission queue prevented final handoff", err)
		}
	}

	if _, err := q.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if count := q.client.HLen(t.Context(), q.keys[0]).Val(); count != 0 {
		t.Fatal("delivered records retained capacity", count)
	}
}

// TestQueueDeliveryFailure checks Postgres failure stops new work while admitted outcomes remain replayable.
func TestQueueDeliveryFailure(t *testing.T) {
	failed := false
	delivered := 0
	sink := sinkFunc(func(_ context.Context, records []store.AccountingRecord) error {
		if failed {
			return errors.New("private-postgres-diagnostic")
		}

		delivered += len(records)

		return nil
	})
	q := openQueue(t, sink)

	a, u := queueRecord("outage")
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	failed = true

	if _, err := q.Flush(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("database failure hidden", err)
	}

	other, _ := queueRecord("rejected")
	if err := q.RecordAdmission(t.Context(), other, func() {}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("database failure did not stop admission", err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal("outage blocked active request's final slot", err)
	}

	failed = false

	replacement, err := Open(t.Context(), q.client, sink, q.options)
	if err != nil || delivered != 1 {
		t.Fatal("replacement worker lost queued final outcome", delivered, err)
	}

	if err := replacement.RecordAdmission(t.Context(), other, func() {}); err != nil {
		t.Fatal("recovery did not reopen admission", err)
	}
}

// TestQueueFinalizationRace checks admission acknowledgment cannot erase a concurrently arriving final result.
func TestQueueFinalizationRace(t *testing.T) {
	var q *Queue

	a, u := queueRecord("race")
	finals := 0
	sink := sinkFunc(func(ctx context.Context, records []store.AccountingRecord) error {
		for _, r := range records {
			if r.Usage == nil {
				if err := q.RecordUsage(ctx, u); err != nil {
					t.Error(err)
				}
			} else {
				finals++
			}
		}

		return nil
	})

	q = openQueue(t, sink)
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if q.client.ZCard(t.Context(), q.keys[1]).Val() != 1 {
		t.Fatal("admission acknowledgment erased final work")
	}

	if _, err := q.Flush(t.Context()); err != nil || finals != 1 {
		t.Fatal("concurrent final result was not delivered", finals, err)
	}
}

// TestQueueConflicts checks immutable identities and outcomes cannot be replaced by later delivery.
func TestQueueConflicts(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	a, u := queueRecord("immutable")
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	changed := a

	changed.OutputTokens++
	if err := q.RecordAdmission(t.Context(), changed, func() {}); !errors.Is(err, ErrConflict) {
		t.Fatal("admission replaced", err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal("identical final retry rejected", err)
	}

	u.Duration += time.Second
	if err := q.RecordUsage(t.Context(), u); !errors.Is(err, ErrConflict) {
		t.Fatal("final outcome replaced", err)
	}
}

// TestQueueWriterLease checks two replicas do not flush the same batch concurrently.
func TestQueueWriterLease(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	sink := sinkFunc(func(ctx context.Context, records []store.AccountingRecord) error {
		if len(records) == 0 {
			return nil
		}

		close(entered)

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	q := openQueue(t, sink)

	other, err := Open(t.Context(), q.client, sink, q.options)
	if err != nil {
		t.Fatal(err)
	}

	a, u := queueRecord("leased")
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := q.Flush(t.Context())
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start")
	}

	count, flushErr := other.Flush(t.Context())

	close(release)

	if err := <-done; err != nil || flushErr != nil || count != 0 {
		t.Fatal("second replica entered an owned batch", count, err, flushErr)
	}
}

// TestQueueWorkerCancellation checks background delivery stops and joins without dropping durable work.
func TestQueueWorkerCancellation(t *testing.T) {
	delivered := make(chan struct{}, 1)
	q := openQueue(t, sinkFunc(func(_ context.Context, records []store.AccountingRecord) error {
		if len(records) > 0 {
			delivered <- struct{}{}
		}

		return nil
	}))

	a, u := queueRecord("worker")
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)

		q.Run(ctx)
	}()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Error("background worker did not deliver")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

// TestQueueRedisConfiguration checks unsafe persistence and final-slot memory pressure on disposable Redis only.
func TestQueueRedisConfiguration(t *testing.T) {
	if os.Getenv("LLMGATE_ACCOUNTING_ADMIN_CHECK") != "1" {
		t.Skip("run alone with LLMGATE_ACCOUNTING_ADMIN_CHECK=1 on disposable Redis")
	}

	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))
	if err := q.client.ConfigSet(t.Context(), "appendfsync", "everysec").Err(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(t.Context(), q.client, q.sink, q.options)

	restoreErr := q.client.ConfigSet(t.Context(), "appendfsync", "always").Err()
	if !errors.Is(err, ErrInvalidConfig) || restoreErr != nil {
		t.Fatal("unsafe persistence accepted", err, restoreErr)
	}

	a, u := queueRecord("memory-pressure")
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	settings, err := q.client.ConfigGet(t.Context(), "maxmemory").Result()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = q.client.ConfigSet(context.Background(), "maxmemory", settings["maxmemory"]).Err() })

	if err := q.client.ConfigSet(t.Context(), "maxmemory", "1").Err(); err != nil {
		t.Fatal(err)
	}

	finalErr := q.RecordUsage(t.Context(), u)
	if err := q.client.ConfigSet(t.Context(), "maxmemory", settings["maxmemory"]).Err(); err != nil {
		t.Fatal(err)
	}

	if finalErr != nil {
		t.Fatal("reserved final slot failed under memory pressure", finalErr)
	}
}

// TestQueueRedisRestart supports a two-step external SIGKILL check against disposable Redis only.
func TestQueueRedisRestart(t *testing.T) {
	phase := os.Getenv("LLMGATE_ACCOUNTING_RESTART_PHASE")
	if phase == "" {
		t.Skip("set seed/replay around restarting isolated Redis to verify AOF recovery")
	}

	options := queueOptions()
	options.Namespace = "llmgate:test:accounting:restart-proof"
	recovered := 0

	q, err := Open(t.Context(), queueClient(t), sinkFunc(func(_ context.Context, records []store.AccountingRecord) error {
		for _, r := range records {
			if r.Usage != nil && r.Usage.RequestID == "restart" && *r.Usage.CompletionTokens == 2 {
				recovered++
			}
		}

		return nil
	}), options)
	if err != nil {
		t.Fatal(err)
	}

	if phase == "seed" {
		a, u := queueRecord("restart")
		if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
			t.Fatal(err)
		}

		if err := q.RecordUsage(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	} else if phase == "replay" {
		if recovered != 1 || q.client.HLen(t.Context(), q.keys[0]).Val() != 0 {
			t.Fatal("acknowledged final result did not survive Redis SIGKILL", recovered)
		}

		_ = q.client.Del(t.Context(), q.keys...).Err()
	} else {
		t.Fatal("restart phase must be seed or replay")
	}
}

func (f sinkFunc) PruneAccounting(context.Context, time.Time, int) (int64, error) { return 0, nil }

// TestQuotaSettlementReplay checks database delivery cannot discard a retryable refund and never refunds twice.
func TestQuotaSettlementReplay(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	r, err := quota.NewRedisStore(q.client).Reserve(t.Context(), quota.Budget{KeyID: 17, Limit: 1000, HardLimit: 1000, PromptTokens: 7, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}

	key := "llmgate:tokens:v1:17"

	t.Cleanup(func() { _ = q.client.Del(context.Background(), key).Err() })

	a, u := queueRecord("settlement-retry")
	a.KeyID, u.KeyID = 17, 17

	a.ReservationID, a.ReservationDay = r.ID, r.Day
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	if err := q.client.HSet(t.Context(), key, "used", "invalid").Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); err == nil {
		t.Fatal("failed settlement acknowledged")
	}

	if q.client.HLen(t.Context(), q.keys[0]).Val() != 2 {
		t.Fatal("retryable settlement discarded")
	}

	if err := q.client.HSet(t.Context(), key, "used", 27).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if used, err := q.client.HGet(t.Context(), key, "used").Int64(); err != nil || used != 9 {
		t.Fatal("refund missing or applied twice", used, err)
	}

	if q.client.HExists(t.Context(), key, "r:"+r.ID).Val() {
		t.Fatal("settled marker retained")
	}
}

// TestRecoveredQuotaAcrossMidnight checks old-day recovery cannot refund a new day's budget.
func TestRecoveredQuotaAcrossMidnight(t *testing.T) {
	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error { return nil }))

	r, err := quota.NewRedisStore(q.client).Reserve(t.Context(), quota.Budget{KeyID: 18, Limit: 1000, HardLimit: 1000, PromptTokens: 7, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}

	key := "llmgate:tokens:v1:" + strconv.FormatInt(r.KeyID, 10)

	t.Cleanup(func() { _ = q.client.Del(context.Background(), key).Err() })

	a, u := queueRecord("midnight-replay")
	a.KeyID, u.KeyID = r.KeyID, r.KeyID

	a.ReservationID, a.ReservationDay = r.ID, r.Day
	if err := q.RecordAdmission(t.Context(), a, func() {}); err != nil {
		t.Fatal(err)
	}

	if err := q.RecordUsage(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	if err := q.client.HSet(t.Context(), key, "day", r.Day+1, "used", 40).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	if used, err := q.client.HGet(t.Context(), key, "used").Int64(); err != nil || used != 40 {
		t.Fatal("replay changed the new UTC day", used, err)
	}
}

// TestDiskFullRecovery supports a disposable small-filesystem ENOSPC check without filling the host disk.
func TestDiskFullRecovery(t *testing.T) {
	phase := os.Getenv("LLMGATE_ACCOUNTING_DISK_PHASE")
	if phase == "" {
		t.Skip("run seed/replay on disposable Redis with a small filesystem")
	}

	client := queueClient(t)
	options := queueOptions()
	options.Namespace = "llmgate:test:disk-full-proof"
	options.MaxPending, options.BatchSize = 512, 64
	recovered := 0
	sink := sinkFunc(func(_ context.Context, records []store.AccountingRecord) error {
		for _, record := range records {
			if record.Usage != nil {
				recovered++
			}
		}

		return nil
	})

	q, err := Open(t.Context(), client, sink, options)
	if err != nil {
		t.Fatal(err)
	}

	if phase == "seed" {
		ctx, cancel := context.WithCancel(t.Context())

		done := make(chan struct{})
		go func() { defer close(done); q.Run(ctx) }()

		accepted := 0

		for i := range options.MaxPending {
			a, _ := queueRecord("disk-" + strconv.Itoa(i))
			if err = q.RecordAdmission(t.Context(), a, func() {}); err != nil {
				break
			}

			accepted++
		}

		cancel()
		<-done

		if err == nil || accepted == 0 || accepted == options.MaxPending || errors.Is(err, ErrFull) {
			t.Fatal("filesystem did not reject admission before queue capacity", accepted, err)
		}

		_ = q.checkStorage(t.Context())
		if q.StorageReady() {
			t.Fatal("full recording storage remained ready")
		}

		t.Logf("disk pressure accepted: %d", accepted)
	} else if phase == "replay" {
		want, err := strconv.Atoi(os.Getenv("LLMGATE_ACCOUNTING_DISK_ACCEPTED"))
		if err != nil || want <= 0 {
			t.Fatal("missing accepted-request count")
		}

		for i := 0; i < 16 && recovered < want; i++ {
			if _, err := q.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}

			time.Sleep(10 * time.Millisecond)
		}

		if recovered < want {
			t.Fatal("disk recovery lost acknowledged attempts", recovered, want)
		}
	} else {
		t.Fatal("disk phase must be seed/replay")
	}
}

// TestIdleDatabaseHealth checks fresh shared health avoids repeated database probes while stale health still fails closed.
func TestIdleDatabaseHealth(t *testing.T) {
	calls := 0
	failed := false

	q := openQueue(t, sinkFunc(func(context.Context, []store.AccountingRecord) error {
		calls++

		if failed {
			return errors.New("database unavailable")
		}

		return nil
	}))
	for range 8 {
		if _, err := q.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if calls != 1 {
		t.Fatal("idle polling repeated Postgres probes", calls)
	}

	failed = true

	if err := q.client.PExpire(t.Context(), q.keys[3], 50*time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Flush(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("stale database health remained usable", err)
	}

	if q.client.Exists(t.Context(), q.keys[3]).Val() != 0 {
		t.Fatal("failed probe retained shared health")
	}
}
