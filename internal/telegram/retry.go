package telegram

import (
	"errors"
	"fmt"
	"time"
)

type APIError struct {
	StatusCode int
	RetryAfter time.Duration
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api: status=%d body=%s", e.StatusCode, e.Body)
}

func (e *APIError) Status() int { return e.StatusCode }

func IsRetryable(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == 429:
			return true, "429"
		case ae.StatusCode >= 500:
			return true, "5xx"
		default:
			return false, "4xx"
		}
	}
	return true, "network"
}

func backoff(initial, maxBackoff time.Duration, attempt int) time.Duration {
	d := initial << attempt
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}
