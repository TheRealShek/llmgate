package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type config struct {
	addr              string
	upstreamURL       *url.URL
	upstreamTimeout   time.Duration
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	shutdownTimeout   time.Duration
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		addr:              ":8080",
		upstreamTimeout:   30 * time.Second,
		readHeaderTimeout: 5 * time.Second,
		idleTimeout:       60 * time.Second,
		shutdownTimeout:   10 * time.Second,
	}

	if val := getenv("GATEWAY_ADDR"); val != "" {
		cfg.addr = val
	}

	var err error
	if val := getenv("GATEWAY_UPSTREAM_URL"); val != "" {
		cfg.upstreamURL, err = parseUpstreamURL(val)
		if err != nil {
			return config{}, err
		}
	}

	cfg.upstreamTimeout, err = envDuration(getenv, "GATEWAY_UPSTREAM_TIMEOUT", cfg.upstreamTimeout)
	if err != nil {
		return config{}, err
	}

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

func parseUpstreamURL(raw string) (*url.URL, error) {
	upstream, err := url.Parse(raw)
	if err != nil {
		// Parse errors include the raw URL, which may contain credentials.
		return nil, fmt.Errorf("parse GATEWAY_UPSTREAM_URL: invalid URL")
	}
	if (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Hostname() == "" {
		return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: requires HTTP or HTTPS and a host")
	}
	if upstream.User != nil || upstream.RawQuery != "" || upstream.ForceQuery || strings.Contains(raw, "#") {
		return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: credentials, query strings, and fragments are unsupported")
	}
	if port := upstream.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("validate GATEWAY_UPSTREAM_URL: port must be between 1 and 65535")
		}
	}
	return upstream, nil
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
