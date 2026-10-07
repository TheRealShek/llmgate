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
	// Let Serve report its result without waiting for the shutdown path to receive it.
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve gateway HTTP: %w", err)
	case <-ctx.Done():
	}

	// The signal context is already canceled; draining needs a fresh deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
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
	return errors.Join(shutdownErr, serveErr)
}
