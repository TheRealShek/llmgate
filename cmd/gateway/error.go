package main

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
