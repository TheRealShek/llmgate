package main

import (
	"fmt"
	"strconv"
	"time"
)

// inferenceConfig separates backend targets, proxy deadlines, health, and admission defaults.
type inferenceConfig struct {
	targets               []backendConfig
	connectTimeout        time.Duration
	responseHeaderTimeout time.Duration
	jsonResponseTimeout   time.Duration // Per JSON attempt, including reading the response body.
	streamIdleTimeout     time.Duration // Upstream read gaps and terminal SSE error delivery.
	health                backendHealthConfig
	admission             inferenceAdmissionConfig
	circuitBreaker        backendCircuitConfig
}

type backendCircuitConfig struct {
	failureThreshold int
	cooldown         time.Duration
}

type backendHealthConfig struct {
	probeInterval    time.Duration
	probeTimeout     time.Duration // Readiness itself only reads the resulting snapshot.
	failureThreshold int           // Consecutive failed probes before becoming unhealthy.
	successThreshold int           // Consecutive successful probes before becoming healthy.
}

type inferenceAdmissionConfig struct {
	defaultActiveSlots   int
	defaultQueueCapacity int
	queueWaitTimeout     time.Duration
}

func loadInferenceConfig(getenv func(string) string) (inferenceConfig, error) {
	cfg := inferenceConfig{
		connectTimeout:        5 * time.Second,
		circuitBreaker:        backendCircuitConfig{failureThreshold: 3, cooldown: 10 * time.Second},
		responseHeaderTimeout: 30 * time.Second,
		jsonResponseTimeout:   30 * time.Second,
		streamIdleTimeout:     30 * time.Second,
		health: backendHealthConfig{
			probeTimeout:     2 * time.Second,
			probeInterval:    5 * time.Second,
			failureThreshold: 3,
			successThreshold: 2,
		},
		admission: inferenceAdmissionConfig{
			defaultActiveSlots:   1,
			defaultQueueCapacity: 4,
			queueWaitTimeout:     5 * time.Second,
		},
	}

	var err error

	cfg.admission.queueWaitTimeout, err = envDuration(getenv, "GATEWAY_BACKEND_QUEUE_TIMEOUT", cfg.admission.queueWaitTimeout)

	if err != nil {
		return inferenceConfig{}, err
	}

	if value := getenv("GATEWAY_BACKEND_SLOTS"); value != "" {
		cfg.admission.defaultActiveSlots, err = strconv.Atoi(value)
		if err != nil || cfg.admission.defaultActiveSlots <= 0 {
			return inferenceConfig{}, fmt.Errorf("validate GATEWAY_BACKEND_SLOTS: requires a positive integer")
		}
	}

	if value := getenv("GATEWAY_BACKEND_QUEUE_CAPACITY"); value != "" {
		cfg.admission.defaultQueueCapacity, err = strconv.Atoi(value)
		if err != nil || cfg.admission.defaultQueueCapacity < 0 {
			return inferenceConfig{}, fmt.Errorf("validate GATEWAY_BACKEND_QUEUE_CAPACITY: requires a nonnegative integer")
		}
	}

	cfg.health.probeInterval, err = envDuration(getenv, "GATEWAY_BACKEND_HEALTH_INTERVAL", cfg.health.probeInterval)

	if err != nil {
		return inferenceConfig{}, err
	}

	for _, setting := range []struct {
		name  string
		value *int
	}{
		{name: "GATEWAY_BACKEND_CIRCUIT_FAILURES", value: &cfg.circuitBreaker.failureThreshold},
		{name: "GATEWAY_BACKEND_HEALTH_FAILURES", value: &cfg.health.failureThreshold},
		{name: "GATEWAY_BACKEND_HEALTH_SUCCESSES", value: &cfg.health.successThreshold},
	} {
		if value := getenv(setting.name); value != "" {
			*setting.value, err = strconv.Atoi(value)
			if err != nil || *setting.value <= 0 {
				return inferenceConfig{}, fmt.Errorf("validate %s: requires a positive integer", setting.name)
			}
		}
	}

	cfg.circuitBreaker.cooldown, err = envDuration(getenv, "GATEWAY_BACKEND_CIRCUIT_COOLDOWN", cfg.circuitBreaker.cooldown)
	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.health.probeTimeout, err = envDuration(getenv, "GATEWAY_READINESS_TIMEOUT", cfg.health.probeTimeout)

	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.connectTimeout, err = envDuration(
		getenv,
		"GATEWAY_UPSTREAM_CONNECT_TIMEOUT",
		cfg.connectTimeout,
	)

	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.responseHeaderTimeout, err = envDuration(
		getenv,
		"GATEWAY_UPSTREAM_HEADER_TIMEOUT",
		cfg.responseHeaderTimeout,
	)

	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.streamIdleTimeout, err = envDuration(
		getenv,
		"GATEWAY_STREAM_IDLE_TIMEOUT",
		cfg.streamIdleTimeout,
	)

	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.targets, err = loadBackends(getenv, cfg.admission.defaultActiveSlots, cfg.admission.defaultQueueCapacity)

	if err != nil {
		return inferenceConfig{}, err
	}

	cfg.jsonResponseTimeout, err = envDuration(getenv, "GATEWAY_UPSTREAM_TIMEOUT", cfg.jsonResponseTimeout)

	if err != nil {
		return inferenceConfig{}, err
	}

	return cfg, nil
}
