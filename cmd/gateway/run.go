package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/therealshek/llmgate/internal/gateway"
)

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return fmt.Errorf("load gateway config: %w", err)
	}

	// Bound slow headers and idle connections without limiting long response streams.
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           gateway.NewHandler(),
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
