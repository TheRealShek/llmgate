package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// namedServer pairs an HTTP server with a label for logs and error messages.
type namedServer struct {
	name   string
	server *http.Server
}

// boundServer pairs a server with its open network listener. It passes claimed
// ports from listenServers to runServers so all ports bind before serving starts.
type boundServer struct {
	name     string
	server   *http.Server
	listener net.Listener
}

// listenServers binds every enabled server before any can accept requests.
// Startup transfers successful listeners to runServers; a partial bind closes them here.
func listenServers(servers []namedServer) ([]boundServer, error) {
	bound := make([]boundServer, 0, len(servers))
	for _, entry := range servers {
		listener, err := net.Listen("tcp", entry.server.Addr)
		if err != nil {
			result := fmt.Errorf("listen %s HTTP: %w", entry.name, err)
			for _, opened := range bound {
				if err := opened.listener.Close(); err != nil {
					result = errors.Join(result, fmt.Errorf("close %s listener: %w", opened.name, err))
				}
			}

			return nil, result
		}

		bound = append(bound, boundServer{name: entry.name, server: entry.server, listener: listener})
	}

	return bound, nil
}

// runServers owns bound listeners and joins all serving and shutdown goroutines.
// Cancellation or an unexpected server exit drains every server under one deadline.
func runServers(ctx context.Context, servers []boundServer, shutdownTimeout time.Duration) error {
	serveDone := make(chan error, len(servers))

	var workers sync.WaitGroup
	for _, entry := range servers {
		workers.Go(func() {
			err := entry.server.Serve(entry.listener)
			serveDone <- fmt.Errorf("serve %s HTTP: %w", entry.name, err)
		})
		fmt.Printf("Server listening on %s (%s)\n", entry.listener.Addr(), entry.name)
	}

	var result error

	consumed := 0

	select {
	case err := <-serveDone:
		consumed++

		if ctx.Err() == nil || !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	case <-ctx.Done():
	}

	// The signal context may already be canceled. One fresh context bounds all
	// drains together; concurrent Shutdown calls stop both listeners promptly.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	shutdownDone := make(chan error, len(servers))
	for _, entry := range servers {
		workers.Go(func() {
			err := entry.server.Shutdown(shutdownCtx)
			if err != nil {
				err = fmt.Errorf("shutdown %s HTTP: %w", entry.name, err)
				if closeErr := entry.server.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("close %s HTTP: %w", entry.name, closeErr))
				}
			}

			shutdownDone <- err
		})
	}

	for range servers {
		result = errors.Join(result, <-shutdownDone)
	}

	for consumed < len(servers) {
		err := <-serveDone
		consumed++

		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, err)
		}
	}

	workers.Wait()

	return result
}
