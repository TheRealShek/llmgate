package ratelimit

import "errors"

var (
	// ErrInvalidConfig identifies invalid Redis connection settings or resource bounds.
	ErrInvalidConfig = errors.New("invalid Redis configuration")
	// ErrUnavailable identifies failed Redis admission or startup operations.
	ErrUnavailable = errors.New("request limiting unavailable")
	// ErrInvalidLimit identifies unsupported key IDs or request rates.
	ErrInvalidLimit = errors.New("invalid request limit")
	// ErrInvalidResult identifies a malformed Redis admission decision.
	ErrInvalidResult = errors.New("invalid request limiter response")
)

// redisError preserves private connection diagnostics without exposing credentials.
type redisError struct {
	category error
	cause    error
}

func (e *redisError) Error() string        { return e.category.Error() }
func (e *redisError) Unwrap() error        { return e.cause }
func (e *redisError) Is(target error) bool { return target == e.category }
