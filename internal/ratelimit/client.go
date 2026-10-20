package ratelimit

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"
)

// ClientLimits bounds Redis connections, startup, and network operations.
type ClientLimits struct {
	MaxConns       int
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
}

// OpenClient validates and pings Redis before serving. The caller closes it after HTTP draining.
func OpenClient(ctx context.Context, redisURL string, limits ClientLimits) (*redis.Client, error) {
	u, err := url.Parse(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse Redis URL: %w", &redisError{category: ErrInvalidConfig, cause: err})
	}

	if (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		limits.MaxConns <= 0 || limits.ConnectTimeout <= 0 || limits.CommandTimeout <= 0 {
		return nil, fmt.Errorf("validate Redis configuration: %w", ErrInvalidConfig)
	}

	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse Redis options: %w", &redisError{category: ErrInvalidConfig, cause: err})
	}

	options.PoolSize = limits.MaxConns
	options.MaxActiveConns = limits.MaxConns
	options.PipelinePoolSize = -1
	options.DialTimeout = limits.ConnectTimeout
	options.ReadTimeout = limits.CommandTimeout
	options.WriteTimeout = limits.CommandTimeout
	options.PoolTimeout = limits.CommandTimeout
	options.ContextTimeoutEnabled = true
	options.Protocol = 2
	// A lost reply may follow a committed decrement. Never retry the decision.
	options.MaxRetries = -1

	client := redis.NewClient(options)

	connectCtx, cancel := context.WithTimeout(ctx, limits.ConnectTimeout)
	defer cancel()

	if err := client.Ping(connectCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect Redis: %w", &redisError{category: ErrUnavailable, cause: err})
	}

	return client, nil
}
