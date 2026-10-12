package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/backend"
	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
	"github.com/therealshek/llmgate/internal/requestlog"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("load gateway config: %w", err)
	}

	// DefaultTransport is declared as the http.RoundTripper interface.
	// Assert its underlying type is *http.Transport and clone it so custom
	// timeouts and idle connection cleanup do not mutate the process-wide transport.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: cfg.upstreamConnectTimeout}).DialContext
	transport.TLSHandshakeTimeout = cfg.upstreamConnectTimeout

	transport.ResponseHeaderTimeout = cfg.upstreamHeaderTimeout
	defer transport.CloseIdleConnections()

	var (
		nonStreaming http.Handler
		streaming    http.Handler
		readiness    http.Handler
	)

	if cfg.upstreamURL != nil {
		nonStreaming = proxy.NewNonStreamingHandler(cfg.upstreamURL, transport, cfg.upstreamTimeout)
		streaming = proxy.NewStreamingHandler(cfg.upstreamURL, transport, cfg.streamIdleTimeout)
		readiness = backend.NewReadinessHandler(cfg.upstreamURL, transport, cfg.readinessTimeout)
	}

	routes := gateway.NewHandler(nonStreaming, streaming, readiness, gateway.ChatLimits{
		MaxBodyBytes: cfg.maxBodyBytes,
		MaxTokens:    cfg.maxTokens,
	})
	loggedRoutes := requestlog.NewHandler(routes, slog.Default())

	// Bound request reads and idle connections without limiting response streams.
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           loggedRoutes,
		ReadTimeout:       cfg.readTimeout,
		ReadHeaderTimeout: cfg.readHeaderTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen gateway HTTP on %s: %w", server.Addr, err)
	}

	if err := runServer(ctx, server, listener, cfg.shutdownTimeout); err != nil {
		return fmt.Errorf("run gateway HTTP on %s: %w", server.Addr, err)
	}

	return nil
}
