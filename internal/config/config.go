// Package config loads openscry runtime configuration from environment
// variables. It follows a fail-loud philosophy: required fields missing or
// malformed values surface as errors at startup rather than degrading
// silently inside the search path.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Configuration defaults. The default model is intentionally a named
// constant (not hard-coded at the call site) so it can be overridden per
// deployment via GROK_MODEL or per call via a CLI flag / MCP argument.
const (
	// DefaultModel mirrors the model proven working on the current grok2api
	// deployment (grok-4.20-fast performs real, cited web search; the
	// previously assumed grok-4.3-console returns HTTP 500 there). It is
	// overridable via GROK_MODEL / --model and is never silently swapped on
	// failure (see internal/grok error handling).
	DefaultModel = "grok-4.20-fast"

	// DefaultRequestTimeout bounds a single upstream search request.
	DefaultRequestTimeout = 120 * time.Second
	// MinRequestTimeout / MaxRequestTimeout clamp GROK_REQUEST_TIMEOUT.
	MinRequestTimeout = 5 * time.Second
	MaxRequestTimeout = 600 * time.Second
)

// Config holds the resolved runtime configuration.
type Config struct {
	APIBaseURL     string        // GROK_API_URL (required) — OpenAI-compatible base, e.g. https://grok.aomanoh.tech/v1
	APIKey         string        // GROK_API_KEY (required)
	Model          string        // GROK_MODEL (default DefaultModel)
	RequestTimeout time.Duration // GROK_REQUEST_TIMEOUT (default 120s, clamped [5s,600s])
	Debug          bool          // GROK_DEBUG
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		APIBaseURL:     strings.TrimSpace(os.Getenv("GROK_API_URL")),
		APIKey:         strings.TrimSpace(os.Getenv("GROK_API_KEY")),
		Model:          firstNonEmpty(strings.TrimSpace(os.Getenv("GROK_MODEL")), DefaultModel),
		RequestTimeout: DefaultRequestTimeout,
		Debug:          parseBoolEnv("GROK_DEBUG"),
	}

	if raw := strings.TrimSpace(os.Getenv("GROK_REQUEST_TIMEOUT")); raw != "" {
		d, err := parseTimeout(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = clampDuration(d, MinRequestTimeout, MaxRequestTimeout)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces required fields.
func (c *Config) Validate() error {
	if c.APIBaseURL == "" {
		return errors.New("GROK_API_URL is required (OpenAI-compatible base URL of grok2api, e.g. https://host/v1)")
	}
	if c.APIKey == "" {
		return errors.New("GROK_API_KEY is required")
	}
	return nil
}

// MaskAPIKey returns a redacted form of the API key for display/logging.
func MaskAPIKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}

// parseTimeout accepts either a Go duration ("120s", "2m") or a bare number
// interpreted as seconds ("120"), staying compatible with the Python
// baseline which treated GROK_REQUEST_TIMEOUT as float seconds.
func parseTimeout(raw string) (time.Duration, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("expected Go duration (e.g. 120s) or seconds (e.g. 120), got %q", raw)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

func clampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseBoolEnv(key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return false
	}
	return v
}
