package main

import "fmt"

// requestCleanupError reports handlers that outlived the bounded cleanup window.
type requestCleanupError struct {
	active int
	cause  error
}

func (e *requestCleanupError) Error() string {
	return fmt.Sprintf("request cleanup timed out with %d active handlers", e.active)
}

func (e *requestCleanupError) Unwrap() error { return e.cause }

// keyCreateError retains a command failure cause without printing private diagnostics.
type keyCreateError struct {
	cause error
}

func (e *keyCreateError) Error() string {
	return "key creation failed"
}

func (e *keyCreateError) Unwrap() error {
	return e.cause
}

// configError preserves a parsing cause without exposing supplied configuration.
type configError struct {
	cause error
}

func (e *configError) Error() string { return "invalid configuration" }
func (e *configError) Unwrap() error { return e.cause }
