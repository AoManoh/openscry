// Package refsource fetches additional reference sources for a query from
// Tavily and/or Firecrawl search endpoints. It augments a Grok web search
// (web_search extra_sources) with independently-discovered citations.
//
// This is distinct from internal/fetch (which extracts one URL's content via
// Tavily /extract and Firecrawl /scrape): refsource hits the /search
// endpoints (query -> a ranked list of result URLs), a capability the rewrite
// previously lacked. Failures are isolated and returned alongside the results
// so the caller can surface a degradation warning without failing the primary
// search — mirroring the GrokSearch baseline's extra_failures behavior.
package refsource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/AoManoh/openscry/internal/sources"
)

// Provider issues query->sources searches against Tavily and/or Firecrawl.
// A tier is used only when its API key is configured. Safe for concurrent use.
type Provider struct {
	http         *http.Client
	tavilyKey    string
	tavilyURL    string
	firecrawlKey string
	firecrawlURL string
}

// Options configures the reference-source provider.
type Options struct {
	TavilyAPIKey    string
	TavilyAPIURL    string
	FirecrawlAPIKey string
	FirecrawlAPIURL string
}

// New builds a Provider. Tiers with an empty key are disabled.
func New(opt Options) *Provider {
	return &Provider{
		http:         &http.Client{},
		tavilyKey:    strings.TrimSpace(opt.TavilyAPIKey),
		tavilyURL:    strings.TrimRight(firstNonEmpty(opt.TavilyAPIURL, "https://api.tavily.com"), "/"),
		firecrawlKey: strings.TrimSpace(opt.FirecrawlAPIKey),
		firecrawlURL: strings.TrimRight(firstNonEmpty(opt.FirecrawlAPIURL, "https://api.firecrawl.dev"), "/"),
	}
}

// Available reports whether at least one search tier is configured.
func (p *Provider) Available() bool {
	return p.tavilyKey != "" || p.firecrawlKey != ""
}

// Failure records why one tier's reference search failed. It is non-fatal:
// the primary Grok search still succeeds; the caller surfaces a warning.
type Failure struct {
	Provider string
	Message  string
}

// Search fetches up to n reference sources for query, splitting the quota
// across the configured tiers and running them concurrently. It returns the
// merged (URL-deduplicated) sources plus any per-tier failures. A nil/empty
// result with no failures simply means no extra sources were found.
//
// Quota allocation diverges deliberately from the GrokSearch baseline (which,
// via a `round(extra_sources * 1)` leftover, gave Firecrawl the whole quota
// and Tavily zero when both were configured): here the quota is split roughly
// evenly so both tiers contribute, widening source diversity.
func (p *Provider) Search(ctx context.Context, query string, n int) ([]sources.Source, []Failure) {
	query = strings.TrimSpace(query)
	if query == "" || n <= 0 || !p.Available() {
		return nil, nil
	}

	tavilyN, firecrawlN := p.allocate(n)

	var (
		mu        sync.Mutex
		tavilySrc []sources.Source
		fireSrc   []sources.Source
		failures  []Failure
		wg        sync.WaitGroup
	)
	record := func(provider string, src []sources.Source, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failures = append(failures, Failure{Provider: provider, Message: err.Error()})
			return
		}
		switch provider {
		case "tavily":
			tavilySrc = src
		case "firecrawl":
			fireSrc = src
		}
	}

	if tavilyN > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src, err := p.tavilySearch(ctx, query, tavilyN)
			record("tavily", src, err)
		}()
	}
	if firecrawlN > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src, err := p.firecrawlSearch(ctx, query, firecrawlN)
			record("firecrawl", src, err)
		}()
	}
	wg.Wait()

	merged := sources.Merge(fireSrc, tavilySrc)
	return merged, failures
}

// allocate splits the quota across configured tiers. With both configured the
// split is roughly even (Firecrawl gets the rounding-up half); with one
// configured it gets the whole quota.
func (p *Provider) allocate(n int) (tavilyN, firecrawlN int) {
	hasTavily := p.tavilyKey != ""
	hasFirecrawl := p.firecrawlKey != ""
	switch {
	case hasTavily && hasFirecrawl:
		firecrawlN = (n + 1) / 2
		tavilyN = n - firecrawlN
	case hasFirecrawl:
		firecrawlN = n
	case hasTavily:
		tavilyN = n
	}
	return tavilyN, firecrawlN
}

// tavilySearch calls Tavily's /search endpoint (query -> ranked results).
func (p *Provider) tavilySearch(ctx context.Context, query string, n int) ([]sources.Source, error) {
	body, err := json.Marshal(map[string]any{
		"query":               query,
		"max_results":         n,
		"search_depth":        "advanced",
		"include_raw_content": false,
		"include_answer":      false,
	})
	if err != nil {
		return nil, fmt.Errorf("tavily marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tavilyURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.tavilyKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tavily status %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var src []sources.Source
	for _, r := range out.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		src = append(src, sources.Source{
			Title:       strings.TrimSpace(r.Title),
			URL:         strings.TrimSpace(r.URL),
			Description: strings.TrimSpace(r.Content),
		})
	}
	return src, nil
}

// firecrawlSearch calls Firecrawl's /search endpoint (query -> web results).
func (p *Provider) firecrawlSearch(ctx context.Context, query string, n int) ([]sources.Source, error) {
	body, err := json.Marshal(map[string]any{"query": query, "limit": n})
	if err != nil {
		return nil, fmt.Errorf("firecrawl marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.firecrawlURL+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.firecrawlKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("firecrawl status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Web []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"web"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var src []sources.Source
	for _, r := range out.Data.Web {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		src = append(src, sources.Source{
			Title:       strings.TrimSpace(r.Title),
			URL:         strings.TrimSpace(r.URL),
			Description: strings.TrimSpace(r.Description),
		})
	}
	return src, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
