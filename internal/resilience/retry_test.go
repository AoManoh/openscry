package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	errRetryable = errors.New("retryable")
	errFatal     = errors.New("fatal")
)

func retryableOnly(err error) bool { return errors.Is(err, errRetryable) }

// noSleep is a sleep stub that honours ctx cancellation but never blocks,
// so backoff-driven tests run instantly.
func noSleep(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func TestRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	opts := RetryOptions{MaxAttempts: 3, IsRetryable: retryableOnly, sleep: noSleep}
	got, err := Retry(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 42, nil
	})
	if err != nil || got != 42 || calls != 1 {
		t.Fatalf("got=%d err=%v calls=%d", got, err, calls)
	}
}

func TestRetryRetriesThenSucceeds(t *testing.T) {
	calls := 0
	opts := RetryOptions{MaxAttempts: 5, IsRetryable: retryableOnly, sleep: noSleep}
	got, err := Retry(context.Background(), opts, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errRetryable
		}
		return "ok", nil
	})
	if err != nil || got != "ok" || calls != 3 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls)
	}
}

func TestRetryStopsAtMaxAttempts(t *testing.T) {
	calls := 0
	opts := RetryOptions{MaxAttempts: 3, IsRetryable: retryableOnly, sleep: noSleep}
	_, err := Retry(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 0, errRetryable
	})
	if !errors.Is(err, errRetryable) || calls != 3 {
		t.Fatalf("err=%v calls=%d want last=errRetryable calls=3", err, calls)
	}
}

func TestRetryNonRetryableNotRetried(t *testing.T) {
	calls := 0
	opts := RetryOptions{MaxAttempts: 5, IsRetryable: retryableOnly, sleep: noSleep}
	_, err := Retry(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 0, errFatal
	})
	if !errors.Is(err, errFatal) || calls != 1 {
		t.Fatalf("err=%v calls=%d want last=errFatal calls=1", err, calls)
	}
}

func TestRetryNilClassifierRunsOnce(t *testing.T) {
	calls := 0
	opts := RetryOptions{MaxAttempts: 5, sleep: noSleep} // IsRetryable nil = never retry
	_, err := Retry(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 0, errRetryable
	})
	if !errors.Is(err, errRetryable) || calls != 1 {
		t.Fatalf("err=%v calls=%d want calls=1 (nil classifier)", err, calls)
	}
}

func TestRetryRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	opts := RetryOptions{MaxAttempts: 5, IsRetryable: retryableOnly, sleep: noSleep}
	_, err := Retry(ctx, opts, func(context.Context) (int, error) {
		calls++
		cancel() // cancel during the first attempt
		return 0, errRetryable
	})
	if calls != 1 {
		t.Fatalf("calls=%d want 1 (ctx cancel must stop retries)", calls)
	}
	if !errors.Is(err, errRetryable) {
		t.Fatalf("err=%v want last op error surfaced (errRetryable)", err)
	}
}

func TestRetryBudgetCapsRetries(t *testing.T) {
	fixed := time.Unix(100, 0)
	budget := NewRetryBudget(1, 1) // 1 token; clock is fixed so no refill
	calls := 0
	opts := RetryOptions{
		MaxAttempts: 5,
		IsRetryable: retryableOnly,
		Budget:      budget,
		now:         func() time.Time { return fixed },
		sleep:       noSleep,
	}
	_, err := Retry(context.Background(), opts, func(context.Context) (int, error) {
		calls++
		return 0, errRetryable
	})
	// attempt1 fails -> take token (ok) -> attempt2 fails -> take token
	// (denied) -> stop. So exactly 2 calls despite MaxAttempts=5.
	if calls != 2 {
		t.Fatalf("calls=%d want 2 (budget caps retries to 1)", calls)
	}
	if !errors.Is(err, errRetryable) {
		t.Fatalf("err=%v want errRetryable", err)
	}
}

func TestRetryBudgetRefillsOverTime(t *testing.T) {
	budget := NewRetryBudget(2, 10) // burst 2, refill 10/s
	t0 := time.Unix(100, 0)
	if !budget.tryTake(t0) {
		t.Fatal("expected first initial token")
	}
	if !budget.tryTake(t0) {
		t.Fatal("expected second initial token")
	}
	if budget.tryTake(t0) {
		t.Fatal("expected empty bucket after draining burst")
	}
	t1 := t0.Add(500 * time.Millisecond) // +5 tokens, capped at burst 2
	if got := budget.Tokens(t1); got < 1.9 {
		t.Fatalf("refilled tokens=%v want ~2", got)
	}
	if !budget.tryTake(t1) {
		t.Fatal("expected a token to be available after refill")
	}
}
