package store

import "errors"

var (
	// ErrInvalidDatabaseConfig hides configuration details that may contain credentials.
	ErrInvalidDatabaseConfig = errors.New("invalid database configuration")
	// ErrDatabaseUnavailable hides connection diagnostics that may contain credentials.
	ErrDatabaseUnavailable = errors.New("database unavailable")
	// ErrKeyNotFound means the key is unknown or revoked.
	ErrKeyNotFound = errors.New("API key not found")
	// ErrInvalidKeyOptions means the key name or limits are invalid.
	ErrInvalidKeyOptions = errors.New("invalid API key options")
	// ErrInvalidUsage means a usage record violates its identity, timing, or count bounds.
	ErrInvalidUsage = errors.New("invalid usage record")
	// ErrInvalidAdmission means pre-inference identity or reservation data is invalid.
	ErrInvalidAdmission = errors.New("invalid inference admission")
	// ErrUsageConflict means a request ID already belongs to a different usage record.
	ErrUsageConflict = errors.New("conflicting usage record")
)

// databaseError preserves a private driver cause while exposing only a safe category.
type databaseError struct {
	category error
	cause    error
}

func (e *databaseError) Error() string        { return e.category.Error() }
func (e *databaseError) Unwrap() error        { return e.cause }
func (e *databaseError) Is(target error) bool { return target == e.category }
