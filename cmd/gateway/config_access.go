package main

import (
	"fmt"
	"strconv"
	"time"
)

type authConfig struct {
	keyStoreTimeout time.Duration // Authentication lookups and key-create CLI writes.
	cacheTTL        time.Duration
	cacheCapacity   int
}

type rateLimitConfig struct {
	checkTimeout time.Duration
}

type quotaConfig struct {
	gracePercent          int64 // Zero disables daily quota grace.
	promptCountTimeout    time.Duration
	redisTimeout          time.Duration // Reservation and settlement operations.
	maxCountResponseBytes int64         // Backend counting responses, not client prompt size.
}

func loadAuthConfig(getenv func(string) string) (authConfig, error) {
	cfg := authConfig{
		keyStoreTimeout: 2 * time.Second,
		cacheTTL:        30 * time.Second,
		cacheCapacity:   1024,
	}

	var err error

	cfg.keyStoreTimeout, err = envDuration(getenv, "GATEWAY_AUTH_LOOKUP_TIMEOUT", cfg.keyStoreTimeout)

	if err != nil {
		return authConfig{}, err
	}

	cfg.cacheTTL, err = envDuration(getenv, "GATEWAY_AUTH_CACHE_TTL", cfg.cacheTTL)

	if err != nil {
		return authConfig{}, err
	}

	if val := getenv("GATEWAY_AUTH_CACHE_CAPACITY"); val != "" {
		cfg.cacheCapacity, err = strconv.Atoi(val)
		if err != nil || cfg.cacheCapacity <= 0 {
			return authConfig{}, fmt.Errorf("validate GATEWAY_AUTH_CACHE_CAPACITY: requires a positive integer")
		}
	}

	return cfg, nil
}

func loadRateLimitConfig(getenv func(string) string) (rateLimitConfig, error) {
	cfg := rateLimitConfig{
		checkTimeout: 500 * time.Millisecond,
	}

	var err error

	cfg.checkTimeout, err = envDuration(getenv, "GATEWAY_RATE_LIMIT_TIMEOUT", cfg.checkTimeout)

	if err != nil {
		return rateLimitConfig{}, err
	}

	return cfg, nil
}

func loadQuotaConfig(getenv func(string) string) (quotaConfig, error) {
	cfg := quotaConfig{
		gracePercent:          0,
		promptCountTimeout:    2 * time.Second,
		redisTimeout:          500 * time.Millisecond,
		maxCountResponseBytes: 8 << 20,
	}

	var err error

	if value := getenv("GATEWAY_QUOTA_GRACE_PERCENT"); value != "" {
		cfg.gracePercent, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.gracePercent < 0 || cfg.gracePercent > 100 {
			return quotaConfig{}, fmt.Errorf("validate GATEWAY_QUOTA_GRACE_PERCENT: requires an integer from 0 to 100")
		}
	}

	cfg.promptCountTimeout, err = envDuration(getenv, "GATEWAY_QUOTA_COUNT_TIMEOUT", cfg.promptCountTimeout)

	if err != nil {
		return quotaConfig{}, err
	}

	cfg.redisTimeout, err = envDuration(getenv, "GATEWAY_QUOTA_REDIS_TIMEOUT", cfg.redisTimeout)

	if err != nil {
		return quotaConfig{}, err
	}

	if value := getenv("GATEWAY_QUOTA_COUNT_MAX_BYTES"); value != "" {
		cfg.maxCountResponseBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.maxCountResponseBytes <= 0 || cfg.maxCountResponseBytes >= 1<<63-1 {
			return quotaConfig{}, fmt.Errorf("validate GATEWAY_QUOTA_COUNT_MAX_BYTES: requires a positive response bound below MaxInt64")
		}
	}

	return cfg, nil
}
