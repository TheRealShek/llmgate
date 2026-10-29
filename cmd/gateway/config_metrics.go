package main

import (
	"fmt"
	"strconv"
	"time"
)

type metricsConfig struct {
	keyLabelCapacity     int // Zero groups authenticated key IDs under "other".
	maxConcurrentScrapes int
	scrapeTimeout        time.Duration
}

func loadMetricsConfig(getenv func(string) string) (metricsConfig, error) {
	cfg := metricsConfig{
		keyLabelCapacity:     0,
		maxConcurrentScrapes: 2,
		scrapeTimeout:        2 * time.Second,
	}

	var err error

	if value := getenv("GATEWAY_METRICS_KEY_CAPACITY"); value != "" {
		cfg.keyLabelCapacity, err = strconv.Atoi(value)
		if err != nil || cfg.keyLabelCapacity < 0 {
			return metricsConfig{}, fmt.Errorf("validate GATEWAY_METRICS_KEY_CAPACITY: requires a non-negative integer")
		}
	}

	if value := getenv("GATEWAY_METRICS_MAX_SCRAPES"); value != "" {
		cfg.maxConcurrentScrapes, err = strconv.Atoi(value)
		if err != nil || cfg.maxConcurrentScrapes <= 0 {
			return metricsConfig{}, fmt.Errorf("validate GATEWAY_METRICS_MAX_SCRAPES: requires a positive integer")
		}
	}

	cfg.scrapeTimeout, err = envDuration(getenv, "GATEWAY_METRICS_SCRAPE_TIMEOUT", cfg.scrapeTimeout)

	if err != nil {
		return metricsConfig{}, err
	}

	return cfg, nil
}
