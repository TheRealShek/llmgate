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

	if val := getenv("GATEWAY_READ_HEADER_TIMEOUT"); val != "" {
		d, err := time.ParseDuration(val)
		if err != nil {
			return config{}, fmt.Errorf("parse GATEWAY_READ_HEADER_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_READ_HEADER_TIMEOUT: must be positive, got %v", d)
		}
		cfg.readHeaderTimeout = d
	}

	if val := getenv("GATEWAY_IDLE_TIMEOUT"); val != "" {
		d, err := time.ParseDuration(val)
		if err != nil {
			return config{}, fmt.Errorf("parse GATEWAY_IDLE_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_IDLE_TIMEOUT: must be positive, got %v", d)
		}
		cfg.idleTimeout = d
	}

	if val := getenv("GATEWAY_SHUTDOWN_TIMEOUT"); val != "" {
		d, err := time.ParseDuration(val)
		if err != nil {
			return config{}, fmt.Errorf("parse GATEWAY_SHUTDOWN_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("validate GATEWAY_SHUTDOWN_TIMEOUT: must be positive, got %v", d)
		}
		cfg.shutdownTimeout = d
	}

	return cfg, nil
}
