package provider

import (
	"strings"
	"time"
)

// RetryOptions configures the retry behavior.
type RetryOptions struct {
	MaxAttempts int
	BackoffMs   []int
	ShouldRetry func(error) bool
}

var defaultBackoff = []int{1000, 2000, 4000}

// isTransientError checks if an error is transient (timeout, connection reset, rate limit).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "econnreset") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "429") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "502")
}

// WithRetry executes fn with exponential backoff retry on transient errors.
func WithRetry[T any](fn func() (T, error), opts *RetryOptions) (T, error) {
	maxAttempts := 3
	backoff := defaultBackoff
	shouldRetry := isTransientError

	if opts != nil {
		if opts.MaxAttempts > 0 {
			maxAttempts = opts.MaxAttempts
		}
		if len(opts.BackoffMs) > 0 {
			backoff = opts.BackoffMs
		}
		if opts.ShouldRetry != nil {
			shouldRetry = opts.ShouldRetry
		}
	}

	var lastErr error
	var zero T
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt >= maxAttempts || !shouldRetry(err) {
			return zero, err
		}
		idx := attempt - 1
		if idx >= len(backoff) {
			idx = len(backoff) - 1
		}
		time.Sleep(time.Duration(backoff[idx]) * time.Millisecond)
	}
	return zero, lastErr
}
