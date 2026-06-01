// Package config loads openscry runtime configuration from environment
// variables. It follows a fail-loud philosophy: required fields missing or
// malformed values surface as errors at startup rather than degrading
// silently inside the search path.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Configuration defaults. The model is deliberately NOT defaulted: per the
// openscry model constraint the model must be user-supplied (GROK_MODEL, or
// a per-call override) and is never defaulted or silently swapped on
// failure -- a missing GROK_MODEL fails loud at startup (see Validate).
const (
	// DefaultRequestTimeout bounds a single upstream search request.
	DefaultRequestTimeout = 120 * time.Second
	// MinRequestTimeout / MaxRequestTimeout clamp GROK_REQUEST_TIMEOUT.
	MinRequestTimeout = 5 * time.Second
	MaxRequestTimeout = 600 * time.Second

	// DefaultConcurrency / DefaultQueueSize size the MCP engine's worker
	// pool and bounded request queue (backpressure).
	DefaultConcurrency = 8
	DefaultQueueSize   = 64
	maxConcurrency     = 100
	maxQueueSize       = 10000

	// DefaultSearchProvider selects the search provider mode. "auto" resolves
	// to "responses" only for the official api.x.ai host, otherwise "chat"
	// (the proven path for grok2api). See ResolveSearchProvider.
	DefaultSearchProvider = "auto"

	// DefaultFetchFallback is the web_fetch degradation policy:
	//   "full"   (default) -- run the whole multi-tier chain down to the
	//            always-available basic-HTTP last resort; degradation is
	//            visible (the tier is reported) and availability is maximised.
	//   "strict" -- disable the low-fidelity basic-HTTP last resort so a
	//            failed extractor/model fails loud instead of silently
	//            degrading to HTML stripping.
	// Operators choose per deployment via GROK_FETCH_FALLBACK.
	DefaultFetchFallback = "full"

	// Default extractor endpoints for the optional web_fetch fallback chain.
	// They are only used when the corresponding API key is configured.
	DefaultTavilyURL    = "https://api.tavily.com"
	DefaultFirecrawlURL = "https://api.firecrawl.dev/v1"
)

// Config holds the resolved runtime configuration.
type Config struct {
	APIBaseURL     string        // GROK_API_URL (required) — OpenAI-compatible base, e.g. https://grok.aomanoh.tech/v1
	APIKey         string        // GROK_API_KEY (required)
	Model          string        // GROK_MODEL (required; user-supplied, never defaulted)
	RequestTimeout time.Duration // GROK_REQUEST_TIMEOUT (default 120s, clamped [5s,600s])
	Concurrency    int           // GROK_CONCURRENCY — MCP worker pool size (default 8, clamp [1,100])
	QueueSize      int           // GROK_QUEUE_SIZE — MCP request queue size (default 64, clamp [1,10000])
	Debug          bool          // GROK_DEBUG

	// SearchProvider is the raw mode (auto|chat|responses); resolve the
	// effective mode for a given base URL via ResolveSearchProvider.
	SearchProvider string // GROK_SEARCH_PROVIDER (default auto)

	// FetchFallback is the web_fetch degradation policy (full|strict); see
	// DefaultFetchFallback. "strict" disables the basic-HTTP last resort.
	FetchFallback string // GROK_FETCH_FALLBACK (default full)

	// Optional web_fetch extractor providers. A provider is only attempted
	// when its API key is non-empty; otherwise that fallback tier is skipped.
	TavilyAPIKey    string // TAVILY_API_KEY
	TavilyAPIURL    string // TAVILY_API_URL (default DefaultTavilyURL)
	FirecrawlAPIKey string // FIRECRAWL_API_KEY
	FirecrawlAPIURL string // FIRECRAWL_API_URL (default DefaultFirecrawlURL)
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		APIBaseURL:      strings.TrimSpace(os.Getenv("GROK_API_URL")),
		APIKey:          strings.TrimSpace(os.Getenv("GROK_API_KEY")),
		Model:           strings.TrimSpace(os.Getenv("GROK_MODEL")),
		RequestTimeout:  DefaultRequestTimeout,
		Concurrency:     DefaultConcurrency,
		QueueSize:       DefaultQueueSize,
		Debug:           parseBoolEnv("GROK_DEBUG"),
		SearchProvider:  DefaultSearchProvider,
		FetchFallback:   DefaultFetchFallback,
		TavilyAPIKey:    strings.TrimSpace(os.Getenv("TAVILY_API_KEY")),
		TavilyAPIURL:    firstNonEmpty(strings.TrimSpace(os.Getenv("TAVILY_API_URL")), DefaultTavilyURL),
		FirecrawlAPIKey: strings.TrimSpace(os.Getenv("FIRECRAWL_API_KEY")),
		FirecrawlAPIURL: firstNonEmpty(strings.TrimSpace(os.Getenv("FIRECRAWL_API_URL")), DefaultFirecrawlURL),
	}

	if raw := strings.TrimSpace(os.Getenv("GROK_REQUEST_TIMEOUT")); raw != "" {
		d, err := parseTimeout(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = clampDuration(d, MinRequestTimeout, MaxRequestTimeout)
	}

	if raw := strings.TrimSpace(os.Getenv("GROK_CONCURRENCY")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_CONCURRENCY (want integer): %w", err)
		}
		cfg.Concurrency = clampInt(n, 1, maxConcurrency)
	}
	if raw := strings.TrimSpace(os.Getenv("GROK_QUEUE_SIZE")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_QUEUE_SIZE (want integer): %w", err)
		}
		cfg.QueueSize = clampInt(n, 1, maxQueueSize)
	}

	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("GROK_SEARCH_PROVIDER"))); raw != "" {
		switch raw {
		case "auto", "chat", "responses":
			cfg.SearchProvider = raw
		default:
			return nil, fmt.Errorf("invalid GROK_SEARCH_PROVIDER %q (want auto|chat|responses)", raw)
		}
	}

	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("GROK_FETCH_FALLBACK"))); raw != "" {
		switch raw {
		case "full", "strict":
			cfg.FetchFallback = raw
		default:
			return nil, fmt.Errorf("invalid GROK_FETCH_FALLBACK %q (want full|strict)", raw)
		}
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
	if c.Model == "" {
		return errors.New("GROK_MODEL is required: the model is never defaulted -- set it explicitly to a model your grok2api deployment exposes")
	}
	return nil
}

// ResolveSearchProvider resolves the effective provider mode for a base URL.
// An explicit "chat"/"responses" is returned unchanged; "auto" (or anything
// else) maps to "responses" only for the official xAI host (api.x.ai),
// otherwise "chat" — the proven path for a grok2api reverse proxy.
func ResolveSearchProvider(mode, baseURL string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "chat" || mode == "responses" {
		return mode
	}
	host := ""
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "api.x.ai" || strings.HasSuffix(host, ".api.x.ai") {
		return "responses"
	}
	return "chat"
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

func clampInt(v, lo, hi int) int {
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
