// Package accounting hands immutable inference records to Redis before batched Postgres delivery.
package accounting

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/quota"
	"github.com/therealshek/llmgate/internal/store"
)

const slotBytes = 8192

var emptyFinal = strings.Repeat(" ", slotBytes)

//go:embed admit.lua
var admitSource string

//go:embed finalize.lua
var finalizeSource string

//go:embed ack.lua
var ackSource string

// Sink commits a batch atomically; an empty batch checks Postgres availability.
type Sink interface {
	RecordBatch(context.Context, []store.AccountingRecord) error
	PruneAccounting(context.Context, time.Time, int) (int64, error)
}

// Options bounds shared slots, flush work, and dependency operations.
type Options struct {
	Namespace     string
	MaxPending    int
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	LeaseTimeout  time.Duration
	MaxAOFBytes   int64
	Retention     time.Duration
	PruneInterval time.Duration
}

// Queue shares Redis records across replicas; Run's caller owns worker cancellation and joining.
type Queue struct {
	client       *redis.Client
	sink         Sink
	options      Options
	keys         []string
	mu           sync.Mutex
	owner        string
	active       map[string]activeRequest
	storageReady atomic.Bool
	aofBytes     atomic.Int64
	maxMemory    int64
}

// Open verifies persistence and makes a bounded initial database delivery attempt.
func Open(ctx context.Context, client *redis.Client, sink Sink, options Options) (*Queue, error) {
	if client == nil || sink == nil || options.Namespace == "" || options.MaxPending <= 0 ||
		options.BatchSize <= 0 || options.BatchSize > options.MaxPending ||
		options.FlushInterval <= 0 || options.Timeout < time.Millisecond ||
		options.Timeout > time.Duration(1<<63-1)/8 || options.FlushInterval > time.Duration(1<<63-1)/8 ||
		options.LeaseTimeout <= 3*(options.Timeout+options.FlushInterval) ||
		options.MaxAOFBytes <= 0 || int64(options.MaxPending) >= options.MaxAOFBytes/(4*slotBytes) ||
		options.Retention <= 0 || options.PruneInterval <= 0 {
		return nil, ErrInvalidConfig
	}

	q := &Queue{client: client, sink: sink, options: options, active: make(map[string]activeRequest)}
	for _, suffix := range []string{":records", ":ready", ":writer", ":healthy", ":owners", ":ownership"} {
		q.keys = append(q.keys, options.Namespace+suffix)
	}

	checkCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()

	for name, want := range map[string]string{"appendonly": "yes", "appendfsync": "always", "maxmemory-policy": "noeviction"} {
		settings, err := client.ConfigGet(checkCtx, name).Result()
		if err != nil || settings[name] != want {
			return nil, ErrInvalidConfig
		}
	}

	memory, err := client.ConfigGet(checkCtx, "maxmemory").Result()
	if err != nil {
		return nil, ErrInvalidConfig
	}

	q.maxMemory, err = strconv.ParseInt(memory["maxmemory"], 10, 64)
	if err != nil || q.maxMemory <= 0 || q.maxMemory >= options.MaxAOFBytes-int64(options.MaxPending)*4*slotBytes {
		return nil, ErrInvalidConfig
	}

	if err := q.checkStorage(checkCtx); err != nil {
		return nil, err
	}

	if err := q.startOwnership(checkCtx); err != nil {
		return nil, err
	}

	if _, err := q.Flush(checkCtx); err != nil {
		return nil, err
	}

	return q, nil
}

// RecordAdmission reserves both fixed-size records and waits for the local Redis AOF flush.
// It never waits for a Postgres batch timer or holds a database connection.
func (q *Queue) RecordAdmission(ctx context.Context, record store.Admission, cancel context.CancelFunc) error {
	if !q.StorageReady() {
		return ErrStorageFull
	}

	if !record.Valid() {
		return store.ErrInvalidAdmission
	}

	payload, err := padded(record)
	if err != nil {
		return err
	}

	owner := q.currentOwner()
	if owner == "" || cancel == nil {
		return ErrOwnershipLost
	}

	err = q.write(ctx, admitSource, q.ownerKeys(owner), record.RequestID, payload, emptyFinal, q.options.MaxPending, owner)
	if err == nil {
		err = q.register(record.RequestID, owner, cancel)
	}

	if err != nil {
		cancel()
	}

	return err
}

// RecordUsage fills the reserved outcome slot independently of the admission health gate.
func (q *Queue) RecordUsage(ctx context.Context, record store.Usage) error {
	if !record.Valid() {
		return store.ErrInvalidUsage
	}

	owner := q.currentOwner()
	defer q.forget(record.RequestID, owner)

	if owner == "" {
		return ErrOwnershipLost
	}

	admission, err := q.client.HGet(ctx, q.keys[0], "a:"+record.RequestID).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrOwnershipLost
		}

		return ErrUnavailable
	}

	var original store.Admission
	if json.Unmarshal([]byte(admission), &original) != nil || record.RequestID != original.RequestID || record.KeyID != original.KeyID ||
		record.Backend != original.Backend || record.Streaming != original.Streaming ||
		!record.StartedAt.Equal(original.StartedAt) || (record.Model == nil) != (original.Model == nil) ||
		(record.Model != nil && *record.Model != *original.Model) {
		return ErrConflict
	}

	payload, err := padded(record)
	if err != nil {
		return err
	}

	return q.write(ctx, finalizeSource, q.ownerKeys(owner), record.RequestID, admission, payload, emptyFinal, owner)
}

func padded(record any) (string, error) {
	payload, err := json.Marshal(record)
	if err != nil || len(payload) > slotBytes {
		return "", ErrUnavailable
	}

	return string(payload) + strings.Repeat(" ", slotBytes-len(payload)), nil
}

func (q *Queue) write(ctx context.Context, source string, keys []string, args ...any) error {
	writeCtx, cancel := context.WithTimeout(ctx, q.options.Timeout)
	defer cancel()

	conn := q.client.Conn()
	defer conn.Close()

	result, err := conn.Eval(writeCtx, source, keys, args...).Int64()
	if err != nil {
		return ErrUnavailable
	}

	switch result {
	case -2:
		return ErrConflict
	case -3:
		return ErrFull
	case -4:
		return ErrOwnershipLost
	case 1:
	default:
		return ErrUnavailable
	}
	// The same reserved connection is essential: WAITAOF covers its preceding writes.
	flushed, err := conn.WaitAOF(writeCtx, 1, 0, q.options.Timeout).Result()
	if err != nil || len(flushed) != 2 || flushed[0] != 1 {
		return ErrUnavailable
	}

	return nil
}

// Run flushes bounded batches until canceled; acknowledged queue writes need no shutdown drain.
func (q *Queue) Run(ctx context.Context) {
	ownershipCtx, stopOwnership := context.WithCancel(ctx)
	ownershipDone := make(chan struct{})
	retentionDone := make(chan struct{})

	go func() {
		defer close(ownershipDone)

		q.maintainOwnership(ownershipCtx)
	}()
	go func() {
		defer close(retentionDone)

		q.maintainRetention(ownershipCtx)
	}()

	defer func() {
		stopOwnership()
		<-ownershipDone
		<-retentionDone
	}()

	ticker := time.NewTicker(q.options.FlushInterval)
	defer ticker.Stop()

	failed := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for range 8 {
				count, err := q.Flush(ctx)
				if err != nil {
					if !failed && ctx.Err() == nil {
						slog.ErrorContext(ctx, "accounting batch delivery failed; new inference paused")
					}

					failed = true
				} else if failed && q.client.Exists(ctx, q.keys[3]).Val() == 1 {
					slog.InfoContext(ctx, "accounting batch delivery recovered")

					failed = false
				}
				// A full batch may have more work behind it. Drain without another timer wait.
				if err != nil || count < q.options.BatchSize {
					break
				}
			}
		}
	}
}

// Flush returns the number of records delivered by one leased writer.
// Lost acknowledgments cause safe re-delivery.
func (q *Queue) Flush(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, q.options.Timeout)
	defer cancel()

	if err := q.checkStorage(ctx); err != nil {
		return 0, err
	}

	lease := rand.Text()
	lifetime := 2*q.options.Timeout + 2*q.options.FlushInterval
	// Fresh shared health needs no per-poll Postgres ping or writer-lease writes.
	// Local time only schedules recovery; expired.lua uses Redis time for the fence.
	preflight := q.client.Pipeline()
	health := preflight.PTTL(ctx, q.keys[3])
	pending := preflight.ZCount(ctx, q.keys[1], "-inf", "9007199254740991")

	expired := preflight.ZCount(ctx, q.keys[4], "-inf", strconv.FormatInt(time.Now().UnixMilli(), 10))
	if _, err := preflight.Exec(ctx); err != nil {
		return 0, ErrUnavailable
	}

	if health.Val() > lifetime/2 && pending.Val() == 0 && expired.Val() == 0 {
		return 0, nil
	}

	locked, err := q.client.SetNX(ctx, q.keys[2], lease, lifetime).Result()
	if err != nil {
		return 0, ErrUnavailable
	}

	if !locked {
		return 0, nil
	}

	ack := []any{lease, "failed", lifetime.Milliseconds()}

	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), q.options.Timeout)
		defer stop()

		if err := q.client.Eval(cleanup, ackSource, q.keys, ack...).Err(); err != nil {
			slog.ErrorContext(cleanup, "accounting batch acknowledgment failed")
		}
	}()

	if err := q.recoverExpired(ctx); err != nil {
		return 0, err
	}

	ids, err := q.client.ZRangeByScore(ctx, q.keys[1], &redis.ZRangeBy{
		Min: "-inf", Max: "9007199254740991", Count: int64(q.options.BatchSize),
	}).Result()
	if err != nil {
		return 0, ErrUnavailable
	}

	pipe := q.client.Pipeline()

	commands := make([]*redis.SliceCmd, len(ids))
	for i, id := range ids {
		commands[i] = pipe.HMGet(ctx, q.keys[0], "a:"+id, "f:"+id)
	}

	if len(commands) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return 0, ErrUnavailable
		}
	}

	var records []store.AccountingRecord

	for i, id := range ids {
		values := commands[i].Val()
		if len(values) != 2 || values[0] == nil || values[1] == nil {
			return 0, ErrUnavailable
		}

		admission, ok := values[0].(string)

		final, finalOK := values[1].(string)
		if !ok || !finalOK {
			return 0, ErrUnavailable
		}

		var record store.AccountingRecord
		if json.Unmarshal([]byte(admission), &record.Admission) != nil {
			return 0, ErrUnavailable
		}

		if strings.TrimSpace(final) != "" {
			record.Usage = &store.Usage{}
			if json.Unmarshal([]byte(final), record.Usage) != nil {
				return 0, ErrUnavailable
			}
		}

		records = append(records, record)
		ack = append(ack, id, admission, final)
	}

	if err := q.sink.RecordBatch(ctx, records); err != nil {
		return 0, fmt.Errorf("persist accounting batch: %w", ErrUnavailable)
	}

	for _, record := range records {
		a := record.Admission
		if record.Usage == nil || a.ReservationID == "" {
			continue
		}

		reservation := quota.Reservation{
			ID: a.ReservationID, KeyID: a.KeyID, Day: a.ReservationDay,
			PromptTokens: a.PromptTokens, OutputTokens: a.OutputTokens,
		}
		if err := quota.NewRedisStore(q.client).Settle(ctx, reservation, quota.Charge(reservation, *record.Usage)); err != nil {
			return 0, ErrUnavailable
		}
	}

	ack[1] = "committed"

	return len(records), nil
}

func decodeAdmission(payload string, record *store.Admission) error {
	if json.Unmarshal([]byte(payload), record) != nil || !record.Valid() {
		return store.ErrInvalidAdmission
	}

	return nil
}
