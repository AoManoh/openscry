package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
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

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "m")
	_, err := svc.Search(context.Background(), Request{Query: "hi"})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeEmpty {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	svc := New(grok.NewClient("http://example.invalid", "k", time.Second), "m")
	_, err := svc.Search(context.Background(), Request{Query: "   "})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-query error, got %v", err)
	}
}
