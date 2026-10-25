package accounting

import "errors"

var (
	// ErrStorageFull means the AOF admission budget or its write health is exhausted.
	ErrStorageFull = errors.New("accounting storage budget exhausted")
	// ErrOwnershipLost fences a request whose gateway lease has expired or been replaced.
	ErrOwnershipLost = errors.New("accounting ownership lost")
	// ErrUnavailable means durable recording or its database delivery is unavailable.
	ErrUnavailable = errors.New("durable accounting unavailable")
	// ErrFull means all reserved accounting slots are occupied.
	ErrFull = errors.New("durable accounting queue full")
	// ErrConflict means a request identity already has different immutable accounting.
	ErrConflict = errors.New("conflicting durable accounting")
	// ErrInvalidConfig means queue bounds or Redis persistence settings are unsafe.
	ErrInvalidConfig = errors.New("invalid durable accounting configuration")
)
