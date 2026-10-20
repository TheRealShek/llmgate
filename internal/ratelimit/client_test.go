package ratelimit

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestRedisConfiguration checks malformed connection settings fail safely without dialing.
func TestRedisConfiguration(t *testing.T) {
	limits := ClientLimits{MaxConns: 2, ConnectTimeout: time.Second, CommandTimeout: time.Second}

	for _, address := range []string{
		"", "http://localhost:6379", "redis:///0", "redis://user:private-secret%zz@localhost",
		"redis://localhost/invalid", "redis://localhost:bad/0", "redis://localhost/0?max_retries=8",
		"redis://localhost/0#private-secret",
	} {
		t.Run(address, func(t *testing.T) {
			client, err := OpenClient(t.Context(), address, limits)
			if client != nil || !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "private-secret") {
				t.Fatal("invalid Redis settings were accepted or leaked", err)
			}
		})
	}

	for _, invalid := range []ClientLimits{
		{ConnectTimeout: time.Second, CommandTimeout: time.Second},
		{MaxConns: 2, CommandTimeout: time.Second},
		{MaxConns: 2, ConnectTimeout: time.Second},
	} {
		if _, err := OpenClient(t.Context(), "redis://localhost/0", invalid); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("nonpositive resource bound accepted", err)
		}
	}
}

// TestRedisStartupDeadline checks a server that accepts but never responds cannot hang startup.
func TestRedisStartupDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var connections []net.Conn

	done := make(chan struct{})

	t.Cleanup(func() {
		_ = listener.Close()

		<-done

		for _, connection := range connections {
			_ = connection.Close()
		}
	})

	go func() {
		defer close(done)

		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}

			connections = append(connections, connection)
		}
	}()

	started := time.Now()

	client, err := OpenClient(t.Context(), "redis://"+listener.Addr().String()+"/0", ClientLimits{
		MaxConns: 1, ConnectTimeout: 50 * time.Millisecond, CommandTimeout: time.Second,
	})
	if client != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatal("unresponsive Redis did not fail startup", err)
	}

	if time.Since(started) > time.Second {
		t.Fatal("Redis startup exceeded its bounded deadline")
	}

	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("private timeout cause was not preserved", err)
	}
}

// TestRedisClientBounds checks production options disable retries and cap total pool connections.
func TestRedisClientBounds(t *testing.T) {
	client := redisTestClient(t)
	options := client.Options()

	if options.MaxRetries != 0 || !options.ContextTimeoutEnabled ||
		options.MaxActiveConns != 4 || options.PoolSize != 4 || options.PipelinePoolSize != -1 {
		t.Fatal("Redis retries, deadlines, or resource bounds were not configured")
	}
}
