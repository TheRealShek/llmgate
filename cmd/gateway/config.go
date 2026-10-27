package main

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type config struct {
	// Public HTTP server.
	addr                   string
	readHeaderTimeout      time.Duration
	readTimeout            time.Duration // Includes reading the request body, not generating the response.
	idleTimeout            time.Duration // Between client keep-alive requests.
	shutdownTimeout        time.Duration
	shutdownCleanupTimeout time.Duration // Accounting window after forced cancellation.

	// Chat request limits.
	maxBodyBytes int64
	maxAttempts  int
	maxTokens    int64 // Output-token ceiling and default when omitted.

	// Inference backend.
	backends               []backendConfig
	upstreamConnectTimeout time.Duration
	upstreamHeaderTimeout  time.Duration
	upstreamTimeout        time.Duration // Total JSON request deadline.
	streamIdleTimeout      time.Duration // Maximum gap between backend reads.
	readinessTimeout       time.Duration
	backendHealthInterval  time.Duration
	backendHealthFailures  int
	backendHealthSuccesses int
	backendSlots           int
	backendQueueCapacity   int
	backendQueueTimeout    time.Duration

	// Postgres: API keys and usage records.
	databaseURL            string
	databaseMaxConns       int32
	databaseConnectTimeout time.Duration

	// In-process authentication cache.
	authLookupTimeout time.Duration
	authCacheTTL      time.Duration
	authCacheCapacity int

	// Redis: request rate limits and token reservations.
	redisURL            string
	redisMaxConns       int
	redisConnectTimeout time.Duration
	rateLimitTimeout    time.Duration

	// Daily input and output token quotas.
	quotaGracePercent  int64 // Allowed overage percentage; zero disables grace.
	quotaCountTimeout  time.Duration
	quotaRedisTimeout  time.Duration
	quotaCountMaxBytes int64 // Bounds backend token-counting responses.

	// Durable admission and final usage recording.
	usageWriteTimeout time.Duration // Per write; final writes detach client cancellation.
	usageMaxBytes     int           // Observation buffer limit, not an output limit.

	// Shared durable accounting queue. Active requests retain their final-result slots.
	accountingMaxAOFBytes   int64
	accountingRetention     time.Duration
	accountingPruneInterval time.Duration
	accountingLeaseTimeout  time.Duration
	accountingMaxPending    int
	accountingBatchSize     int
	accountingFlushInterval time.Duration // Background delivery only; inference does not wait for this timer.

	// Prometheus metrics.
	metricsKeyCapacity   int // Zero groups all key IDs under "other".
	metricsMaxScrapes    int
	metricsScrapeTimeout time.Duration

	// Optional loopback admin API.
	adminEnabled       bool
	adminAddr          string
	adminToken         string
	adminMaxBodyBytes  int64
	adminCreateTimeout time.Duration
	adminRevokeTimeout time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		accountingMaxAOFBytes:   256 << 20,
		accountingRetention:     30 * 24 * time.Hour,
		accountingPruneInterval: time.Hour,
		accountingLeaseTimeout:  30 * time.Second,
		accountingMaxPending:    1024,
		accountingBatchSize:     64,
		accountingFlushInterval: 250 * time.Millisecond,
		metricsKeyCapacity:      0,
		metricsMaxScrapes:       2,
		metricsScrapeTimeout:    2 * time.Second,
		adminAddr:               "127.0.0.1:8082",
		adminMaxBodyBytes:       4096,
		adminCreateTimeout:      2 * time.Second,
		adminRevokeTimeout:      2 * time.Second,
		databaseURL:             getenv("GATEWAY_DATABASE_URL"),
		databaseMaxConns:        10,
		databaseConnectTimeout:  5 * time.Second,
		authLookupTimeout:       2 * time.Second,
		authCacheTTL:            30 * time.Second,
		authCacheCapacity:       1024,
		redisURL:                "redis://127.0.0.1:6379/0",
		redisMaxConns:           10,
		redisConnectTimeout:     2 * time.Second,
		rateLimitTimeout:        500 * time.Millisecond,
		quotaGracePercent:       0,
		quotaCountTimeout:       2 * time.Second,
		quotaRedisTimeout:       500 * time.Millisecond,
		quotaCountMaxBytes:      8 << 20,
		usageWriteTimeout:       2 * time.Second,
		usageMaxBytes:           64 << 10,
		addr:                    ":8080",
		upstreamTimeout:         30 * time.Second,
		readHeaderTimeout:       5 * time.Second,
		idleTimeout:             60 * time.Second,
		shutdownTimeout:         10 * time.Second,
		shutdownCleanupTimeout:  3 * time.Second,
		readTimeout:             10 * time.Second,
		maxBodyBytes:            1 << 20, // 1 MiB
		maxTokens:               1024,
		maxAttempts:             2,
		upstreamConnectTimeout:  5 * time.Second,
		upstreamHeaderTimeout:   30 * time.Second,
		streamIdleTimeout:       30 * time.Second,
		readinessTimeout:        2 * time.Second,
		backendHealthInterval:   5 * time.Second,
		backendHealthFailures:   3,
		backendHealthSuccesses:  2,
		backendSlots:            1,
		backendQueueCapacity:    4,
		backendQueueTimeout:     5 * time.Second,
	}

	if val := getenv("GATEWAY_ADMIN_ENABLED"); val != "" {
		enabled, err := strconv.ParseBool(val)
		if err != nil {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ENABLED: requires a boolean: %w",
				&configError{cause: err})
		}

		cfg.adminEnabled = enabled
	}

	if val := getenv("GATEWAY_ADMIN_ADDR"); val != "" {
		cfg.adminAddr = val
	}

	if cfg.adminEnabled {
		cfg.adminToken = getenv("GATEWAY_ADMIN_TOKEN")
		if len(cfg.adminToken) < 32 || len(cfg.adminToken) > 256 {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_TOKEN: requires 32 to 256 URL-safe ASCII characters")
		}

		for _, char := range cfg.adminToken {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '_' || char == '-' {
				continue
			}

			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_TOKEN: requires 32 to 256 URL-safe ASCII characters")
		}

		host, port, err := net.SplitHostPort(cfg.adminAddr)
		if err != nil {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a loopback IP and numeric port: %w",
				&configError{cause: err})
		}

		ip, err := netip.ParseAddr(host)
		if err != nil {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a literal loopback IP: %w",
				&configError{cause: err})
		}

		if !ip.IsLoopback() || ip.Zone() != "" {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a literal loopback IP")
		}

		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a numeric port: %w",
				&configError{cause: err})
		}

		if number == 0 {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a port from 1 to 65535")
		}
	}

	if cfg.databaseURL == "" {
		return config{}, fmt.Errorf("validate GATEWAY_DATABASE_URL: required")
	}

	if val := getenv("GATEWAY_DATABASE_MAX_CONNS"); val != "" {
		n, err := strconv.ParseInt(val, 10, 32)
		if err != nil || n <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_DATABASE_MAX_CONNS: requires a positive integer")
		}

		cfg.databaseMaxConns = int32(n)
	}

	if val := getenv("GATEWAY_REDIS_URL"); val != "" {
		cfg.redisURL = val
	}

	if val := getenv("GATEWAY_REDIS_MAX_CONNS"); val != "" {
		limit, err := strconv.Atoi(val)
		if err != nil || limit <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_REDIS_MAX_CONNS: requires a positive integer")
		}

		cfg.redisMaxConns = limit
	}

	if val := getenv("GATEWAY_ADDR"); val != "" {
		cfg.addr = val
	}

	var err error

	cfg.adminCreateTimeout, err = envDuration(getenv, "GATEWAY_ADMIN_CREATE_TIMEOUT", cfg.adminCreateTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.adminRevokeTimeout, err = envDuration(getenv, "GATEWAY_ADMIN_REVOKE_TIMEOUT", cfg.adminRevokeTimeout)
	if err != nil {
		return config{}, err
	}

	if val := getenv("GATEWAY_ADMIN_MAX_BODY_BYTES"); val != "" {
		cfg.adminMaxBodyBytes, err = strconv.ParseInt(val, 10, 64)
		if err != nil {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_MAX_BODY_BYTES: requires a positive integer: %w",
				&configError{cause: err})
		}

		if cfg.adminMaxBodyBytes <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_ADMIN_MAX_BODY_BYTES: requires a positive integer")
		}
	}

	cfg.databaseConnectTimeout, err = envDuration(getenv, "GATEWAY_DATABASE_CONNECT_TIMEOUT", cfg.databaseConnectTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.authLookupTimeout, err = envDuration(getenv, "GATEWAY_AUTH_LOOKUP_TIMEOUT", cfg.authLookupTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.authCacheTTL, err = envDuration(getenv, "GATEWAY_AUTH_CACHE_TTL", cfg.authCacheTTL)
	if err != nil {
		return config{}, err
	}

	if val := getenv("GATEWAY_AUTH_CACHE_CAPACITY"); val != "" {
		cfg.authCacheCapacity, err = strconv.Atoi(val)
		if err != nil || cfg.authCacheCapacity <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_AUTH_CACHE_CAPACITY: requires a positive integer")
		}
	}

	cfg.redisConnectTimeout, err = envDuration(getenv, "GATEWAY_REDIS_CONNECT_TIMEOUT", cfg.redisConnectTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.rateLimitTimeout, err = envDuration(getenv, "GATEWAY_RATE_LIMIT_TIMEOUT", cfg.rateLimitTimeout)
	if err != nil {
		return config{}, err
	}

	if value := getenv("GATEWAY_QUOTA_GRACE_PERCENT"); value != "" {
		cfg.quotaGracePercent, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.quotaGracePercent < 0 || cfg.quotaGracePercent > 100 {
			return config{}, fmt.Errorf("validate GATEWAY_QUOTA_GRACE_PERCENT: requires an integer from 0 to 100")
		}
	}

	cfg.quotaCountTimeout, err = envDuration(getenv, "GATEWAY_QUOTA_COUNT_TIMEOUT", cfg.quotaCountTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.quotaRedisTimeout, err = envDuration(getenv, "GATEWAY_QUOTA_REDIS_TIMEOUT", cfg.quotaRedisTimeout)
	if err != nil {
		return config{}, err
	}

	if value := getenv("GATEWAY_QUOTA_COUNT_MAX_BYTES"); value != "" {
		cfg.quotaCountMaxBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.quotaCountMaxBytes <= 0 || cfg.quotaCountMaxBytes >= 1<<63-1 {
			return config{}, fmt.Errorf("validate GATEWAY_QUOTA_COUNT_MAX_BYTES: requires a positive response bound below MaxInt64")
		}
	}

	if value := getenv("GATEWAY_METRICS_KEY_CAPACITY"); value != "" {
		cfg.metricsKeyCapacity, err = strconv.Atoi(value)
		if err != nil || cfg.metricsKeyCapacity < 0 {
			return config{}, fmt.Errorf("validate GATEWAY_METRICS_KEY_CAPACITY: requires a non-negative integer")
		}
	}

	if value := getenv("GATEWAY_METRICS_MAX_SCRAPES"); value != "" {
		cfg.metricsMaxScrapes, err = strconv.Atoi(value)
		if err != nil || cfg.metricsMaxScrapes <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_METRICS_MAX_SCRAPES: requires a positive integer")
		}
	}

	cfg.metricsScrapeTimeout, err = envDuration(getenv, "GATEWAY_METRICS_SCRAPE_TIMEOUT", cfg.metricsScrapeTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.usageWriteTimeout, err = envDuration(getenv, "GATEWAY_USAGE_WRITE_TIMEOUT", cfg.usageWriteTimeout)
	if err != nil {
		return config{}, err
	}

	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"GATEWAY_ACCOUNTING_MAX_PENDING", &cfg.accountingMaxPending},
		{"GATEWAY_ACCOUNTING_BATCH_SIZE", &cfg.accountingBatchSize},
	} {
		if value := getenv(setting.name); value != "" {
			*setting.value, err = strconv.Atoi(value)
			if err != nil || *setting.value <= 0 {
				return config{}, fmt.Errorf("validate %s: requires a positive integer", setting.name)
			}
		}
	}

	if cfg.accountingBatchSize > cfg.accountingMaxPending {
		return config{}, fmt.Errorf("validate accounting batch size: exceeds pending capacity")
	}

	cfg.accountingFlushInterval, err = envDuration(getenv, "GATEWAY_ACCOUNTING_FLUSH_INTERVAL", cfg.accountingFlushInterval)
	if err != nil {
		return config{}, err
	}

	if value := getenv("GATEWAY_ACCOUNTING_MAX_AOF_BYTES"); value != "" {
		cfg.accountingMaxAOFBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil || cfg.accountingMaxAOFBytes <= 0 {
			return config{}, fmt.Errorf("validate accounting AOF bytes: requires a positive integer")
		}
	}

	cfg.accountingRetention, err = envDuration(getenv, "GATEWAY_ACCOUNTING_RETENTION", cfg.accountingRetention)
	if err != nil {
		return config{}, err
	}

	cfg.accountingPruneInterval, err = envDuration(getenv, "GATEWAY_ACCOUNTING_PRUNE_INTERVAL", cfg.accountingPruneInterval)
	if err != nil {
		return config{}, err
	}

	cfg.accountingLeaseTimeout, err = envDuration(getenv, "GATEWAY_ACCOUNTING_LEASE_TIMEOUT", cfg.accountingLeaseTimeout)
	if err != nil {
		return config{}, err
	}

	if cfg.usageWriteTimeout > time.Duration(1<<63-1)/8 || cfg.accountingFlushInterval > time.Duration(1<<63-1)/8 {
		return config{}, fmt.Errorf("validate accounting intervals: duration too large")
	}

	if cfg.accountingLeaseTimeout <= 3*(cfg.usageWriteTimeout+cfg.accountingFlushInterval) {
		return config{}, fmt.Errorf("validate accounting lease: must exceed three operation/poll intervals")
	}

	if cfg.usageWriteTimeout < time.Millisecond {
		return config{}, fmt.Errorf("validate GATEWAY_USAGE_WRITE_TIMEOUT: requires at least 1ms for durable recording")
	}

	if value := getenv("GATEWAY_USAGE_MAX_BYTES"); value != "" {
		cfg.usageMaxBytes, err = strconv.Atoi(value)
		if err != nil || cfg.usageMaxBytes <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_USAGE_MAX_BYTES: requires a positive integer")
		}
	}

	cfg.backendQueueTimeout, err = envDuration(getenv, "GATEWAY_BACKEND_QUEUE_TIMEOUT", cfg.backendQueueTimeout)
	if err != nil {
		return config{}, err
	}

	if value := getenv("GATEWAY_BACKEND_SLOTS"); value != "" {
		cfg.backendSlots, err = strconv.Atoi(value)
		if err != nil || cfg.backendSlots <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_BACKEND_SLOTS: requires a positive integer")
		}
	}

	if value := getenv("GATEWAY_BACKEND_QUEUE_CAPACITY"); value != "" {
		cfg.backendQueueCapacity, err = strconv.Atoi(value)
		if err != nil || cfg.backendQueueCapacity < 0 {
			return config{}, fmt.Errorf("validate GATEWAY_BACKEND_QUEUE_CAPACITY: requires a nonnegative integer")
		}
	}

	cfg.backendHealthInterval, err = envDuration(getenv, "GATEWAY_BACKEND_HEALTH_INTERVAL", cfg.backendHealthInterval)
	if err != nil {
		return config{}, err
	}

	for _, setting := range []struct {
		name  string
		value *int
	}{
		{name: "GATEWAY_BACKEND_HEALTH_FAILURES", value: &cfg.backendHealthFailures},
		{name: "GATEWAY_BACKEND_HEALTH_SUCCESSES", value: &cfg.backendHealthSuccesses},
	} {
		if value := getenv(setting.name); value != "" {
			*setting.value, err = strconv.Atoi(value)
			if err != nil || *setting.value <= 0 {
				return config{}, fmt.Errorf("validate %s: requires a positive integer", setting.name)
			}
		}
	}

	cfg.readinessTimeout, err = envDuration(getenv, "GATEWAY_READINESS_TIMEOUT", cfg.readinessTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.upstreamConnectTimeout, err = envDuration(
		getenv,
		"GATEWAY_UPSTREAM_CONNECT_TIMEOUT",
		cfg.upstreamConnectTimeout,
	)
	if err != nil {
		return config{}, err
	}

	cfg.upstreamHeaderTimeout, err = envDuration(
		getenv,
		"GATEWAY_UPSTREAM_HEADER_TIMEOUT",
		cfg.upstreamHeaderTimeout,
	)
	if err != nil {
		return config{}, err
	}

	cfg.streamIdleTimeout, err = envDuration(
		getenv,
		"GATEWAY_STREAM_IDLE_TIMEOUT",
		cfg.streamIdleTimeout,
	)
	if err != nil {
		return config{}, err
	}

	if value := getenv("GATEWAY_MAX_ATTEMPTS"); value != "" {
		cfg.maxAttempts, err = strconv.Atoi(value)
		if err != nil || cfg.maxAttempts < 1 || cfg.maxAttempts > 2 {
			return config{}, fmt.Errorf("validate GATEWAY_MAX_ATTEMPTS: requires 1 or 2")
		}
	}

	if val := getenv("GATEWAY_MAX_TOKENS"); val != "" {
		cfg.maxTokens, err = strconv.ParseInt(val, 10, 64)
		if err != nil || cfg.maxTokens <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_MAX_TOKENS: requires a positive integer")
		}
	}

	if val := getenv("GATEWAY_MAX_BODY_BYTES"); val != "" {
		cfg.maxBodyBytes, err = strconv.ParseInt(val, 10, 64)
		if err != nil || cfg.maxBodyBytes <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_MAX_BODY_BYTES: requires a positive integer")
		}
	}

	cfg.readTimeout, err = envDuration(getenv, "GATEWAY_READ_TIMEOUT", cfg.readTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.backends, err = loadBackends(getenv, cfg.backendSlots, cfg.backendQueueCapacity)
	if err != nil {
		return config{}, err
	}

	cfg.upstreamTimeout, err = envDuration(getenv, "GATEWAY_UPSTREAM_TIMEOUT", cfg.upstreamTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.readHeaderTimeout, err = envDuration(
		getenv,
		"GATEWAY_READ_HEADER_TIMEOUT",
		cfg.readHeaderTimeout,
	)
	if err != nil {
		return config{}, err
	}

	cfg.idleTimeout, err = envDuration(getenv, "GATEWAY_IDLE_TIMEOUT", cfg.idleTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.shutdownTimeout, err = envDuration(getenv, "GATEWAY_SHUTDOWN_TIMEOUT", cfg.shutdownTimeout)
	if err != nil {
		return config{}, err
	}

	cfg.shutdownCleanupTimeout, err = envDuration(getenv, "GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT", cfg.shutdownCleanupTimeout)
	if err != nil {
		return config{}, err
	}

	if cfg.adminEnabled && cfg.readTimeout+max(cfg.adminCreateTimeout, cfg.adminRevokeTimeout) <= 0 {
		return config{}, fmt.Errorf("validate admin write timeout: read and operation timeouts are too large together")
	}

	return cfg, nil
}

func parseUpstreamURL(raw string) (*url.URL, error) {
	upstream, err := url.Parse(raw)
	if err != nil {
		// Parse errors include the raw URL, which may contain credentials.
		return nil, fmt.Errorf("parse GATEWAY_UPSTREAM_URL: invalid URL")
	}

	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Hostname() == "" {
		return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: requires HTTP or HTTPS and a host")
	}

	if upstream.User != nil ||
		upstream.RawQuery != "" ||
		upstream.ForceQuery ||
		strings.Contains(raw, "#") {
		return nil, fmt.Errorf(
			"validate GATEWAY_UPSTREAM_URL: credentials, query strings, and fragments are unsupported",
		)
	}

	if port := upstream.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: port must be between 1 and 65535")
		}
	}

	return upstream, nil
}

func envDuration(
	getenv func(string) string,
	key string,
	fallback time.Duration,
) (time.Duration, error) {
	val := getenv(key)
	if val == "" {
		return fallback, nil
	}

	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, &configError{cause: err})
	}

	if d <= 0 {
		return 0, fmt.Errorf("validate %s: must be positive, got %v", key, d)
	}

	return d, nil
}
