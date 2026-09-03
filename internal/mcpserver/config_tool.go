package mcpserver

import (
	"context"
	"time"
)

// ConfigInfo is a static, secret-free snapshot of the running server's
// configuration, surfaced by the get_config_info tool and the
// /.well-known/mcp-config endpoint. It deliberately excludes API keys; only
// booleans report whether optional providers are enabled.
type ConfigInfo struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Protocol       string `json:"protocol_version"`
	Transport      string `json:"transport"` // "stdio" | "http"
	Model          string `json:"model"`
	BaseURL        string `json:"base_url"`
	Provider       string `json:"provider"`
	FetchFallback  string `json:"fetch_fallback"`
	RequestTimeout string `json:"request_timeout"`
	Concurrency    int    `json:"concurrency"`
	QueueSize      int    `json:"queue_size"`
	// UpstreamConcurrency is the global cap on simultaneous grok2api calls.
	// Unlike Concurrency (stdio-only request processing), this is honored in
	// both transports, so the reported value is always accurate. 0 = disabled.
	UpstreamConcurrency int    `json:"upstream_concurrency"`
	Tavily              bool   `json:"tavily_enabled"`
	Firecrawl           bool   `json:"firecrawl_enabled"`
	Toolset             string `json:"toolset"` // core | all
	// SearchTools 是搜索请求声明的托管工具类型（GROK_SEARCH_TOOLS）；空切片表示
	// 不声明。暴露它是为了让调用方能直接判断"搜索为什么没有来源"。
	SearchTools []string `json:"search_tools"`
}

// Map renders the snapshot as a JSON-friendly map (used by the well-known
// config endpoint).
func (c ConfigInfo) Map() map[string]any {
	return map[string]any{
		"name":                 c.Name,
		"version":              c.Version,
		"protocol_version":     c.Protocol,
		"transport":            c.Transport,
		"model":                c.Model,
		"base_url":             c.BaseURL,
		"provider":             c.Provider,
		"fetch_fallback":       c.FetchFallback,
		"request_timeout":      c.RequestTimeout,
		"concurrency":          c.Concurrency,
		"queue_size":           c.QueueSize,
		"upstream_concurrency": c.UpstreamConcurrency,
		"tavily_enabled":       c.Tavily,
		"firecrawl_enabled":    c.Firecrawl,
		"toolset":              c.Toolset,
		"search_tools":         c.searchToolsOrEmpty(),
		"endpoint":             "/mcp",
		"authentication":       map[string]any{"scheme": "bearer", "headers": []string{"Authorization", "X-API-Key"}},
	}
}

// GetConfigInfoTool builds the `get_config_info` tool: report the running
// server's configuration (no secrets) and, when a probe is supplied, test
// upstream reachability on demand. This mirrors the GrokSearch diagnostic of
// the same name while honoring openscry's fail-loud, secret-free conventions.
func GetConfigInfoTool(info ConfigInfo, probe func(ctx context.Context) error) (Tool, ToolHandler) {
	def := Tool{
		Name: "get_config_info",
		Description: "Report the running openscry server's configuration (model, base URL, provider, " +
			"fetch fallback policy, enabled extractor providers, transport) and test upstream " +
			"reachability. No API keys are ever returned. Use this to debug connectivity or verify " +
			"which model/provider is active.",
		InputSchema: InputSchema{
			Type:                 "object",
			Properties:           map[string]Property{},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, _ map[string]any) (*ToolCallResult, error) {
		out := map[string]any{"config": info.Map()}
		if probe != nil {
			start := time.Now()
			err := probe(ctx)
			conn := map[string]any{
				"reachable":  err == nil,
				"latency_ms": time.Since(start).Milliseconds(),
			}
			if err != nil {
				conn["error"] = err.Error()
			}
			out["connectivity"] = conn
		}
		return jsonResult(out)
	}
	return def, handler
}

// searchToolsOrEmpty 保证 JSON 输出为 [] 而不是 null，便于调用方直接判空。
func (c ConfigInfo) searchToolsOrEmpty() []string {
	if c.SearchTools == nil {
		return []string{}
	}
	return c.SearchTools
}
