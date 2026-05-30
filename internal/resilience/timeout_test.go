package resilience

import (
	"testing"
	"time"
)

func TestProfilesForDefaults(t *testing.T) {
	p := DefaultProfiles()
	if got := p.For(OpSearch); got != DefaultSearchTimeout {
		t.Errorf("search: got %v want %v", got, DefaultSearchTimeout)
	}
	if got := p.For(OpFetch); got != DefaultFetchTimeout {
		t.Errorf("fetch: got %v want %v", got, DefaultFetchTimeout)
	}
	if got := p.For(OpMap); got != DefaultMapTimeout {
		t.Errorf("map: got %v want %v", got, DefaultMapTimeout)
	}
}

func TestProfilesForUnknownFallsBackToSearch(t *testing.T) {
	p := DefaultProfiles()
	if got := p.For(OpKind("unrecognized")); got != DefaultSearchTimeout {
		t.Errorf("unknown op: got %v want %v (search fallback)", got, DefaultSearchTimeout)
	}
}

func TestProfilesForClamps(t *testing.T) {
	p := Profiles{Search: 9999 * time.Hour, Fetch: time.Millisecond, Map: DefaultMapTimeout}
	if got := p.For(OpSearch); got != maxTimeout {
		t.Errorf("over-max not clamped: got %v want %v", got, maxTimeout)
	}
	if got := p.For(OpFetch); got != minTimeout {
		t.Errorf("under-min not clamped: got %v want %v", got, minTimeout)
	}
}

func TestProfilesNormalizeFillsZeroWithDefaults(t *testing.T) {
	got := Profiles{}.Normalize()
	if got != DefaultProfiles() {
		t.Errorf("Normalize zero value: got %+v want %+v", got, DefaultProfiles())
	}
}

func TestProfilesNormalizeIsIdempotent(t *testing.T) {
	once := Profiles{Search: 99 * time.Hour, Fetch: 0, Map: 45 * time.Second}.Normalize()
	twice := once.Normalize()
	if once != twice {
		t.Errorf("Normalize not idempotent: %+v vs %+v", once, twice)
	}
}
