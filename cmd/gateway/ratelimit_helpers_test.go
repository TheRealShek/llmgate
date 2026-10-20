package main

import (
	"context"
	"time"

	"github.com/therealshek/llmgate/internal/ratelimit"
)

// allowRequestBuckets isolates existing route and logging tests from Redis.
type allowRequestBuckets struct{}

func (allowRequestBuckets) Take(context.Context, int64, int64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

func testRequestLimits() *ratelimit.Limiter {
	return ratelimit.NewLimiter(allowRequestBuckets{}, time.Second)
}
