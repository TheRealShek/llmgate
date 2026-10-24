package quota

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"math/big"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/ratelimit"
)

func testRedis(t *testing.T) (*redis.Client, *RedisStore, int64) {
	t.Helper()

	address := os.Getenv("LLMGATE_TEST_REDIS_URL")
	if address == "" {
		t.Skip("set LLMGATE_TEST_REDIS_URL to run quota Redis tests")
	}

	client, err := ratelimit.OpenClient(t.Context(), address, ratelimit.ClientLimits{MaxConns: 4, ConnectTimeout: time.Second, CommandTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	number, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64-1))
	if err != nil {
		t.Fatal(err)
	}

	id := number.Int64() + 1

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if err := client.Del(ctx, quotaKey(id)).Err(); err != nil {
			t.Error(err)
		}

		_ = client.Close()
	})

	return client, NewRedisStore(client), id
}

// TestReservations checks normal and grace ceilings, prompt charging, refunds, and duplicate settlement.
func TestReservations(t *testing.T) {
	for _, grace := range []int64{0, 100} {
		t.Run(strconv.FormatInt(grace, 10), func(t *testing.T) {
			client, store, id := testRedis(t)

			first, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 1000, HardLimit: 1000 + grace, PromptTokens: 100, MaxOutputTokens: 800})
			if err != nil || first.OutputTokens != 800 || first.ID == "" {
				t.Fatal(first, err)
			}

			second, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 1000, HardLimit: 1000 + grace, PromptTokens: 50, MaxOutputTokens: 500})
			if err != nil || second.OutputTokens != 50+grace || second.ID == "" {
				t.Fatal(second, err)
			}

			denied, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 1000, HardLimit: 1000 + grace, MaxOutputTokens: 1})
			if err != nil || denied.ID != "" || denied.RetryAfter <= 0 || denied.RetryAfter > 24*time.Hour {
				t.Fatal(denied, err)
			}

			if err := store.Settle(t.Context(), first, 110); err != nil {
				t.Fatal(err)
			}

			if err := store.Settle(t.Context(), first, 0); err != nil {
				t.Fatal(err)
			}

			used, err := client.HGet(t.Context(), quotaKey(id), "used").Int64()
			if err != nil || used != 210+grace {
				t.Fatal("wrong reconciled charge", used, err)
			}

			if err := store.Settle(t.Context(), second, -1); err != nil {
				t.Fatal(err)
			}

			fields, err := client.HLen(t.Context(), quotaKey(id)).Result()
			if err != nil || fields != 2 {
				t.Fatal("finished markers retained", fields, err)
			}

			ttl, err := client.TTL(t.Context(), quotaKey(id)).Result()
			if err != nil || ttl <= 0 || ttl > 24*time.Hour {
				t.Fatal("unbounded daily state", ttl, err)
			}
		})
	}
}

// TestConcurrentReservations checks independent gateways cannot spend the same allowance.
func TestConcurrentReservations(t *testing.T) {
	client, first, id := testRedis(t)

	secondClient, err := ratelimit.OpenClient(t.Context(), os.Getenv("LLMGATE_TEST_REDIS_URL"), ratelimit.ClientLimits{MaxConns: 4, ConnectTimeout: time.Second, CommandTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = secondClient.Close() })

	stores := []*RedisStore{first, NewRedisStore(secondClient)}

	var (
		workers  sync.WaitGroup
		admitted atomic.Int64
	)

	for i := range 32 {
		workers.Go(func() {
			r, err := stores[i%2].Reserve(t.Context(), Budget{KeyID: id, Limit: 100, HardLimit: 100, PromptTokens: 5, MaxOutputTokens: 5})
			if err != nil {
				t.Error(err)
				return
			}

			if r.ID != "" {
				admitted.Add(1)
			}
		})
	}

	workers.Wait()

	used, err := client.HGet(t.Context(), quotaKey(id), "used").Int64()
	if err != nil || used != 100 || admitted.Load() != 10 {
		t.Fatal("double spending", used, admitted.Load(), err)
	}
}

// TestReservationDays checks rollover, late settlements, and backwards-clock state fail safely.
func TestReservationDays(t *testing.T) {
	client, store, id := testRedis(t)

	clock, err := client.Time(t.Context()).Result()
	if err != nil {
		t.Fatal(err)
	}

	day := clock.Unix() / 86400

	old := Reservation{ID: "old", KeyID: id, Day: day - 1}
	if err := client.HSet(t.Context(), quotaKey(id), "day", day-1, "used", 1000, "r:old", 1000).Err(); err != nil {
		t.Fatal(err)
	}

	current, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 100, HardLimit: 100, PromptTokens: 10, MaxOutputTokens: 40})
	if err != nil || current.ID == "" || current.Day != day {
		t.Fatal(current, err)
	}

	if err := store.Settle(t.Context(), old, 0); err != nil {
		t.Fatal(err)
	}

	used, err := client.HGet(t.Context(), quotaKey(id), "used").Int64()
	if err != nil || used != 50 {
		t.Fatal("late settlement changed new day", used, err)
	}

	if err := client.HSet(t.Context(), quotaKey(id), "day", day+1).Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 100, HardLimit: 100, MaxOutputTokens: 1}); !errors.Is(err, ErrUnavailable) {
		t.Fatal("future state reset quota", err)
	}
}

// TestReservationFailures checks invalid limits, corrupt state, cancellation, and Redis outages fail closed.
func TestReservationFailures(t *testing.T) {
	client, store, id := testRedis(t)

	valid := Budget{KeyID: id, Limit: 10, HardLimit: 10, PromptTokens: 2, MaxOutputTokens: 3}
	for _, invalid := range []Budget{
		{}, {KeyID: id, Limit: 10, HardLimit: 9, MaxOutputTokens: 1},
		{KeyID: id, Limit: maxExact, HardLimit: maxExact + 1, MaxOutputTokens: 1},
		{KeyID: id, Limit: 10, HardLimit: 10, PromptTokens: -1, MaxOutputTokens: 1},
	} {
		if _, err := store.Reserve(t.Context(), invalid); !errors.Is(err, ErrInvalidLimit) {
			t.Fatal(err)
		}
	}

	clock, err := client.Time(t.Context()).Result()
	if err != nil {
		t.Fatal(err)
	}

	if err := client.HSet(t.Context(), quotaKey(id), "day", clock.Unix()/86400, "used", "corrupt").Err(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Reserve(t.Context(), valid); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}

	if err := client.Del(t.Context(), quotaKey(id)).Err(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := store.Reserve(ctx, valid); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}

	if _, err := (*RedisStore)(nil).Reserve(t.Context(), valid); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}

	disconnected := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 10 * time.Millisecond})

	t.Cleanup(func() { _ = disconnected.Close() })

	if _, err := NewRedisStore(disconnected).Reserve(t.Context(), valid); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// TestReservationPrecision checks exact large counters and a lowered limit cannot grant new budget.
func TestReservationPrecision(t *testing.T) {
	client, store, id := testRedis(t)

	r, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: maxExact, HardLimit: maxExact, PromptTokens: maxExact - 1, MaxOutputTokens: 2})
	if err != nil || r.OutputTokens != 1 {
		t.Fatal(r, err)
	}

	if err := store.Settle(t.Context(), r, maxExact-1); err != nil {
		t.Fatal(err)
	}

	next, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: maxExact, HardLimit: maxExact, MaxOutputTokens: 1})
	if err != nil || next.ID == "" {
		t.Fatal(next, err)
	}

	denied, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 1, HardLimit: 1, MaxOutputTokens: 1})
	if err != nil || denied.ID != "" {
		t.Fatal(denied, err)
	}

	used, err := client.HGet(t.Context(), quotaKey(id), "used").Int64()
	if err != nil || used != maxExact {
		t.Fatal("integer precision lost", used, err)
	}
}

// TestConcurrentSettlements checks duplicate finalizers refund once and leave other keys untouched.
func TestConcurrentSettlements(t *testing.T) {
	client, store, id := testRedis(t)
	_, otherStore, otherID := testRedis(t)

	first, err := store.Reserve(t.Context(), Budget{KeyID: id, Limit: 100, HardLimit: 100, PromptTokens: 10, MaxOutputTokens: 80})
	if err != nil || first.ID == "" {
		t.Fatal(first, err)
	}

	other, err := otherStore.Reserve(t.Context(), Budget{KeyID: otherID, Limit: 50, HardLimit: 50, PromptTokens: 10, MaxOutputTokens: 80})
	if err != nil || other.OutputTokens != 40 {
		t.Fatal(other, err)
	}

	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			if err := store.Settle(t.Context(), first, 15); err != nil {
				t.Error(err)
			}
		})
	}

	workers.Wait()

	used, err := client.HGet(t.Context(), quotaKey(id), "used").Int64()
	if err != nil || used != 15 {
		t.Fatal("duplicate refund", used, err)
	}

	denied, err := otherStore.Reserve(t.Context(), Budget{KeyID: otherID, Limit: 50, HardLimit: 50, MaxOutputTokens: 1})
	if err != nil || denied.ID != "" {
		t.Fatal("other key's budget changed", denied, err)
	}
}
