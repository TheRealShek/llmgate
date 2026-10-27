package proxy

// responseStatusError stops ReverseProxy before it forwards a retryable response.
type responseStatusError struct{ status int }

func (e *responseStatusError) Error() string { return "upstream returned a retryable status" }
