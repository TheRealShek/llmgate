package main

import (
	"fmt"
	"strconv"
	"time"
)

// postgresConfig bounds this gateway's Postgres client pool.
type postgresConfig struct {
	url            string
	maxConnections int32
	connectTimeout time.Duration
}

// redisConfig bounds this gateway's Redis client pool.
type redisConfig struct {
	url            string
	maxConnections int
	connectTimeout time.Duration
}

// accountingConfig bounds the shared durable queue and per-attempt usage recording.
type accountingConfig struct {
	writeTimeout          time.Duration // Shared handoff and worker-operation deadline.
	observationMaxBytes   int           // Metadata observation buffer, not an output limit.
	maxAOFBytes           int64
	retentionPeriod       time.Duration
	pruneInterval         time.Duration
	ownershipLeaseTimeout time.Duration
	maxPendingAttempts    int // Active attempts and undelivered outcomes.
	batchSize             int
	flushInterval         time.Duration // Background delivery; inference does not wait for this timer.
}

func loadPostgresConfig(getenv func(string) string) (postgresConfig, error) {
	cfg := postgresConfig{
		url:            getenv("GATEWAY_DATABASE_URL"),
		maxConnections: 10,
		connectTimeout: 5 * time.Second,
	}

	var err error

	if cfg.url == "" {
		return postgresConfig{}, fmt.Errorf("validate GATEWAY_DATABASE_URL: required")
	}

	if val := getenv("GATEWAY_DATABASE_MAX_CONNS"); val != "" {
		n, err := strconv.ParseInt(val, 10, 32)
		if err != nil || n <= 0 {
			return postgresConfig{}, fmt.Errorf("validate GATEWAY_DATABASE_MAX_CONNS: requires a positive integer")
		}

		cfg.maxConnections = int32(n)
	}

	cfg.connectTimeout, err = envDuration(getenv, "GATEWAY_DATABASE_CONNECT_TIMEOUT", cfg.connectTimeout)

	if err != nil {
		return postgresConfig{}, err
	}

	return cfg, nil
}

func loadRedisConfig(getenv func(string) string) (redisConfig, error) {
	cfg := redisConfig{
		url:            "redis://127.0.0.1:6379/0",
		maxConnections: 10,
		connectTimeout: 2 * time.Second,
	}

	var err error

	if val := getenv("GATEWAY_REDIS_URL"); val != "" {
		cfg.url = val
	}

	if val := getenv("GATEWAY_REDIS_MAX_CONNS"); val != "" {
		limit, err := strconv.Atoi(val)
		if err != nil || limit <= 0 {
			return redisConfig{}, fmt.Errorf("validate GATEWAY_REDIS_MAX_CONNS: requires a positive integer")
		}

		cfg.maxConnections = limit
	}

	cfg.connectTimeout, err = envDuration(getenv, "GATEWAY_REDIS_CONNECT_TIMEOUT", cfg.connectTimeout)

	if err != nil {
		return redisConfig{}, err
	}

	return cfg, nil
}

func loadAccountingConfig(getenv func(string) string) (accountingConfig, error) {
	cfg := accountingConfig{
		writeTimeout:          2 * time.Second,
		observationMaxBytes:   64 << 10,
		maxAOFBytes:           256 << 20,
		retentionPeriod:       30 * 24 * time.Hour,
		pruneInterval:         time.Hour,
		ownershipLeaseTimeout: 30 * time.Second,
		maxPendingAttempts:    1024,
		batchSize:             64,
		flushInterval:         250 * time.Millisecond,
	}

	var err error

	cfg.writeTimeout, err = envDuration(getenv, "GATEWAY_USAGE_WRITE_TIMEOUT", cfg.writeTimeout)

	if err != nil {
		return accountingConfig{}, err
	}

	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"GATEWAY_ACCOUNTING_MAX_PENDING", &cfg.maxPendingAttempts},
		{"GATEWAY_ACCOUNTING_BATCH_SIZE", &cfg.batchSize},
	} {
		if value := getenv(setting.name); value != "" {
			*setting.value, err = strconv.Atoi(value)
			if err != nil || *setting.value <= 0 {
				return accountingConfig{}, fmt.Errorf("validate %s: requires a positive integer", setting.name)
			}
		}
	}

	if cfg.batchSize > cfg.maxPendingAttempts {
		return accountingConfig{}, fmt.Errorf("validate accounting batch size: exceeds pending capacity")
	}

	cfg.flushInterval, err = envDuration(getenv, "GATEWAY_ACCOUNTING_FLUSH_INTERVAL", cfg.flushInterval)

	if err != nil {
		return accountingConfig{}, err
	}

	if value := getenv("GATEWAY_ACCOUNTING_MAX_AOF_BYTES"); value != "" {
		cfg.maxAOFBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.maxAOFBytes <= 0 {
			return accountingConfig{}, fmt.Errorf("validate accounting AOF bytes: requires a positive integer")
		}
	}

	cfg.retentionPeriod, err = envDuration(getenv, "GATEWAY_ACCOUNTING_RETENTION", cfg.retentionPeriod)

	if err != nil {
		return accountingConfig{}, err
	}

	cfg.pruneInterval, err = envDuration(getenv, "GATEWAY_ACCOUNTING_PRUNE_INTERVAL", cfg.pruneInterval)

	if err != nil {
		return accountingConfig{}, err
	}

	cfg.ownershipLeaseTimeout, err = envDuration(getenv, "GATEWAY_ACCOUNTING_LEASE_TIMEOUT", cfg.ownershipLeaseTimeout)

	if err != nil {
		return accountingConfig{}, err
	}

	if cfg.writeTimeout > time.Duration(1<<63-1)/8 || cfg.flushInterval > time.Duration(1<<63-1)/8 {
		return accountingConfig{}, fmt.Errorf("validate accounting intervals: duration too large")
	}

	if cfg.ownershipLeaseTimeout <= 3*(cfg.writeTimeout+cfg.flushInterval) {
		return accountingConfig{}, fmt.Errorf("validate accounting lease: must exceed three operation/poll intervals")
	}

	if cfg.writeTimeout < time.Millisecond {
		return accountingConfig{}, fmt.Errorf("validate GATEWAY_USAGE_WRITE_TIMEOUT: requires at least 1ms for durable recording")
	}

	if value := getenv("GATEWAY_USAGE_MAX_BYTES"); value != "" {
		cfg.observationMaxBytes, err = strconv.Atoi(value)
		if err != nil || cfg.observationMaxBytes <= 0 {
			return accountingConfig{}, fmt.Errorf("validate GATEWAY_USAGE_MAX_BYTES: requires a positive integer")
		}
	}

	return cfg, nil
}
