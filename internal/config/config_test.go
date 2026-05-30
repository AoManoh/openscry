package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresModel(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "") // empty = unset; must fail loud

	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail when GROK_MODEL is unset, got nil")
	}
	if !strings.Contains(err.Error(), "GROK_MODEL") {
		t.Fatalf("error should mention GROK_MODEL, got: %v", err)
	}
}

func TestLoadModelIsUserSupplied(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "my-model" {
		t.Fatalf("Model = %q, want my-model", cfg.Model)
	}
	if cfg.FetchFallback != "full" {
		t.Fatalf("FetchFallback default = %q, want full", cfg.FetchFallback)
	}
}

func TestLoadFetchFallbackValidation(t *testing.T) {
	t.Setenv("GROK_API_URL", "https://example.test/v1")
	t.Setenv("GROK_API_KEY", "k-test")
	t.Setenv("GROK_MODEL", "my-model")

	t.Setenv("GROK_FETCH_FALLBACK", "strict")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error for strict: %v", err)
	}
	if cfg.FetchFallback != "strict" {
		t.Fatalf("FetchFallback = %q, want strict", cfg.FetchFallback)
	}

	t.Setenv("GROK_FETCH_FALLBACK", "bogus")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid GROK_FETCH_FALLBACK, got nil")
	}
}
