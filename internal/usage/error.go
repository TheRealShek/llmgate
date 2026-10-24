package usage

import "errors"

// ErrUnavailable means inference usage cannot be observed and recorded safely.
var ErrUnavailable = errors.New("usage recording unavailable")
