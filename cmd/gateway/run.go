package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/gateway"
	"github.com/therealshek/llmgate/internal/proxy"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("load gateway config: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var upstream http.Handler
	if cfg.upstreamURL != nil {
		upstream = proxy.NewHandler(cfg.upstreamURL, transport, cfg.upstreamTimeout)
	}

	// Bound request reads and idle connections without limiting response streams.
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           gateway.NewHandlerWithChat(upstream, cfg.maxBodyBytes),
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
