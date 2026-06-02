package grok

import (
	"errors"
	"fmt"
	"time"
)

// Code classifies an upstream failure so callers (and ultimately the AI
// client) can see exactly why a search did not succeed. Per the openscry
// design principle, failures are always surfaced explicitly and are never
// masked by silently switching to a different model.
type Code string

const (
	// CodeModelUnavailable indicates the requested model/request was
	// rejected (e.g. HTTP 400/404). It is NOT retryable and MUST NOT
	// trigger an automatic switch to another model.
	CodeModelUnavailable Code = "model_unavailable"
	// CodeUpstreamStatus is a non-success HTTP status not otherwise
	// classified (auth, 5xx, unexpected).
	CodeUpstreamStatus Code = "upstream_status"
	// CodeTimeout indicates the request exceeded its deadline.
	CodeTimeout Code = "timeout"
	// CodeConnect indicates a connection-level failure.
	CodeConnect Code = "connect"
	// CodeCanceled indicates the caller/client cancelled the request.
	CodeCanceled Code = "canceled"
	// CodeRateLimit indicates upstream throttling (HTTP 429).
	CodeRateLimit Code = "rate_limit"
	// CodeEmpty indicates a successful transport but empty content — a
	// failure class that must remain visible, not silently treated as OK.
	CodeEmpty Code = "empty"
	// CodeDecode indicates a request/response (de)serialization failure.
	CodeDecode Code = "decode"
	// CodeOverloaded indicates the local upstream-concurrency limiter could
	// not grant a slot before the deadline elapsed. It is a LOCAL condition,
	// not an upstream-health signal, so it deliberately maps to neither a
	// retry nor a breaker failure (it must not trip the circuit breaker the
	// way CodeTimeout/CodeConnect do). The request was bounded on purpose;
	// the caller should back off (HTTP transport surfaces it as 503).
	CodeOverloaded Code = "overloaded"
)

// Error is the structured error returned by the grok client.
type Error struct {
	Code      Code
	Message   string
	Status    int  // HTTP status when applicable; 0 otherwise
	Retryable bool // hint for the resilience layer (retry/breaker classification)
	// RetryAfter carries the upstream's Retry-After hint (parsed from the
	// response header) on a 429/503. Zero means "no hint"; the resilience
	// layer honors it as the backoff delay instead of blind exponential
	// backoff so we do not retry while still throttled.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("grok[%s] (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("grok[%s]: %s", e.Code, e.Message)
}

// AsError extracts a *Error from err, unwrapping wrapped errors (errors.As)
// so classification stays correct even if a caller wraps the error with %w.
func AsError(err error) (*Error, bool) {
	var ge *Error
	if errors.As(err, &ge) {
		return ge, true
	}
	return nil, false
}
