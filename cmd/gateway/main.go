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

// main configures HTTP serving and process signals so termination can drain active
// requests and startup or shutdown failures produce a nonzero exit status.
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	// Bound slow headers and idle connections without limiting long response streams.
	server := &http.Server{
		Addr:              ":8080",
		Handler:           gateway.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Open the port first so a bind failure is reported before entering the serving loop.
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		slog.Error("gateway HTTP listener failed", "address", server.Addr, "error", err)
		os.Exit(1)
	}

	// Turn terminal interrupts and deployment termination into a drain request.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, server, listener, 10*time.Second); err != nil {
		slog.Error("gateway HTTP server failed", "address", server.Addr, "error", err)
		os.Exit(1)
	}
}
