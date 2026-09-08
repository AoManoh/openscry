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
	"regexp"
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
	// FetchedURL 是实际抓取的地址；与 URL 不同时表示做过改写（如 GitHub blob -> raw）。
	FetchedURL string
}

var githubBlobPattern = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([^/\s]+)/([^/\s]+)/(?:blob|raw)/([^/\s]+)/(.+)$`)

// RewriteGitHubBlobURL 把 github.com/{owner}/{repo}/blob|raw/{ref}/{path} 改写为
// raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}；其它 URL 原样返回。片段
// 标识（#L10-L20）会被去掉，因为 raw 文件没有行锚点。
func RewriteGitHubBlobURL(raw string) string {
	m := githubBlobPattern.FindStringSubmatch(raw)
	if m == nil {
		return raw
	}
	path := m[4]
	if i := strings.IndexAny(path, "#"); i >= 0 {
		path = path[:i]
	}
	return "https://raw.githubusercontent.com/" + m[1] + "/" + m[2] + "/" + m[3] + "/" + path
}

// Fetch retrieves rawURL's content as Markdown, trying each tier in order
// under a single OpFetch timeout budget. The first non-empty result wins; if
// every tier fails the returned error lists the tiers tried.
func (s *Service) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	return s.FetchWithTimeout(ctx, rawURL, 0)
}

// FetchWithTimeout 与 Fetch 相同，但允许调用方给出显式预算（CLI --timeout / MCP timeout
// 参数）；timeout <= 0 表示使用 OpFetch 档位。预算以参数传递而不是由调用方给上下文设
// 截止时间，是为了让服务层能区分"调用方要求的预算"与"传输层的请求上限"：后者只应
// 收紧预算，不应替代默认预算。
func (s *Service) FetchWithTimeout(ctx context.Context, rawURL string, timeout time.Duration) (*Result, error) {
	target := strings.TrimSpace(rawURL)
	if target == "" {
		return nil, fmt.Errorf("fetch: url must not be empty")
	}
	if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("fetch: invalid url %q (need an absolute http/https URL)", target)
	}
	// GitHub 的 blob 页面是带行号的渲染骨架，各抓取层拿到的都是页面壳而不是文件内容；
	// 改写为 raw.githubusercontent.com 直接取文件原文。Result.URL 仍报告调用方给的地址，
	// 实际抓取地址放在 Result.FetchedURL。
	requested := target
	target = RewriteGitHubBlobURL(target)

	// 预算：显式预算优先，否则用 OpFetch 档位；两者之一总会作为操作上下文的截止时间
	// 生效，父上下文更早的截止时间由 context 自动保留。之前"父上下文已有截止时间就跳过
	// 档位"的做法让 MCP HTTP 传输的 10 分钟请求上限冒充了抓取预算，同一次抓取在 stdio
	// 与 HTTP 下的行为因此不一致。所有层共用这一个上下文，慢的层消耗的是公共预算。
	budget := timeout
	if budget <= 0 {
		budget = s.profiles.For(resilience.OpFetch)
	}
	opCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// Reserve a slice of the budget for the always-available basic-HTTP last
	// resort so a slow extractor (e.g. a model browse that hangs) cannot
	// starve the reliable fallback. Extractor tiers (Tavily/Firecrawl/Grok)
	// run under extractorCtx (budget minus the reserve); basic HTTP runs under
	// the full opCtx and is thus guaranteed at least the reserve. In strict
	// mode there is no basic-HTTP tier, so extractors use the whole budget.
	// 预留量按操作上下文的实际截止时间计算：父上下文更早时以父上下文为准，与之前一致。
	extractorCtx := opCtx
	if dl, ok := opCtx.Deadline(); ok && !s.strict {
		reserve := time.Until(dl) / 3
		if reserve > 15*time.Second {
			reserve = 15 * time.Second
		}
		if reserve > 0 {
			var cancelExtractor context.CancelFunc
			extractorCtx, cancelExtractor = context.WithDeadline(opCtx, dl.Add(-reserve))
			defer cancelExtractor()
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
			return &Result{URL: requested, FetchedURL: target, Content: content, Tier: "tavily"}, nil
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
			return &Result{URL: requested, FetchedURL: target, Content: content, Tier: "firecrawl"}, nil
		}
		record("firecrawl", err)
		if opCtx.Err() != nil {
			return nil, s.chainErr(target, trace, opCtx.Err())
		}
	}

	// Tier 3: Grok fetch via grok2api (FetchPrompt asks the model for the
	// page's structured Markdown). Reuses the shared streaming primitive.
	if s.client != nil {
		content := ""
		completion, err := s.client.CompleteDetailed(extractorCtx, s.model, prompt.FetchPrompt, prompt.FetchUserContent(target), s.tools)
		if err == nil {
			content = completion.Content
			if completion.State != grok.StateComplete {
				// 抓取承诺的是完整页面：流未确认完整（截断 / 过滤 / 未收到 [DONE]）时，残缺
				// 正文不能当成功页面返回，按该层失败处理，继续降级（full）或显式失败（strict）。
				// 这与模型自报 OPENSCRY_FETCH_PARTIAL 的处理口径一致，只是判据来自传输层。
				err = fmt.Errorf("grok tier: response incomplete (%s: %s)", completion.State, completion.Detail)
			}
		}
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
			return &Result{URL: requested, FetchedURL: target, Content: content, Tier: "grok", Model: s.model}, nil
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
			return &Result{URL: requested, FetchedURL: target, Content: content, Tier: "http"}, nil
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
