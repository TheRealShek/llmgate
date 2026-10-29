package router

import "errors"

// ErrNoBackend means no configured backend is eligible under health and circuit state.
var ErrNoBackend = errors.New("no healthy backend")
