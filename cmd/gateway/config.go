package main

import (
	"fmt"
	"time"
)

type config struct {
	addr              string
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	shutdownTimeout   time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		addr:              ":8080",
		readHeaderTimeout: 5 * time.Second,
		idleTimeout:       60 * time.Second,
		shutdownTimeout:   10 * time.Second,
	}

	if val := getenv("GATEWAY_ADDR"); val != "" {
		cfg.addr = val
	}

	var err error
	cfg.readHeaderTimeout, err = envDuration(getenv, "GATEWAY_READ_HEADER_TIMEOUT", cfg.readHeaderTimeout)
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

	return cfg, nil
}

func envDuration(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	val := getenv(key)
	if val == "" {
		return fallback, nil
	}

	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("validate %s: must be positive, got %v", key, d)
	}
	return d, nil
}
