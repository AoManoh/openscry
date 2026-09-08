package search

import (
	"testing"

	"github.com/AoManoh/openscry/internal/grok"
)

// TestOverloadedNotBreakerFailure locks the critical correctness property of
// the upstream-concurrency limiter: a CodeOverloaded (the local "no slot
// before the deadline" signal) must NOT count against the circuit breaker and
// must NOT be retried. Otherwise a local saturation spike would trip the
// breaker and shed load from a perfectly healthy upstream.
func TestOverloadedNotBreakerFailure(t *testing.T) {
	err := &grok.Error{Code: grok.CodeOverloaded, Message: "saturated"}
	if isBreakerFailure(err) {
		t.Fatal("CodeOverloaded must not be a breaker failure (local saturation is not an upstream-health signal)")
	}
	if isRetryable(err) {
		t.Fatal("CodeOverloaded must not be retryable")
	}
}

// TestTimeoutStillBreakerFailure guards against a regression where the new
// CodeOverloaded path might be confused with a genuine upstream timeout: a
// real CodeTimeout must still trip the breaker and be retryable.
func TestTimeoutStillBreakerFailure(t *testing.T) {
	err := &grok.Error{Code: grok.CodeTimeout, Message: "deadline", Retryable: true}
	if !isBreakerFailure(err) {
		t.Fatal("CodeTimeout must remain a breaker failure")
	}
	if !isRetryable(err) {
		t.Fatal("CodeTimeout must remain retryable")
	}
}
