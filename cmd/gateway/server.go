package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// runServer serves HTTP and, on cancellation, drains requests up to shutdownTimeout.
// It waits for shutdown and closes overdue connections so process exit has a bounded wait.
func runServer(ctx context.Context, server *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	// Serve blocks, so run it separately while this goroutine watches for cancellation.
	// One buffer slot lets it report its result even while we are waiting for shutdown.
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	// A serving failure ends the wait too; otherwise a dead server could wait for a signal.
	select {
	case err := <-serveDone:
		// Closing an HTTP server returns this sentinel even when no failure occurred.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve gateway HTTP: %w", err)
	case <-ctx.Done():
	}

	// The signal context is already canceled; draining needs a fresh deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shutdown stops new connections but lets active handlers finish before returning.
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = fmt.Errorf("shutdown gateway HTTP: %w", shutdownErr)
		// Shutdown does not close active connections when its deadline expires; Close does.
		if err := server.Close(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close gateway HTTP: %w", err))
		}
	}
	// Serve returns when the listener closes, before draining finishes. Wait for both
	// calls before returning so main cannot exit with a serving goroutine still running.
	serveErr := <-serveDone
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	} else if serveErr != nil {
		serveErr = fmt.Errorf("serve gateway HTTP: %w", serveErr)
	}
	// Keep both failures, if present, and preserve their causes for errors.Is checks.
	return errors.Join(shutdownErr, serveErr)
}
