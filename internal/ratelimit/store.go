package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed bucket.lua
var bucketScript string

var takeRequest = redis.NewScript(bucketScript)

// Decision reports admission or the delay until another request credit is available.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RedisStore keeps request credits shared by all gateways using the same Redis database.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore uses the caller-owned Redis client without changing its lifetime.
func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

// Take atomically refills and consumes one request credit for id, with no automatic retries.
func (s *RedisStore) Take(ctx context.Context, id, requestsPerMin int64) (Decision, error) {
	// Lua doubles represent integers exactl                          y only through 2^53 - 1.
	if id <= 0 || requestsPerMin <= 0 || requestsPerMin > 1<<53-1 {
		return Decision{}, fmt.Errorf("validate request bucket: %w", ErrInvalidLimit)
	}

	if s == nil || s.client == nil {
		return Decision{}, fmt.Errorf("take request credit: %w", ErrUnavailable)
	}

	result, err := takeRequest.Run(ctx, s.client, []string{bucketKey(id)}, requestsPerMin).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("take request credit: %w", &redisError{category: ErrUnavailable, cause: err})
	}

	if len(result) != 2 || (result[0] != 0 && result[0] != 1) ||
		(result[0] == 1 && result[1] != 0) ||
		(result[0] == 0 && (result[1] <= 0 || result[1] > math.MaxInt64/int64(time.Millisecond))) {
		return Decision{}, fmt.Errorf("decode request bucket: %w", ErrInvalidResult)
	}

	return Decision{Allowed: result[0] == 1, RetryAfter: time.Duration(result[1]) * time.Millisecond}, nil
}

func bucketKey(id int64) string {
	return "llmgate:requests:v1:" + strconv.FormatInt(id, 10)
}
