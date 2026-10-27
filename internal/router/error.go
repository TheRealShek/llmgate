package router

import "errors"

// ErrNoBackend means no configured backend is currently healthy.
var ErrNoBackend = errors.New("no healthy backend")
