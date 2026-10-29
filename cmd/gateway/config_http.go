package main

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// serverConfig owns the public address, shared HTTP limits, and shutdown budgets.
type serverConfig struct {
	address                string
	readHeaderTimeout      time.Duration
	readTimeout            time.Duration // Request-body reading, not response generation.
	idleTimeout            time.Duration // Between client keep-alive requests.
	shutdownTimeout        time.Duration
	shutdownCleanupTimeout time.Duration // Accounting after forced cancellation.
}

// chatConfig bounds one client request across its backend attempts.
type chatConfig struct {
	maxRequestBodyBytes int64
	maxOutputTokens     int64 // Ceiling and default when omitted.
	maxBackendAttempts  int
}

// adminConfig applies only to the optional loopback operator API.
type adminConfig struct {
	enabled             bool
	address             string
	token               string
	maxRequestBodyBytes int64
	createTimeout       time.Duration
	revokeTimeout       time.Duration
}

func loadServerConfig(getenv func(string) string) (serverConfig, error) {
	cfg := serverConfig{
		address:                ":8080",
		readHeaderTimeout:      5 * time.Second,
		readTimeout:            10 * time.Second,
		idleTimeout:            60 * time.Second,
		shutdownTimeout:        10 * time.Second,
		shutdownCleanupTimeout: 3 * time.Second,
	}

	var err error

	if val := getenv("GATEWAY_ADDR"); val != "" {
		cfg.address = val
	}

	cfg.readTimeout, err = envDuration(getenv, "GATEWAY_READ_TIMEOUT", cfg.readTimeout)

	if err != nil {
		return serverConfig{}, err
	}

	cfg.readHeaderTimeout, err = envDuration(
		getenv,
		"GATEWAY_READ_HEADER_TIMEOUT",
		cfg.readHeaderTimeout,
	)

	if err != nil {
		return serverConfig{}, err
	}

	cfg.idleTimeout, err = envDuration(getenv, "GATEWAY_IDLE_TIMEOUT", cfg.idleTimeout)

	if err != nil {
		return serverConfig{}, err
	}

	cfg.shutdownTimeout, err = envDuration(getenv, "GATEWAY_SHUTDOWN_TIMEOUT", cfg.shutdownTimeout)

	if err != nil {
		return serverConfig{}, err
	}

	cfg.shutdownCleanupTimeout, err = envDuration(getenv, "GATEWAY_SHUTDOWN_CLEANUP_TIMEOUT", cfg.shutdownCleanupTimeout)

	if err != nil {
		return serverConfig{}, err
	}

	return cfg, nil
}

func loadChatConfig(getenv func(string) string) (chatConfig, error) {
	cfg := chatConfig{
		maxRequestBodyBytes: 1 << 20,
		maxOutputTokens:     1024,
		maxBackendAttempts:  2,
	}

	var err error

	if value := getenv("GATEWAY_MAX_ATTEMPTS"); value != "" {
		cfg.maxBackendAttempts, err = strconv.Atoi(value)
		if err != nil || cfg.maxBackendAttempts < 1 || cfg.maxBackendAttempts > 2 {
			return chatConfig{}, fmt.Errorf("validate GATEWAY_MAX_ATTEMPTS: requires 1 or 2")
		}
	}

	if val := getenv("GATEWAY_MAX_TOKENS"); val != "" {
		cfg.maxOutputTokens, err = strconv.ParseInt(val, 10, 64)
		if err != nil || cfg.maxOutputTokens <= 0 {
			return chatConfig{}, fmt.Errorf("validate GATEWAY_MAX_TOKENS: requires a positive integer")
		}
	}

	if val := getenv("GATEWAY_MAX_BODY_BYTES"); val != "" {
		cfg.maxRequestBodyBytes, err = strconv.ParseInt(val, 10, 64)
		if err != nil || cfg.maxRequestBodyBytes <= 0 {
			return chatConfig{}, fmt.Errorf("validate GATEWAY_MAX_BODY_BYTES: requires a positive integer")
		}
	}

	return cfg, nil
}

func loadAdminConfig(getenv func(string) string) (adminConfig, error) {
	cfg := adminConfig{
		address:             "127.0.0.1:8082",
		maxRequestBodyBytes: 4096,
		createTimeout:       2 * time.Second,
		revokeTimeout:       2 * time.Second,
	}

	var err error

	if val := getenv("GATEWAY_ADMIN_ENABLED"); val != "" {
		enabled, err := strconv.ParseBool(val)
		if err != nil {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ENABLED: requires a boolean: %w",
				&configError{cause: err})
		}

		cfg.enabled = enabled
	}

	if val := getenv("GATEWAY_ADMIN_ADDR"); val != "" {
		cfg.address = val
	}

	if cfg.enabled {
		cfg.token = getenv("GATEWAY_ADMIN_TOKEN")
		if len(cfg.token) < 32 || len(cfg.token) > 256 {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_TOKEN: requires 32 to 256 URL-safe ASCII characters")
		}

		for _, char := range cfg.token {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '_' || char == '-' {
				continue
			}

			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_TOKEN: requires 32 to 256 URL-safe ASCII characters")
		}

		host, port, err := net.SplitHostPort(cfg.address)
		if err != nil {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a loopback IP and numeric port: %w",
				&configError{cause: err})
		}

		ip, err := netip.ParseAddr(host)
		if err != nil {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a literal loopback IP: %w",
				&configError{cause: err})
		}

		if !ip.IsLoopback() || ip.Zone() != "" {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a literal loopback IP")
		}

		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a numeric port: %w",
				&configError{cause: err})
		}

		if number == 0 {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_ADDR: requires a port from 1 to 65535")
		}
	}

	cfg.createTimeout, err = envDuration(getenv, "GATEWAY_ADMIN_CREATE_TIMEOUT", cfg.createTimeout)

	if err != nil {
		return adminConfig{}, err
	}

	cfg.revokeTimeout, err = envDuration(getenv, "GATEWAY_ADMIN_REVOKE_TIMEOUT", cfg.revokeTimeout)

	if err != nil {
		return adminConfig{}, err
	}

	if val := getenv("GATEWAY_ADMIN_MAX_BODY_BYTES"); val != "" {
		cfg.maxRequestBodyBytes, err = strconv.ParseInt(val, 10, 64)
		if err != nil {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_MAX_BODY_BYTES: requires a positive integer: %w",
				&configError{cause: err})
		}

		if cfg.maxRequestBodyBytes <= 0 {
			return adminConfig{}, fmt.Errorf("validate GATEWAY_ADMIN_MAX_BODY_BYTES: requires a positive integer")
		}
	}

	return cfg, nil
}
