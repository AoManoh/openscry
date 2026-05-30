package search

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/resilience"
)

func TestSearchReturnsAccumulatedContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "test-key", 5*time.Second), "grok-4.3-console")
	res, err := svc.Search(context.Background(), Request{Query: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "Hello world" {
		t.Fatalf("unexpected content: %q", res.Content)
	}
	if res.Model != "grok-4.3-console" {
		t.Fatalf("unexpected model: %q", res.Model)
	}
}

func TestSearchModelFailureIsExplicitNoSilentFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "default-model")
	_, err := svc.Search(context.Background(), Request{Query: "hi", Model: "missing-model"})
	if err == nil {
		t.Fatal("expected explicit error, got nil")
	}
	ge, ok := grok.AsError(err)
	if !ok {
		t.Fatalf("expected *grok.Error, got %T", err)
	}
	if ge.Code != grok.CodeModelUnavailable {
		t.Fatalf("expected model_unavailable, got %q", ge.Code)
	}
}

func TestSearchEmptyContentIsVisibleError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	// MaxAttempts=1 keeps the test fast and focused on "empty is a visible
	// error" rather than exercising the retry-on-empty path.
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m", Options{MaxAttempts: 1})
	_, err := svc.Search(context.Background(), Request{Query: "hi"})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeEmpty {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestSearchRetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // transient 5xx
			_, _ = w.Write([]byte(`{"error":{"message":"upstream hiccup"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m",
		Options{MaxAttempts: 3, RetryBaseDelay: time.Millisecond})
	res, err := svc.Search(context.Background(), Request{Query: "hi"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if res.Content != "recovered" {
		t.Fatalf("content=%q want \"recovered\"", res.Content)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls=%d want 3 (2 transient failures + 1 success)", got)
	}
}

// TestSearchCircuitBreakerOpensUnderSustainedFailure is the S2 fault-injection
// gate: sustained upstream 5xx must trip the breaker so further calls fail
// fast with ErrCircuitOpen instead of hammering the dead upstream.
func TestSearchCircuitBreakerOpensUnderSustainedFailure(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer srv.Close()

	// FailureThreshold=2, no retry: the first two searches each record one
	// transport failure and trip the breaker; the third must fail fast with
	// ErrCircuitOpen without reaching upstream.
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m", Options{
		MaxAttempts: 1,
		Breaker:     resilience.BreakerConfig{FailureThreshold: 2, OpenDuration: time.Minute},
	})

	_, err1 := svc.Search(context.Background(), Request{Query: "q"})
	_, err2 := svc.Search(context.Background(), Request{Query: "q"})
	if err1 == nil || err2 == nil {
		t.Fatalf("expected first two searches to fail, got %v / %v", err1, err2)
	}
	callsAfterTrip := calls.Load()

	_, err3 := svc.Search(context.Background(), Request{Query: "q"})
	if !errors.Is(err3, resilience.ErrCircuitOpen) {
		t.Fatalf("third search err=%v want ErrCircuitOpen", err3)
	}
	if got := calls.Load(); got != callsAfterTrip {
		t.Fatalf("breaker-open call reached upstream: calls %d -> %d", callsAfterTrip, got)
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	svc := New(grok.NewClient("http://example.invalid", "k", time.Second), "m")
	_, err := svc.Search(context.Background(), Request{Query: "   "})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-query error, got %v", err)
	}
}
