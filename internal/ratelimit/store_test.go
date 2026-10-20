package ratelimit

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/therealshek/llmgate/internal/auth"
	"github.com/therealshek/llmgate/internal/store"
)

func redisTestClient(t *testing.T) *redis.Client {
	t.Helper()

	redisURL := os.Getenv("LLMGATE_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set LLMGATE_TEST_REDIS_URL to run Redis integration tests")
	}

	client, err := OpenClient(t.Context(), redisURL, ClientLimits{
		MaxConns: 4, ConnectTimeout: 2 * time.Second, CommandTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

func redisTestKey(t *testing.T, client *redis.Client) int64 {
	t.Helper()

	number, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt64-1))
	if err != nil {
		t.Fatal(err)
	}

	id := number.Int64() + 1

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if err := client.Del(ctx, bucketKey(id)).Err(); err != nil {
			t.Error("could not remove test bucket")
		}
	})

	return id
}

// TestRedisAtomicAdmission checks independent clients share one bucket without double-spending credits.
func TestRedisAtomicAdmission(t *testing.T) {
	first, second := redisTestClient(t), redisTestClient(t)
	id := redisTestKey(t, first)
	stores := []*RedisStore{NewRedisStore(first), NewRedisStore(second)}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var (
		admitted atomic.Int32
		workers  sync.WaitGroup
	)
	for i := range 64 {
		workers.Go(func() {
			decision, err := stores[i%2].Take(ctx, id, 3)
			if err != nil {
				t.Error(err)
				return
			}

			if decision.Allowed {
				admitted.Add(1)
			} else if decision.RetryAfter <= 0 || decision.RetryAfter > 20*time.Second {
				t.Error("invalid retry guidance", decision.RetryAfter)
			}
		})
	}

	workers.Wait()

	if admitted.Load() != 3 {
		t.Fatal("concurrent requests overspent or lost credits", admitted.Load())
	}

	for _, client := range []*redis.Client{first, second} {
		if client.PoolStats().TotalConns > 4 {
			t.Fatal("Redis pool exceeded its configured connection bound")
		}
	}
}

// TestRedisDifferentKeyLimits checks identical HTTP load produces different per-key admission results.
func TestRedisDifferentKeyLimits(t *testing.T) {
	client := redisTestClient(t)
	buckets := NewRedisStore(client)
	low, high := redisTestKey(t, client), redisTestKey(t, client)
	admitted := make(map[string]int)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	a := auth.NewAuthenticator(testKeys{identities: map[string]store.Key{
		"low":  {ID: low, RequestsPerMin: 2},
		"high": {ID: high, RequestsPerMin: 4},
	}}, auth.Options{LookupTimeout: time.Second, CacheTTL: 30 * time.Second, CacheCapacity: 8})
	handler := a.Protect(NewLimiter(buckets, time.Second).Protect(next))

	for _, secret := range []string{"low", "high"} {
		for range 3 {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request(secret))

			switch w.Code {
			case http.StatusNoContent:
				admitted[secret]++
			case http.StatusTooManyRequests:
				if w.Header().Get("Retry-After") == "" {
					t.Fatal("rate rejection has no retry guidance")
				}
			default:
				t.Fatal("unexpected response", w.Code)
			}
		}
	}

	if admitted["low"] != 2 || admitted["high"] != 3 {
		t.Fatal("different keys did not enforce their independent limits", admitted)
	}
}

// TestRedisRefillAndExpiry checks server-clock refill, backward-clock protection, and bounded idle state.
func TestRedisRefillAndExpiry(t *testing.T) {
	client := redisTestClient(t)
	buckets := NewRedisStore(client)
	id := redisTestKey(t, client)
	ctx := t.Context()

	now, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}

	if err := client.HSet(ctx, bucketKey(id), "tokens", 0, "updated_ms", now.UnixMilli()-30000).Err(); err != nil {
		t.Fatal(err)
	}

	first, err := buckets.Take(ctx, id, 2)
	if err != nil || !first.Allowed {
		t.Fatal("elapsed time did not refill a credit", first, err)
	}

	second, err := buckets.Take(ctx, id, 2)
	if err != nil || second.Allowed || second.RetryAfter < 29*time.Second {
		t.Fatal("refill admitted an extra request", second, err)
	}

	ttl, err := client.PTTL(ctx, bucketKey(id)).Result()
	if err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatal("bucket has no bounded lifetime", ttl, err)
	}

	if err := client.HSet(ctx, bucketKey(id), "tokens", 0, "updated_ms", now.Add(time.Minute).UnixMilli()).Err(); err != nil {
		t.Fatal(err)
	}

	decision, err := buckets.Take(ctx, id, 60)
	if err != nil || decision.Allowed || decision.RetryAfter <= time.Minute || decision.RetryAfter > 61*time.Second {
		t.Fatal("backward clock minted credits or omitted recovery time", decision, err)
	}

	ttl, err = client.PTTL(ctx, bucketKey(id)).Result()
	if err != nil || ttl <= time.Minute || ttl > 2*time.Minute {
		t.Fatal("bucket could expire before clock recovery and refill", ttl, err)
	}

	if err := client.PExpire(ctx, bucketKey(id), time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)

	for {
		exists, err := client.Exists(ctx, bucketKey(id)).Result()
		if err != nil {
			t.Fatal(err)
		}

		if exists == 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("idle bucket did not expire")
		}

		time.Sleep(time.Millisecond)
	}

	decision, err = buckets.Take(ctx, id, 2)
	if err != nil || !decision.Allowed {
		t.Fatal("expired bucket did not regain its initial allowance", decision, err)
	}
}

// TestRedisLimitChanges checks lower limits clamp existing credits without granting a new bucket.
func TestRedisLimitChanges(t *testing.T) {
	client := redisTestClient(t)
	buckets := NewRedisStore(client)
	id := redisTestKey(t, client)

	if _, err := buckets.Take(t.Context(), id, 100); err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		decision, err := buckets.Take(t.Context(), id, 2)
		if err != nil || decision.Allowed != (i < 2) {
			t.Fatal("lower limit did not cap existing allowance", i, decision, err)
		}
	}
}

// TestRedisLargeLimitPrecision checks Lua storage preserves integer credits near its exact-number boundary.
func TestRedisLargeLimitPrecision(t *testing.T) {
	client := redisTestClient(t)
	buckets := NewRedisStore(client)
	id := redisTestKey(t, client)

	const limit int64 = 1<<53 - 1

	now, err := client.Time(t.Context()).Result()
	if err != nil {
		t.Fatal(err)
	}

	if err := client.HSet(t.Context(), bucketKey(id), "tokens", strconv.FormatInt(limit, 10),
		"updated_ms", now.Add(time.Minute).UnixMilli()).Err(); err != nil {
		t.Fatal(err)
	}

	for i := int64(1); i <= 2; i++ {
		decision, err := buckets.Take(t.Context(), id, limit)
		if err != nil || !decision.Allowed {
			t.Fatal(decision, err)
		}

		remaining, err := client.HGet(t.Context(), bucketKey(id), "tokens").Int64()
		if err != nil || remaining != limit-i {
			t.Fatal("Lua rounded away a credit decrement", remaining, err)
		}
	}
}

// TestRedisFailureAndRecovery checks a failed client cannot bypass admission or reset persisted credits.
func TestRedisFailureAndRecovery(t *testing.T) {
	cleanupClient, failingClient := redisTestClient(t), redisTestClient(t)
	id := redisTestKey(t, cleanupClient)
	buckets := NewRedisStore(failingClient)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := buckets.Take(ctx, id, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled admission did not preserve its cause", err)
	}

	if decision, err := buckets.Take(t.Context(), id, 1); err != nil || !decision.Allowed {
		t.Fatal("initial admission failed", decision, err)
	}

	if err := failingClient.Close(); err != nil {
		t.Fatal(err)
	}

	decision, err := buckets.Take(t.Context(), id, 1)
	if !errors.Is(err, ErrUnavailable) || decision.Allowed {
		t.Fatal("unavailable Redis admitted work", decision, err)
	}

	decision, err = NewRedisStore(cleanupClient).Take(t.Context(), id, 1)
	if err != nil || decision.Allowed {
		t.Fatal("recovery lost the shared bucket", decision, err)
	}
}

// TestRedisInvalidState checks script errors fail closed instead of granting a fresh bucket.
func TestRedisInvalidState(t *testing.T) {
	client := redisTestClient(t)
	id := redisTestKey(t, client)

	if err := client.Set(t.Context(), bucketKey(id), "invalid-bucket-type", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}

	decision, err := NewRedisStore(client).Take(t.Context(), id, 1)
	if !errors.Is(err, ErrUnavailable) || decision.Allowed {
		t.Fatal("script failure admitted a request", decision, err)
	}
}

// TestInvalidBuckets checks unsupported limits fail closed before any Redis access.
func TestInvalidBuckets(t *testing.T) {
	for _, limit := range []int64{0, -1, math.MaxInt64} {
		decision, err := NewRedisStore(nil).Take(t.Context(), 1, limit)
		if decision.Allowed || !errors.Is(err, ErrInvalidLimit) {
			t.Fatal("invalid limit was accepted", limit, decision, err)
		}
	}

	if _, err := NewRedisStore(nil).Take(t.Context(), 0, 60); !errors.Is(err, ErrInvalidLimit) {
		t.Fatal("invalid key ID was accepted", err)
	}
}
