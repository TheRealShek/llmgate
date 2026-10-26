package queue

import "errors"

var (
	// ErrUnavailable identifies missing queue configuration.
	ErrUnavailable = errors.New("inference admission unavailable")
	// ErrFull means all active and waiting capacity is occupied.
	ErrFull = errors.New("inference queue full")
	// ErrTimeout means a request exhausted its queue wait deadline.
	ErrTimeout = errors.New("inference queue wait expired")
)
