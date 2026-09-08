// Package mapper implements web_map: discover a website's URL structure by
// traversing it as a graph. Two tiers are available:
//
//  1. Tavily /map  (when TAVILY_API_KEY is configured) — high-quality,
//     handles JS-rendered pages and SPA content.
//  2. Pure HTTP    (always available) — GET + HTML link extraction via BFS.
//
// The Tavily tier is tried first when its key is present; if it fails or
// returns nothing, the HTTP tier takes over. Unlike web_fetch there is no
// "strict" policy: when Tavily is unavailable HTTP is the only option (not
// degradation, just "have or don't have"). A single OpMap timeout budget
// bounds the whole operation.
package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/resilience"
)

// Service runs the web_map site structure discovery. Safe for concurrent use.
type Service struct {
	http      *http.Client
	tavilyKey string
	tavilyURL string
	profiles  resilience.Profiles
}

// Options configures the mapper service.
type Options struct {
	TavilyAPIKey string
	TavilyAPIURL string
	Profiles     resilience.Profiles
}

// New builds a mapper Service.
func New(opt Options) *Service {
	tavilyURL := strings.TrimRight(strings.TrimSpace(opt.TavilyAPIURL), "/")
	if tavilyURL == "" {
		tavilyURL = "https://api.tavily.com"
	}
	return &Service{
		http:      &http.Client{},
		tavilyKey: strings.TrimSpace(opt.TavilyAPIKey),
		tavilyURL: tavilyURL,
		profiles:  opt.Profiles.Normalize(),
	}
}

// Request configures a single map operation.
type Request struct {
	URL          string // root URL to start mapping (required)
	MaxDepth     int    // maximum traversal depth from root (default 1)
	MaxBreadth   int    // maximum links to follow per page (default 20)
	Limit        int    // total URL cap (default 50)
	Instructions string // optional natural-language filter for the crawler
	// Timeout 是调用方的显式预算（CLI --timeout / MCP timeout 参数），0 表示使用 OpMap
	// 档位。预算以参数传递而不是由调用方给上下文设截止时间，是为了让服务层能区分
	// "调用方要求的预算"与"传输层的请求上限"：后者只应收紧预算，不应替代默认预算。
	Timeout time.Duration
}

func (r Request) normalized() Request {
	if r.MaxDepth < 1 {
		r.MaxDepth = 1
	}
	if r.MaxDepth > 5 {
		r.MaxDepth = 5
	}
	if r.MaxBreadth < 1 {
		r.MaxBreadth = 20
	}
	if r.MaxBreadth > 500 {
		r.MaxBreadth = 500
	}
	if r.Limit < 1 {
		r.Limit = 50
	}
	if r.Limit > 500 {
		r.Limit = 500
	}
	return r
}

// Result is a successful map operation.
type Result struct {
	RootURL string   // the root URL as requested
	URLs    []string // discovered URLs (deduplicated)
	Tier    string   // which tier produced results: "tavily" or "http"
	Warning string   // non-empty when degradation occurred (e.g. instructions not honored)
}

// Map discovers the URL structure of a site starting at req.URL. It tries
// the Tavily tier first (when configured) and falls back to HTTP crawl.
func (s *Service) Map(ctx context.Context, req Request) (*Result, error) {
	target := strings.TrimSpace(req.URL)
	if target == "" {
		return nil, fmt.Errorf("mapper: url must not be empty")
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("mapper: invalid url %q (need an absolute http/https URL)", target)
	}

	// 预算：显式预算优先，否则用 OpMap 档位；两者之一总会作为操作上下文的截止时间生效，
	// 父上下文更早的截止时间由 context 自动保留。不再因父上下文已有截止时间而跳过档位，
	// 否则 MCP HTTP 传输的请求上限会冒充 map 预算，同一次 map 在 stdio 与 HTTP 下行为不一致。
	budget := req.Timeout
	if budget <= 0 {
		budget = s.profiles.For(resilience.OpMap)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	req = req.normalized()

	// Tier 1: Tavily /map (when key is configured).
	tavilyAttempted := false
	if s.tavilyKey != "" {
		tavilyAttempted = true
		urls, err := s.tavilyMap(ctx, target, req)
		if err == nil && len(urls) > 0 {
			return &Result{RootURL: target, URLs: urls, Tier: "tavily"}, nil
		}
		// Tavily failed or empty; fall through to HTTP.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("mapper: budget exhausted during Tavily tier for %s: %w", target, ctx.Err())
		}
	}

	// Tier 2: Pure HTTP BFS crawl.
	urls, err := s.httpCrawl(ctx, u, req)
	if err != nil {
		return nil, fmt.Errorf("mapper: http crawl failed for %s: %w", target, err)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("mapper: no URLs discovered for %s", target)
	}

	// Degradation visibility: if instructions were provided but we fell to
	// the HTTP tier (which cannot filter), the user must know their filter
	// was not honored. This satisfies AGENTS.md §10.1 and the openscry
	// "degradation is visible" principle.
	var warning string
	if req.Instructions != "" {
		if tavilyAttempted {
			warning = "instructions provided but Tavily returned empty results; fell back to HTTP BFS which does not support filtering — results are unfiltered"
		} else {
			warning = "instructions provided but no Tavily API key configured; HTTP BFS does not support filtering — results are unfiltered"
		}
	}
	return &Result{RootURL: target, URLs: urls, Tier: "http", Warning: warning}, nil
}

// tavilyMap calls Tavily's /map endpoint.
func (s *Service) tavilyMap(ctx context.Context, target string, req Request) ([]string, error) {
	body, err := json.Marshal(map[string]any{
		"url":          target,
		"max_depth":    req.MaxDepth,
		"max_breadth":  req.MaxBreadth,
		"limit":        req.Limit,
		"instructions": req.Instructions,
	})
	if err != nil {
		return nil, fmt.Errorf("tavily marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tavilyURL+"/map", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.tavilyKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tavily /map status %d", resp.StatusCode)
	}

	var out struct {
		Results []string `json:"results"` // Tavily /map 实际返回字段
		URLs    []string `json:"urls"`    // 兼容旧契约/其他实现
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	// Tavily /map 返回 results；保留 urls 兼容
	if len(out.Results) > 0 {
		return out.Results, nil
	}
	return out.URLs, nil
}

// httpCrawl performs a BFS link crawl starting at root, respecting
// depth/breadth/limit constraints and staying on the same host.
func (s *Service) httpCrawl(ctx context.Context, root *url.URL, req Request) ([]string, error) {
	type entry struct {
		u     *url.URL
		depth int
	}

	visited := map[string]bool{root.String(): true}
	queue := []entry{{u: root, depth: 0}}
	var result []string

	for len(queue) > 0 && len(result) < req.Limit {
		if ctx.Err() != nil {
			break
		}
		cur := queue[0]
		queue = queue[1:]
		result = append(result, cur.u.String())

		if cur.depth >= req.MaxDepth {
			continue
		}

		links, err := s.extractLinks(ctx, cur.u)
		if err != nil {
			// The root being unreachable is fatal: a crawl that discovered
			// nothing beyond the seed is a failure, not a success. Deeper
			// pages remain best-effort and are skipped on error.
			if cur.depth == 0 {
				return nil, fmt.Errorf("root page unreachable: %w", err)
			}
			continue // non-fatal: skip unreachable child pages
		}

		added := 0
		for _, link := range links {
			if added >= req.MaxBreadth || len(result)+len(queue) >= req.Limit {
				break
			}
			// Same-host filter.
			if !strings.EqualFold(link.Host, root.Host) {
				continue
			}
			canonical := link.String()
			if visited[canonical] {
				continue
			}
			visited[canonical] = true
			queue = append(queue, entry{u: link, depth: cur.depth + 1})
			added++
		}
	}

	return result, nil
}

// maxCrawlPageBytes limits how much of a page we read for link extraction.
const maxCrawlPageBytes = 2 << 20 // 2 MiB

// reHref extracts href values from anchor tags.
var reHref = regexp.MustCompile(`(?i)<a\s[^>]*href\s*=\s*["']([^"'#][^"']*)["']`)

// extractLinks fetches a page and returns the resolved absolute links found.
func (s *Service) extractLinks(ctx context.Context, page *url.URL) ([]*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "openscry-mapper/0.1")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCrawlPageBytes))
	if err != nil {
		return nil, err
	}

	matches := reHref.FindAllSubmatch(raw, -1)
	var links []*url.URL
	for _, m := range matches {
		href := strings.TrimSpace(string(m[1]))
		if href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") {
			continue
		}
		resolved, err := page.Parse(href)
		if err != nil {
			continue
		}
		// Normalize: strip fragment, ensure scheme.
		resolved.Fragment = ""
		if resolved.Scheme == "http" || resolved.Scheme == "https" {
			links = append(links, resolved)
		}
	}
	return links, nil
}
