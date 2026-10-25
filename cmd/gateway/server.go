package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/therealshek/llmgate/internal/api"
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

// requestDrain tracks complete handlers, including their deferred accounting.
// Its mutex makes request registration and closing admission one atomic boundary.
type requestDrain struct {
	mu      sync.Mutex
	active  int
	closing bool
	done    chan struct{}
}

func (d *requestDrain) track(next http.Handler) http.Handler {
	if next == nil {
		next = http.DefaultServeMux
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		if d.closing {
			d.mu.Unlock()

			_ = api.WriteError(w, http.StatusServiceUnavailable, api.Error{
				Message: "server is shutting down", Type: "server_error", Code: "server_shutting_down",
			})

			return
		}

		d.active++
		d.mu.Unlock()

		// Remain registered until logging, quota settlement, and usage writing return,
		// including when the proxy unwinds through an abort panic.
		defer d.finish()

		next.ServeHTTP(w, r)
	})
}

func (d *requestDrain) finish() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.active--
	if d.closing && d.active == 0 {
		close(d.done)
	}
}

func (d *requestDrain) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closing {
		return
	}

	d.closing = true
	if d.active == 0 {
		close(d.done)
	}
}

func (d *requestDrain) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.active
}

// runServers owns listeners, serving workers, and complete request lifetimes.
// The cleanup deadline starts after draining and forced connection cancellation.
func runServers(ctx context.Context, servers []boundServer, shutdownTimeout, cleanupTimeout time.Duration) error {
	serveDone := make(chan error, len(servers))
	requests := &requestDrain{done: make(chan struct{})}

	var workers sync.WaitGroup

	for _, entry := range servers {
		entry.server.Handler = requests.track(entry.server.Handler)

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

	requests.stop()

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

	// Close cancels connections but does not join their handlers. Keep dependencies
	// owned by run available while those handlers finish under this fresh deadline.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cleanupCancel()

	select {
	case <-requests.done:
	case <-cleanupCtx.Done():
		if active := requests.count(); active != 0 {
			result = errors.Join(result, &requestCleanupError{active: active, cause: cleanupCtx.Err()})
		}
	}

	return result
}
