package main

import (
	"fmt"
	"time"
)

// config contains validated startup settings. Dependencies are assembled in run.
type config struct {
	server     serverConfig
	chat       chatConfig
	inference  inferenceConfig
	postgres   postgresConfig
	redis      redisConfig
	auth       authConfig
	rateLimit  rateLimitConfig
	quota      quotaConfig
	accounting accountingConfig
	metrics    metricsConfig
	admin      adminConfig
}

// loadConfig is the single environment-loading boundary. Subsystem loaders own
// their defaults and validation; checks spanning subsystems run after assembly.
func loadConfig(getenv func(string) string) (config, error) {
	var (
		cfg config
		err error
	)

	cfg.admin, err = loadAdminConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.postgres, err = loadPostgresConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.redis, err = loadRedisConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.server, err = loadServerConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.auth, err = loadAuthConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.rateLimit, err = loadRateLimitConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.quota, err = loadQuotaConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.metrics, err = loadMetricsConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.accounting, err = loadAccountingConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.chat, err = loadChatConfig(getenv)
	if err != nil {
		return config{}, err
	}

	cfg.inference, err = loadInferenceConfig(getenv)
	if err != nil {
		return config{}, err
	}

	if cfg.admin.enabled && cfg.server.readTimeout+max(cfg.admin.createTimeout, cfg.admin.revokeTimeout) <= 0 {
		return config{}, fmt.Errorf("validate admin write timeout: read and operation timeouts are too large together")
	}

	return cfg, nil
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
