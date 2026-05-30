// Package resilience provides transport-agnostic resilience primitives used
// to wrap upstream grok2api calls: per-operation timeout profiles, bounded
// retry with a shared retry budget (amplification control), and a circuit
// breaker.
//
// The package is a leaf: it imports only the standard library and never the
// grok/search packages. Callers inject error classification via function
// values (e.g. IsRetryable), which keeps this package free of import cycles
// and lets it be unit-tested with fake clocks and stub operations.
package resilience

import "time"

// OpKind identifies an operation class for timeout selection. Different
// operations have very different latency profiles: a deep web search can
// legitimately take a minute, while a single page fetch should be short.
type OpKind string

const (
	// OpSearch is a deep web search (long timeout).
	OpSearch OpKind = "search"
	// OpFetch is a single-page fetch (short timeout).
	OpFetch OpKind = "fetch"
	// OpMap is a bounded site map/crawl (medium timeout).
	OpMap OpKind = "map"
)

// Default per-operation timeouts.
const (
	DefaultSearchTimeout = 120 * time.Second
	DefaultFetchTimeout  = 30 * time.Second
	DefaultMapTimeout    = 90 * time.Second

	// minTimeout / maxTimeout clamp any configured profile so a
	// misconfiguration cannot disable timeouts or set an absurd ceiling.
	minTimeout = 1 * time.Second
	maxTimeout = 600 * time.Second
)

// Profiles holds the timeout for each operation class. The zero value is
// not usable directly; call Normalize (or DefaultProfiles) first.
type Profiles struct {
	Search time.Duration
	Fetch  time.Duration
	Map    time.Duration
}

// DefaultProfiles returns the built-in timeout profile set.
func DefaultProfiles() Profiles {
	return Profiles{
		Search: DefaultSearchTimeout,
		Fetch:  DefaultFetchTimeout,
		Map:    DefaultMapTimeout,
	}
}

// For returns the timeout for op, clamped to [minTimeout, maxTimeout].
// Unknown operation kinds fall back to the search timeout (the most
// permissive), so a new caller never accidentally gets a too-short bound.
func (p Profiles) For(op OpKind) time.Duration {
	var d time.Duration
	switch op {
	case OpSearch:
		d = p.Search
	case OpFetch:
		d = p.Fetch
	case OpMap:
		d = p.Map
	default:
		d = p.Search
	}
	return clampDuration(d)
}

// Normalize fills zero/negative fields with the defaults and clamps every
// field into the valid range. It is idempotent.
func (p Profiles) Normalize() Profiles {
	def := DefaultProfiles()
	if p.Search <= 0 {
		p.Search = def.Search
	}
	if p.Fetch <= 0 {
		p.Fetch = def.Fetch
	}
	if p.Map <= 0 {
		p.Map = def.Map
	}
	p.Search = clampDuration(p.Search)
	p.Fetch = clampDuration(p.Fetch)
	p.Map = clampDuration(p.Map)
	return p
}

func clampDuration(d time.Duration) time.Duration {
	if d < minTimeout {
		return minTimeout
	}
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}
