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
// Degradation policy (Strict): when Strict is false (the default, "full")
// the chain runs to completion including the basic-HTTP last resort. When
// Strict is true ("strict") the basic-HTTP last resort is disabled, so a
// failed extractor/model fails loud with the tiers tried instead of
// degrading to low-fidelity HTML stripping. The choice is operator-set via
// GROK_FETCH_FALLBACK.
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
	strict   bool // when true, disable the basic-HTTP last resort (fail loud)

	tavilyKey    string
	tavilyURL    string
	firecrawlKey string
	firecrawlURL string
	// tools 是 Grok 层请求声明的托管工具（通常只有 web_search）。xAI 的 web_search
	// 服务端工具组含 browse_page / open_page，模型借此才能真正打开目标 URL，
	// 否则 Console 路由下只能回复 FetchFailureSentinel。
	tools []string
}

// Options configures the fetch service. The Tavily and Firecrawl tiers are
// enabled only when their API key is non-empty; otherwise that tier is
// skipped entirely (no wasted round-trip).
type Options struct {
	Model           string
	Profiles        resilience.Profiles
	Strict          bool // GROK_FETCH_FALLBACK=strict: disable basic-HTTP last resort
	TavilyAPIKey    string
	TavilyAPIURL    string
	FirecrawlAPIKey string
	FirecrawlAPIURL string
	// Tools 是 Grok 层声明的托管工具类型；接线时传入 config.Config.FetchTools()。
	// 空表示不声明，保持历史请求形态。
	Tools []string
}

// New builds a fetch Service over the given grok client.
func New(client *grok.Client, opt Options) *Service {
	return &Service{
		client:       client,
		model:        strings.TrimSpace(opt.Model),
		profiles:     opt.Profiles.Normalize(),
		http:         &http.Client{},
		strict:       opt.Strict,
		tavilyKey:    strings.TrimSpace(opt.TavilyAPIKey),
		tavilyURL:    strings.TrimRight(firstNonEmpty(opt.TavilyAPIURL, "https://api.tavily.com"), "/"),
		firecrawlKey: strings.TrimSpace(opt.FirecrawlAPIKey),
		firecrawlURL: strings.TrimRight(firstNonEmpty(opt.FirecrawlAPIURL, "https://api.firecrawl.dev"), "/"),
		tools:        append([]string(nil), opt.Tools...),
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
	// the full opCtx and is thus guaranteed at least the reserve. In strict
	// mode there is no basic-HTTP tier, so extractors use the whole budget.
	extractorCtx := opCtx
	if dl, ok := opCtx.Deadline(); ok && !s.strict {
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
		content, err := s.client.CompleteWithTools(extractorCtx, s.model, prompt.FetchPrompt, prompt.FetchUserContent(target), s.tools)
		if err == nil && strings.Contains(content, prompt.FetchFailureSentinel) {
			// The model signalled it could not retrieve the page (see
			// prompt.FetchFailureSentinel). Treat this as a tier failure so
			// the chain falls through to basic-HTTP (full) or fails loud
			// (strict), rather than accepting failure-narration as content.
			err = fmt.Errorf("grok tier: model reported page unavailable")
		}
		if err == nil && strings.Contains(content, prompt.FetchPartialSentinel) {
			// 模型承认只拿到部分正文：残缺内容不能当成功返回，按该层失败处理。
			err = fmt.Errorf("grok tier: model reported partial content")
		}
		if err == nil && strings.TrimSpace(content) != "" {
			return &Result{URL: target, Content: content, Tier: "grok", Model: s.model}, nil
		}
		record("grok", err)
		if opCtx.Err() != nil {
			return nil, s.chainErr(target, trace, opCtx.Err())
		}
	}

	// Tier 4: basic HTTP GET + HTML-to-text (last resort, no key needed).
	// Skipped in strict mode: the low-fidelity fallback is disabled so a
	// failed extractor/model fails loud rather than degrading silently.
	if !s.strict {
		content, err := s.basicHTTPFetch(opCtx, target)
		if err == nil && strings.TrimSpace(content) != "" {
			return &Result{URL: target, Content: content, Tier: "http"}, nil
		}
		record("http", err)
	}

	return nil, s.chainErr(target, trace, opCtx.Err())
}

// chainErr builds a visible, structured failure describing every tier tried.
func (s *Service) chainErr(target string, trace []string, ctxErr error) error {
	tiers := strings.Join(trace, "; ")
	mode := ""
	if s.strict {
		mode = " [strict: basic-HTTP fallback disabled]"
	}
	if ctxErr != nil {
		return fmt.Errorf("fetch: budget exhausted before any tier succeeded for %s%s (tried: %s)", target, mode, tiers)
	}
	return fmt.Errorf("fetch: all extractors failed for %s%s (tried: %s)", target, mode, tiers)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
