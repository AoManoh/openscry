package resilience

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen is returned by Breaker.Guard when the breaker is open and
// fast-failing instead of forwarding the call upstream.
var ErrCircuitOpen = errors.New("resilience: circuit breaker open")

// BreakerState is the circuit breaker's current state.
type BreakerState int

const (
	// StateClosed forwards all calls; failures are counted.
	StateClosed BreakerState = iota
	// StateOpen fast-fails all calls until the cooldown elapses.
	StateOpen
	// StateHalfOpen admits a limited number of probe calls to test
	// whether the upstream has recovered.
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// BreakerConfig tunes a Breaker. Zero values fall back to sensible defaults.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive transport-class
	// failures that trips the breaker from closed to open. Default 5.
	FailureThreshold int
	// OpenDuration is the cooldown before an open breaker admits a
	// half-open probe. Default 30s.
	OpenDuration time.Duration
	// HalfOpenMaxProbes caps concurrent probe calls in half-open. Default 1.
	HalfOpenMaxProbes int
	// SuccessesToClose is the number of half-open successes required to
	// fully close the breaker. Default 1.
	SuccessesToClose int
}

func (c BreakerConfig) normalized() BreakerConfig {
	if c.FailureThreshold < 1 {
		c.FailureThreshold = 5
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 30 * time.Second
	}
	if c.HalfOpenMaxProbes < 1 {
		c.HalfOpenMaxProbes = 1
	}
	if c.SuccessesToClose < 1 {
		c.SuccessesToClose = 1
	}
	return c
}

// Breaker is a circuit breaker that fast-fails when an upstream is
// unhealthy, giving it room to recover and shedding load instead of piling
// on doomed requests.
//
// Crucially, only transport-class failures (timeout, connect, 5xx, rate
// limit) should be reported as failures via OnResult / classified as
// failure by Guard's isFailure. Client/model errors such as
// model_unavailable indicate a bad request, not an unhealthy upstream, and
// MUST NOT trip the breaker — otherwise one malformed model name could
// blackhole all traffic.
//
// A Breaker is safe for concurrent use.
type Breaker struct {
	cfg BreakerConfig
	now func() time.Time // injectable for tests

	mu                  sync.Mutex
	state               BreakerState
	consecutiveFailures int
	openedAt            time.Time
	halfOpenProbes      int
	halfOpenSuccesses   int
}

// NewBreaker constructs a closed breaker.
func NewBreaker(cfg BreakerConfig) *Breaker {
	return &Breaker{
		cfg:   cfg.normalized(),
		now:   time.Now,
		state: StateClosed,
	}
}

// State returns the current state after applying any pending
// open -> half-open time transition.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()
	return b.state
}

// Allow reports whether a call may proceed. In half-open it admits up to
// HalfOpenMaxProbes probes; callers that receive true MUST eventually call
// OnResult so the probe accounting stays balanced.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maybeHalfOpen()
	switch b.state {
	case StateClosed:
		return true
	case StateHalfOpen:
		if b.halfOpenProbes < b.cfg.HalfOpenMaxProbes {
			b.halfOpenProbes++
			return true
		}
		return false
	default: // StateOpen
		return false
	}
}

// OnResult records the outcome of a call that Allow admitted. failure must
// be true only for transport-class failures (see the Breaker doc comment).
func (b *Breaker) OnResult(failure bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		if failure {
			b.consecutiveFailures++
			if b.consecutiveFailures >= b.cfg.FailureThreshold {
				b.trip()
			}
		} else {
			b.consecutiveFailures = 0
		}
	case StateHalfOpen:
		if failure {
			// Any probe failure immediately reopens the breaker.
			b.trip()
		} else {
			b.halfOpenSuccesses++
			if b.halfOpenSuccesses >= b.cfg.SuccessesToClose {
				b.close()
			}
		}
	case StateOpen:
		// No call should have been admitted in the open state; ignore
		// stray reports defensively.
	}
}

// Guard runs fn if the breaker allows it and records the outcome. isFailure
// classifies fn's error: true means a transport-class failure that should
// count against the breaker; false (or a nil error) is treated as a healthy
// outcome for breaker purposes (the error, if any, is still returned to the
// caller unchanged). A nil isFailure treats every non-nil error as a
// failure. Returns ErrCircuitOpen when the breaker rejects the call.
func (b *Breaker) Guard(fn func() error, isFailure func(error) bool) error {
	if !b.Allow() {
		return ErrCircuitOpen
	}
	// Record the outcome via defer so that even if fn panics the half-open
	// probe slot admitted by Allow is released (counted as a failure)
	// instead of leaking and wedging the breaker.
	var err error
	fail := true
	defer func() { b.OnResult(fail) }()
	err = fn()
	fail = err != nil && (isFailure == nil || isFailure(err))
	return err
}

// maybeHalfOpen transitions open -> half-open once the cooldown elapses.
// The caller must hold b.mu.
func (b *Breaker) maybeHalfOpen() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cfg.OpenDuration {
		b.state = StateHalfOpen
		b.halfOpenProbes = 0
		b.halfOpenSuccesses = 0
	}
}

// trip moves the breaker to open. The caller must hold b.mu.
func (b *Breaker) trip() {
	b.state = StateOpen
	b.openedAt = b.now()
	b.consecutiveFailures = 0
	b.halfOpenProbes = 0
	b.halfOpenSuccesses = 0
}

// close moves the breaker to closed. The caller must hold b.mu.
func (b *Breaker) close() {
	b.state = StateClosed
	b.consecutiveFailures = 0
	b.halfOpenProbes = 0
	b.halfOpenSuccesses = 0
}
