package resilience

import (
	"context"
	"math"
	"math/rand"
	"sync"
	"time"
)

// RetryOptions configures bounded retry with exponential backoff.
//
// The zero value performs a single attempt with no retries (because
// IsRetryable defaults to nil = "never retry"); set IsRetryable to enable
// retrying and tune the rest as needed.
type RetryOptions struct {
	// MaxAttempts is the total number of attempts including the first one
	// (so MaxAttempts=3 means 1 try + up to 2 retries). Values < 1 are
	// normalized to 3.
	MaxAttempts int
	// BaseDelay is the first backoff delay; subsequent delays grow by
	// Multiplier. Defaults to 500ms.
	BaseDelay time.Duration
	// MaxDelay caps any single backoff delay. Defaults to 10s.
	MaxDelay time.Duration
	// Multiplier is the exponential backoff base. Defaults to 2.0.
	Multiplier float64
	// Jitter, when true (recommended), applies +/-25% randomness to each
	// delay to avoid synchronized retry storms across callers.
	Jitter bool
	// IsRetryable decides whether an error warrants a retry. A nil
	// classifier means "never retry" — the operation runs exactly once.
	IsRetryable func(error) bool
	// Budget, when non-nil, gates every retry (the 2nd+ attempt). When the
	// budget is exhausted, retries stop even if attempts remain. This is
	// the amplification control that prevents batch x retry fan-out from
	// turning a transient upstream blip into a retry storm.
	Budget *RetryBudget
	// OnRetry is an optional observability hook fired before each backoff
	// sleep.
	OnRetry func(attempt int, err error, delay time.Duration)

	// now / sleep are injectable for deterministic tests. nil uses the
	// wall clock / a real ctx-aware sleep.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func (o RetryOptions) normalized() RetryOptions {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 3
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = 500 * time.Millisecond
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 10 * time.Second
	}
	if o.Multiplier < 1 {
		o.Multiplier = 2.0
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	return o
}

// Retry runs op with bounded exponential-backoff retry. The result type is
// generic so typed payloads survive without an interface{} round-trip.
//
// Termination:
//   - first success returns immediately;
//   - a non-retryable error (per IsRetryable) returns immediately;
//   - exhausting MaxAttempts returns the last error;
//   - an exhausted retry Budget stops further retries and returns the last
//     error;
//   - ctx cancellation during an attempt or backoff returns the last
//     operation error if one exists, otherwise ctx.Err().
func Retry[T any](ctx context.Context, opts RetryOptions, op func(context.Context) (T, error)) (T, error) {
	opts = opts.normalized()
	var zero T
	var lastErr error

	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, err
		}

		val, err := op(ctx)
		if err == nil {
			return val, nil
		}
		lastErr = err

		if attempt == opts.MaxAttempts {
			break
		}
		if opts.IsRetryable == nil || !opts.IsRetryable(err) {
			break
		}
		if opts.Budget != nil && !opts.Budget.tryTake(opts.now()) {
			break
		}

		delay := backoffDelay(attempt, opts)
		if opts.OnRetry != nil {
			opts.OnRetry(attempt, err, delay)
		}
		if serr := opts.sleep(ctx, delay); serr != nil {
			// ctx cancelled mid-backoff: surface the last operation error
			// rather than the ctx error so the caller sees why we were
			// retrying in the first place.
			return zero, lastErr
		}
	}
	return zero, lastErr
}

// backoffDelay computes the delay before the next retry. attempt is
// 1-based, so the first backoff uses exponent 0 (== BaseDelay).
func backoffDelay(attempt int, opts RetryOptions) time.Duration {
	d := float64(opts.BaseDelay) * math.Pow(opts.Multiplier, float64(attempt-1))
	if opts.Jitter {
		d *= 0.75 + rand.Float64()*0.5 // 0.75..1.25
	}
	if d <= 0 {
		return 0
	}
	if time.Duration(d) > opts.MaxDelay {
		return opts.MaxDelay
	}
	return time.Duration(d)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryBudget is a token-bucket limiter that caps the rate of retries
// across all callers that share it. Each retry attempt consumes one token;
// tokens refill continuously at refillPerSec up to burst. Sharing a single
// budget across a batch of concurrent searches ensures that a widespread
// upstream failure cannot multiply into retries x fan-out load.
//
// A RetryBudget is safe for concurrent use.
type RetryBudget struct {
	mu           sync.Mutex
	tokens       float64
	burst        float64
	refillPerSec float64
	last         time.Time
}

// NewRetryBudget constructs a budget that starts full with burst tokens and
// refills at refillPerSec tokens/second. burst < 1 is clamped to 1 and
// refillPerSec <= 0 is clamped to 1.
func NewRetryBudget(burst int, refillPerSec float64) *RetryBudget {
	if burst < 1 {
		burst = 1
	}
	if refillPerSec <= 0 {
		refillPerSec = 1
	}
	return &RetryBudget{
		tokens:       float64(burst),
		burst:        float64(burst),
		refillPerSec: refillPerSec,
	}
}

// tryTake refills based on elapsed time since the last call and consumes one
// token if available. now is passed in so it can be driven by a fake clock
// from RetryOptions in tests.
func (b *RetryBudget) tryTake(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.refillPerSec)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Tokens returns the current (refilled-as-of now) token count. Intended for
// tests and observability.
func (b *RetryBudget) Tokens(now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last.IsZero() {
		b.last = now
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.refillPerSec)
		b.last = now
	}
	return b.tokens
}
