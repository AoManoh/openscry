// Package fetch implements web_fetch: retrieve a URL's content as Markdown
// via a multi-tier fallback chain under a single timeout budget. Tiers are
// attempted in order and the first non-empty result wins; every tier failure
// is recorded so a total failure reports which tiers were tried — degradation
// is always visible, never silent.
//
// Tier order mirrors the GrokSearch Python baseline:
//  1. Tavily extract    (only if TAVILY_API_KEY configured)
//  2. Firecrawl scrape  (only if FIRECRAWL_API_KEY configured)
//  3. Grok fetch        (grok2api /chat/completions + FetchPrompt)
//  4. basic HTTP        (GET + HTML-to-text), last resort
//
// A single OpFetch deadline bounds the whole chain: each tier shares the same
// context, so a slow tier consumes the common budget and later tiers fail
// fast rather than extending total latency.
package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
	"github.com/AoManoh/openscry/internal/resilience"
)

// Service runs the web_fetch fallback chain. It is safe for concurrent use.
type Service struct {
	client   *grok.Client
	model    string
	profiles resilience.Profiles
	http     *http.Client

	tavilyKey    string
	tavilyURL    string
	firecrawlKey string
	firecrawlURL string
}

// Options configures the fetch service. The Tavily and Firecrawl tiers are
// enabled only when their API key is non-empty; otherwise that tier is
// skipped entirely (no wasted round-trip).
type Options struct {
	Model           string
	Profiles        resilience.Profiles
	TavilyAPIKey    string
	TavilyAPIURL    string
	FirecrawlAPIKey string
	FirecrawlAPIURL string
}

// New builds a fetch Service over the given grok client.
func New(client *grok.Client, opt Options) *Service {
	return &Service{
		client:       client,
		model:        strings.TrimSpace(opt.Model),
		profiles:     opt.Profiles.Normalize(),
		http:         &http.Client{},
		tavilyKey:    strings.TrimSpace(opt.TavilyAPIKey),
		tavilyURL:    strings.TrimRight(firstNonEmpty(opt.TavilyAPIURL, "https://api.tavily.com"), "/"),
		firecrawlKey: strings.TrimSpace(opt.FirecrawlAPIKey),
		firecrawlURL: strings.TrimRight(firstNonEmpty(opt.FirecrawlAPIURL, "https://api.firecrawl.dev"), "/"),
	}
}

// Result is a successful fetch.
type Result struct {
	URL     string
	Content string
	Tier    string // which extractor produced the content: tavily|firecrawl|grok|http
	Model   string // set only when Tier == "grok"
}

// Fetch retrieves rawURL's content as Markdown, trying each tier in order
// under a single OpFetch timeout budget. The first non-empty result wins; if
// every tier fails the returned error lists the tiers tried.
func (s *Service) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	target := strings.TrimSpace(rawURL)
	if target == "" {
		return nil, fmt.Errorf("fetch: url must not be empty")
	}
	if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("fetch: invalid url %q (need an absolute http/https URL)", target)
	}

	// Budget: honour a caller-supplied deadline (CLI --timeout / MCP timeout
	// arg); otherwise apply the OpFetch profile. All tiers share this single
	// context, so a slow tier consumes the common budget.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.profiles.For(resilience.OpFetch))
		defer cancel()
	}
	opCtx := ctx

	// Reserve a slice of the budget for the always-available basic-HTTP last
	// resort so a slow extractor (e.g. a model browse that hangs) cannot
	// starve the reliable fallback. Extractor tiers (Tavily/Firecrawl/Grok)
	// run under extractorCtx (budget minus the reserve); basic HTTP runs under
	// the full opCtx and is thus guaranteed at least the reserve.
	extractorCtx := opCtx
	if dl, ok := opCtx.Deadline(); ok {
		reserve := time.Until(dl) / 3
		if reserve > 15*time.Second {
			reserve = 15 * time.Second
		}
		if reserve > 0 {
			var cancel context.CancelFunc
			extractorCtx, cancel = context.WithDeadline(opCtx, dl.Add(-reserve))
			defer cancel()
		}
	}

	var trace []string
	record := func(tier string, err error) {
		if err != nil {
			trace = append(trace, tier+": "+err.Error())
		} else {
			trace = append(trace, tier+": empty")
		}
	}

	// Tier 1: Tavily extract (opt-in via key).
	if s.tavilyKey != "" {
		content, err := s.tavilyExtract(extractorCtx, target)
		if err == nil && strings.TrimSpace(content) != "" {
			return &Result{URL: target, Content: content, Tier: "tavily"}, nil
		}
		record("tavily", err)
		if opCtx.Err() != nil {
			return nil, s.chainErr(target, trace, opCtx.Err())
		}
	}

	// Tier 2: Firecrawl scrape (opt-in via key).
	if s.firecrawlKey != "" {
		content, err := s.firecrawlScrape(extractorCtx, target)
		if err == nil && strings.TrimSpace(content) != "" {
			return &Result{URL: target, Content: content, Tier: "firecrawl"}, nil
		}
		record("firecrawl", err)
		if opCtx.Err() != nil {
			return nil, s.chainErr(target, trace, opCtx.Err())
		}
	}

	// Tier 3: Grok fetch via grok2api (FetchPrompt asks the model for the
	// page's structured Markdown). Reuses the shared streaming primitive.
	if s.client != nil {
		content, err := s.client.Complete(extractorCtx, s.model, prompt.FetchPrompt, prompt.FetchUserContent(target))
		if err == nil && strings.TrimSpace(content) != "" {
			return &Result{URL: target, Content: content, Tier: "grok", Model: s.model}, nil
		}
		record("grok", err)
		if opCtx.Err() != nil {
			return nil, s.chainErr(target, trace, opCtx.Err())
		}
	}

	// Tier 4: basic HTTP GET + HTML-to-text (last resort, no key needed).
	content, err := s.basicHTTPFetch(opCtx, target)
	if err == nil && strings.TrimSpace(content) != "" {
		return &Result{URL: target, Content: content, Tier: "http"}, nil
	}
	record("http", err)

	return nil, s.chainErr(target, trace, opCtx.Err())
}

// chainErr builds a visible, structured failure describing every tier tried.
func (s *Service) chainErr(target string, trace []string, ctxErr error) error {
	tiers := strings.Join(trace, "; ")
	if ctxErr != nil {
		return fmt.Errorf("fetch: budget exhausted before any tier succeeded for %s (tried: %s)", target, tiers)
	}
	return fmt.Errorf("fetch: all extractors failed for %s (tried: %s)", target, tiers)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
