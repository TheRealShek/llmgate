package proxy

import "context"

type attemptKey struct{}

// Attempt holds a proxy failure until the chat handler decides whether to retry.
// The request goroutine owns this state; inspect it only after proxying returns.
type Attempt struct {
	retryResponses bool
	status         int
	retryable      bool
}

// NewAttempt captures transport failures and retryable upstream statuses instead
// of immediately writing their error responses. The caller must deliver a final
// error when it cannot retry. retryResponses enables capture of 502/503/504
// responses when another attempt is allowed. Model listing installs no capture.
func NewAttempt(ctx context.Context, retryResponses bool) (context.Context, *Attempt) {
	attempt := &Attempt{retryResponses: retryResponses}
	return context.WithValue(ctx, attemptKey{}, attempt), attempt
}

// Failure reports the captured HTTP error status and whether it permits a retry.
// Status zero means no pre-response failure was captured.
func (a *Attempt) Failure() (int, bool) {
	return a.status, a.retryable
}
