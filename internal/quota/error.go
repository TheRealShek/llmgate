package quota

import (
	"errors"
	"time"
)

var (
	// ErrUnavailable identifies a failed quota dependency or unsupported accounting state.
	ErrUnavailable = errors.New("token quota unavailable")
	// ErrInvalidRequest identifies request options incompatible with exact token accounting.
	ErrInvalidRequest = errors.New("unsupported quota request")
	// ErrInvalidLimit identifies limits outside the exact Redis integer range.
	ErrInvalidLimit = errors.New("invalid token quota")
)

// Exhausted reports the wait until the next UTC budget, not guaranteed future admission.
type Exhausted struct{ RetryAfter time.Duration }

func (e *Exhausted) Error() string { return "daily token quota exhausted" }
