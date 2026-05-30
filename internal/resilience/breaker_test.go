package resilience

import (
	"errors"
	"testing"
	"time"
)

// newTestBreaker wires a breaker to a caller-controlled clock so cooldown
// transitions are deterministic.
func newTestBreaker(cfg BreakerConfig, clock *time.Time) *Breaker {
	b := NewBreaker(cfg)
	b.now = func() time.Time { return *clock }
	return b
}

func TestBreakerOpensAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 3, OpenDuration: time.Minute}, &now)
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("call %d: expected allow while closed", i)
		}
		b.OnResult(true)
	}
	if b.State() != StateOpen {
		t.Fatalf("state=%s want open", b.State())
	}
	if b.Allow() {
		t.Fatal("expected reject while open")
	}
}

func TestBreakerSuccessResetsConsecutiveFailures(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 3, OpenDuration: time.Minute}, &now)
	b.OnResult(true)
	b.OnResult(true)
	b.OnResult(false) // reset to 0
	b.OnResult(true)
	b.OnResult(true) // only 2 consecutive -> still closed
	if b.State() != StateClosed {
		t.Fatalf("state=%s want closed (success should have reset the counter)", b.State())
	}
}

func TestBreakerHalfOpenAfterCooldown(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 1, OpenDuration: 30 * time.Second, HalfOpenMaxProbes: 1}, &now)
	b.OnResult(true) // threshold 1 -> trip
	if b.State() != StateOpen {
		t.Fatalf("state=%s want open", b.State())
	}
	now = now.Add(31 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatalf("state=%s want half-open after cooldown", b.State())
	}
	if !b.Allow() {
		t.Fatal("expected first probe admitted in half-open")
	}
	if b.Allow() {
		t.Fatal("expected second probe rejected (HalfOpenMaxProbes=1)")
	}
}

func TestBreakerHalfOpenSuccessCloses(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 1, OpenDuration: time.Second, HalfOpenMaxProbes: 1, SuccessesToClose: 1}, &now)
	b.OnResult(true) // open
	now = now.Add(2 * time.Second)
	if !b.Allow() {
		t.Fatal("expected probe admitted")
	}
	b.OnResult(false) // probe succeeds
	if b.State() != StateClosed {
		t.Fatalf("state=%s want closed after successful probe", b.State())
	}
}

func TestBreakerHalfOpenFailureReopens(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 1, OpenDuration: time.Second, HalfOpenMaxProbes: 1}, &now)
	b.OnResult(true) // open
	now = now.Add(2 * time.Second)
	if !b.Allow() {
		t.Fatal("expected probe admitted")
	}
	b.OnResult(true) // probe fails -> reopen
	if b.State() != StateOpen {
		t.Fatalf("state=%s want open after probe failure", b.State())
	}
}

func TestBreakerGuardReturnsCircuitOpen(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 1, OpenDuration: time.Minute}, &now)
	transportFail := func(error) bool { return true }
	_ = b.Guard(func() error { return errors.New("boom") }, transportFail) // trips open
	err := b.Guard(func() error { return nil }, transportFail)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err=%v want ErrCircuitOpen", err)
	}
}

func TestBreakerGuardNonTransportErrorDoesNotTrip(t *testing.T) {
	now := time.Unix(0, 0)
	b := newTestBreaker(BreakerConfig{FailureThreshold: 2, OpenDuration: time.Minute}, &now)
	modelErr := errors.New("model_unavailable")
	notTransport := func(error) bool { return false } // model errors are never upstream-health signals
	for i := 0; i < 10; i++ {
		if err := b.Guard(func() error { return modelErr }, notTransport); !errors.Is(err, modelErr) {
			t.Fatalf("call %d: err=%v want modelErr passthrough", i, err)
		}
	}
	if b.State() != StateClosed {
		t.Fatalf("state=%s want closed (non-transport errors must never trip)", b.State())
	}
}
