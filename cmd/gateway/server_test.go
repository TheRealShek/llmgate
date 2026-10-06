package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestRunServerShutdown checks that shutdown stops new connections, preserves
// requests that finish in time, and cancels those that exceed the deadline.
func TestRunServerShutdown(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		drain   bool
	}{
		{name: "active request finishes", timeout: 5 * time.Second, drain: true},
		{name: "deadline closes active request", timeout: 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Port zero asks the OS for a free port, keeping tests independent of port 8080.
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			started := make(chan struct{})
			release := make(chan struct{})
			canceled := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				// Hold a real request open until the test releases it or Close cancels it.
				select {
				case <-release:
					_, _ = w.Write([]byte("finished\n"))
				case <-r.Context().Done():
					close(canceled)
				}
			})}
			t.Cleanup(func() { _ = server.Close() })
			shutdownStarted := make(chan struct{})
			server.RegisterOnShutdown(func() { close(shutdownStarted) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			serverDone := make(chan error, 1)
			go func() { serverDone <- runServer(ctx, server, listener, tt.timeout) }()

			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
			defer client.CloseIdleConnections()
			type result struct {
				body string
				err  error
			}
			clientDone := make(chan result, 1)
			go func() {
				response, err := client.Get("http://" + listener.Addr().String())
				if err != nil {
					clientDone <- result{err: err}
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				clientDone <- result{body: string(body), err: err}
			}()
			// Begin shutdown only after a request is active, rather than racing its arrival.
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach the handler")
			}
			cancel()
			select {
			case <-shutdownStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown did not start")
			}
			connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err == nil {
				_ = connection.Close()
				t.Fatal("listener accepted a connection during shutdown")
			}
			if tt.drain {
				// Give an incorrect early return time to surface before letting the request finish.
				select {
				case err := <-serverDone:
					t.Fatalf("server returned before the active request finished: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				close(release)
			}
			select {
			case err := <-serverDone:
				if tt.drain && err != nil {
					t.Fatalf("shutdown failed: %v", err)
				}
				if !tt.drain && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("shutdown error = %v, want context.DeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not return")
			}
			select {
			case got := <-clientDone:
				if tt.drain && (got.err != nil || got.body != "finished\n") {
					t.Fatalf("response = %q, error = %v", got.body, got.err)
				}
				if !tt.drain && got.err == nil {
					t.Fatal("request succeeded after the shutdown deadline")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("client did not return")
			}
			if !tt.drain {
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("closing the connection did not cancel the request context")
				}
			}
		})
	}
}

// TestRunServerListenerFailure checks that serving errors retain their cause so
// callers can distinguish listener failures from normal shutdown.
func TestRunServerListenerFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A closed listener makes Serve fail without relying on a port collision.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = runServer(t.Context(), &http.Server{}, listener, time.Second)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("serve error = %v, want net.ErrClosed", err)
	}
}
