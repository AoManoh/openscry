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
	"regexp"
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

	// DefaultUpstreamConcurrency caps simultaneous in-flight grok2api calls
	// across every consumer sharing the client (search, web_search_batch
	// fan-out, fetch) regardless of transport. Unlike GROK_CONCURRENCY (which
	// bounds MCP request *processing* and only in stdio mode), this bounds
	// the actual upstream load and is honored in both stdio and HTTP. It
	// defaults ON (protect-by-default); set GROK_UPSTREAM_CONCURRENCY=0 to
	// disable. Distinct knob because request concurrency and upstream
	// concurrency are genuinely different concerns.
	DefaultUpstreamConcurrency = 32
	maxUpstreamConcurrency     = 256

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

	// DefaultMCPTools selects which tool set the MCP server exposes:
	//   "core" (default) -- the six request/response tools agents actually
	//            drive: web_search, web_fetch, web_map, research_plan,
	//            web_search_batch, get_config_info. Minimal selection load.
	//   "all"  -- core plus the async task-lifecycle tools (submit / get /
	//            cancel / list_search_task), opt-in for long-running or
	//            fire-and-poll workloads. The capability always exists in the
	//            binary; this only gates the advertised surface.
	// Operators choose via GROK_MCP_TOOLS or the `mcp --tools` flag.
	DefaultMCPTools = "core"

	// DefaultSearchTools 是搜索请求默认声明的托管（服务端）工具清单。
	// grok2api v3 的 Console 路由只转发客户端显式声明的 tools，不再自动注入搜索
	// 工具；Grok Web 路由则始终原生搜索并把 web_search 声明视为无害标记。因此
	// 默认声明 web_search 才能让搜索在两类路由上都真正发生。x_search 不设默认：
	// Grok Web 路由会拒绝它（上游 400），需要时通过 GROK_SEARCH_TOOLS 显式追加。
	// 设为 none 可完全关闭声明，恢复只发 model/messages/stream 的旧请求形态。
	DefaultSearchTools = "web_search"

	// SearchToolsDisabled 是 GROK_SEARCH_TOOLS 的显式关闭值（off 为同义词）。
	SearchToolsDisabled = "none"
)

// searchToolPattern 约束工具类型名：小写字母、数字、下划线、点、连字符。
// 不维护类型白名单，避免在下游复制上游知识；不被支持的类型由上游显式报错。
var searchToolPattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// Config holds the resolved runtime configuration.
type Config struct {
	APIBaseURL     string        // GROK_API_URL (required) — OpenAI-compatible base, e.g. https://grok.aomanoh.tech/v1
	APIKey         string        // GROK_API_KEY (required)
	Model          string        // GROK_MODEL (required; user-supplied, never defaulted)
	RequestTimeout time.Duration // GROK_REQUEST_TIMEOUT (default 120s, clamped [5s,600s])
	Concurrency    int           // GROK_CONCURRENCY — MCP worker pool size (default 8, clamp [1,100])
	QueueSize      int           // GROK_QUEUE_SIZE — MCP request queue size (default 64, clamp [1,10000])
	// UpstreamConcurrency caps simultaneous in-flight grok2api calls across
	// all consumers and transports (GROK_UPSTREAM_CONCURRENCY; default 32,
	// clamp [1,256]; 0 disables). See DefaultUpstreamConcurrency.
	UpstreamConcurrency int
	Debug               bool // GROK_DEBUG

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

	// Optional HTTP transport for the MCP server. When HTTPAddr is set (env
	// or the `mcp --http` flag) the server serves JSON-RPC over HTTP instead
	// of stdio. HTTPAPIKey is the bearer token clients must present; it is
	// mandatory for HTTP serving (we never expose an unauthenticated network
	// endpoint) and is distinct from APIKey (the upstream grok2api key).
	HTTPAddr   string // GROK_HTTP_ADDR (e.g. ":8080"); empty = stdio
	HTTPAPIKey string // GROK_HTTP_API_KEY — required when serving over HTTP

	// MCPTools gates the advertised MCP tool surface ("core"|"all"); see
	// DefaultMCPTools. The `mcp --tools` flag overrides this.
	MCPTools string // GROK_MCP_TOOLS (default core)

	// SearchTools 是每次搜索请求在 tools 数组中声明的托管工具类型（按声明顺序、
	// 已去重）。空切片表示不声明。来源 GROK_SEARCH_TOOLS，见 DefaultSearchTools。
	SearchTools []string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	cfg := &Config{
		APIBaseURL:          strings.TrimSpace(os.Getenv("GROK_API_URL")),
		APIKey:              strings.TrimSpace(os.Getenv("GROK_API_KEY")),
		Model:               strings.TrimSpace(os.Getenv("GROK_MODEL")),
		RequestTimeout:      DefaultRequestTimeout,
		Concurrency:         DefaultConcurrency,
		QueueSize:           DefaultQueueSize,
		UpstreamConcurrency: DefaultUpstreamConcurrency,
		Debug:               parseBoolEnv("GROK_DEBUG"),
		SearchProvider:      DefaultSearchProvider,
		FetchFallback:       DefaultFetchFallback,
		TavilyAPIKey:        strings.TrimSpace(os.Getenv("TAVILY_API_KEY")),
		TavilyAPIURL:        firstNonEmpty(strings.TrimSpace(os.Getenv("TAVILY_API_URL")), DefaultTavilyURL),
		FirecrawlAPIKey:     strings.TrimSpace(os.Getenv("FIRECRAWL_API_KEY")),
		FirecrawlAPIURL:     firstNonEmpty(strings.TrimSpace(os.Getenv("FIRECRAWL_API_URL")), DefaultFirecrawlURL),
		HTTPAddr:            strings.TrimSpace(os.Getenv("GROK_HTTP_ADDR")),
		HTTPAPIKey:          strings.TrimSpace(os.Getenv("GROK_HTTP_API_KEY")),
		MCPTools:            DefaultMCPTools,
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
	if raw := strings.TrimSpace(os.Getenv("GROK_UPSTREAM_CONCURRENCY")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_UPSTREAM_CONCURRENCY (want integer): %w", err)
		}
		// <= 0 is the explicit escape hatch: disable the limiter (unlimited).
		// Any positive value is clamped into the supported range.
		if n <= 0 {
			cfg.UpstreamConcurrency = 0
		} else {
			cfg.UpstreamConcurrency = clampInt(n, 1, maxUpstreamConcurrency)
		}
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

	if raw := strings.TrimSpace(os.Getenv("GROK_MCP_TOOLS")); raw != "" {
		v, err := NormalizeToolset(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_MCP_TOOLS: %w", err)
		}
		cfg.MCPTools = v
	}

	tools, err := ParseSearchTools(os.Getenv("GROK_SEARCH_TOOLS"))
	if err != nil {
		return nil, fmt.Errorf("invalid GROK_SEARCH_TOOLS: %w", err)
	}
	cfg.SearchTools = tools

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseSearchTools 解析 GROK_SEARCH_TOOLS：逗号分隔的托管工具类型清单。
// 未设置或空白时返回 DefaultSearchTools；"none"/"off" 返回空切片（显式关闭）；
// 各项 trim 后转小写、按出现顺序去重，空项忽略；含非法字符的项 fail-loud，
// 与 GROK_SEARCH_PROVIDER 等选项一致，绝不静默丢弃配置。
func ParseSearchTools(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultSearchTools
	}
	if v := strings.ToLower(raw); v == SearchToolsDisabled || v == "off" {
		return []string{}, nil
	}
	seen := make(map[string]struct{})
	tools := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if !searchToolPattern.MatchString(name) {
			return nil, fmt.Errorf("unknown tool type %q (want comma-separated names such as web_search,x_search, or none)", strings.TrimSpace(part))
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		tools = append(tools, name)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("no tool types found in %q (use none to disable explicitly)", raw)
	}
	return tools, nil
}

// FetchTools 返回 web_fetch 的 Grok 层应声明的托管工具：仅当 GROK_SEARCH_TOOLS
// 含 web_search 时返回 ["web_search"]，否则为空。页面抓取只需要 web_search 工具组
// 里的 browse_page / open_page；x_search 等对抓取无意义，且会被 Grok Web 路由拒绝，
// 因此不透传，避免一次无关的 400 让 Grok 层白白失败。
func (c *Config) FetchTools() []string {
	for _, name := range c.SearchTools {
		if name == "web_search" {
			return []string{"web_search"}
		}
	}
	return nil
}

// NormalizeToolset validates and lower-cases a tool-set selector ("core"|
// "all"). It is shared by env loading and the `mcp --tools` flag so both reject
// an unknown value the same way (fail loud, never silently fall back).
func NormalizeToolset(raw string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "core", "all":
		return v, nil
	default:
		return "", fmt.Errorf("unknown tool set %q (want core|all)", raw)
	}
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
