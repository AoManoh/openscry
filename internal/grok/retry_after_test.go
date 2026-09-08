package grok

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterSeconds(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Fatalf("got %v want 30s", got)
	}
	if got := parseRetryAfter("  5 "); got != 5*time.Second {
		t.Fatalf("got %v want 5s", got)
	}
}

func TestParseRetryAfterZeroOrNegative(t *testing.T) {
	if got := parseRetryAfter("0"); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
	if got := parseRetryAfter("-10"); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
}

func TestParseRetryAfterEmptyOrJunk(t *testing.T) {
	for _, v := range []string{"", "   ", "soon", "abc"} {
		if got := parseRetryAfter(v); got != 0 {
			t.Fatalf("parseRetryAfter(%q)=%v want 0", v, got)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().UTC().Add(45 * time.Second)
	header := future.Format(http.TimeFormat)
	got := parseRetryAfter(header)
	// Allow slack for the second-resolution HTTP date and test execution time.
	if got < 30*time.Second || got > 50*time.Second {
		t.Fatalf("got %v want ~45s", got)
	}
}

func TestParseRetryAfterPastDate(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Fatalf("got %v want 0 for a past date", got)
	}
}

func TestClassifyStatusErrorAttachesRetryAfter(t *testing.T) {
	err := classifyStatusError(http.StatusTooManyRequests, "slow down", "12")
	ge, ok := AsError(err)
	if !ok {
		t.Fatalf("not a grok error: %v", err)
	}
	if ge.Code != CodeRateLimit {
		t.Fatalf("code=%s want rate_limit", ge.Code)
	}
	if ge.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter=%v want 12s", ge.RetryAfter)
	}
	if !ge.Retryable {
		t.Fatal("429 should be retryable")
	}
}

func TestClassifyStatusErrorNoRetryAfterOn400(t *testing.T) {
	err := classifyStatusError(http.StatusBadRequest, "bad model", "30")
	ge, _ := AsError(err)
	if ge.Code != CodeModelUnavailable {
		t.Fatalf("code=%s want model_unavailable", ge.Code)
	}
	// 400 is not retryable; the Retry-After is irrelevant and must stay zero.
	if ge.RetryAfter != 0 {
		t.Fatalf("RetryAfter=%v want 0 on 400", ge.RetryAfter)
	}
}
