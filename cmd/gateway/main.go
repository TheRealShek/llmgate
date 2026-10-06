package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/therealshek/llmgate/internal/gateway"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	server := &http.Server{
		Addr:              ":8080",
		Handler:           gateway.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		slog.Error("gateway HTTP listener failed", "address", server.Addr, "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, server, listener, 10*time.Second); err != nil {
		slog.Error("gateway HTTP server failed", "address", server.Addr, "error", err)
		os.Exit(1)
	}
}
