package store

import "errors"

var (
	// ErrKeyNotFound means the key is unknown or revoked.
	ErrKeyNotFound = errors.New("API key not found")
	// ErrInvalidKeyOptions means the key name or limits are invalid.
	ErrInvalidKeyOptions = errors.New("invalid API key options")
)
