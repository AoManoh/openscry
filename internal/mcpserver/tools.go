package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/mapper"
	"github.com/AoManoh/openscry/internal/planner"
	"github.com/AoManoh/openscry/internal/search"
)

// formatCompletion 渲染 "<state> (<detail>)"；detail 为空时只输出状态，避免出现空括号。
func formatCompletion(state grok.CompletionState, detail string) string {
	if detail == "" {
		return string(state)
	}
	return fmt.Sprintf("%s (%s)", state, detail)
}

// WebSearchTool builds the `web_search` tool definition and a handler bound
// to the given search service. This is the single tool exposed in the stage
// S1 minimal closed loop.
func WebSearchTool(svc *search.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "web_search",
		Description: "Perform a deep web search via grok2api and return the model's answer. " +
			"Provide a clear, self-contained natural-language query. The model and provider " +
			"are fixed by configuration; an unavailable model fails explicitly rather than " +
			"silently switching to a different one.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"query": {
					Type:        "string",
					Description: "Clear, self-contained natural-language search query.",
				},
				"platform": {
					Type:        "string",
					Description: "Optional platform to focus on (e.g. GitHub, Reddit). Leave empty for general web search.",
				},
				"model": {
					Type:        "string",
					Description: "Optional per-call model override. Defaults to the server's configured GROK_MODEL (user-supplied; there is no built-in default, and an unavailable model fails explicitly).",
				},
				"extra_sources": {
					Type:        "integer",
					Description: "Number of additional reference sources to fetch from Tavily/Firecrawl search, merged into the answer's citations. 0 (default) disables it; a no-op when no Tavily/Firecrawl key is configured.",
				},
			},
			Required:             []string{"query"},
			AdditionalProperties: false,
		},
	}

	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		query, _ := args["query"].(string)
		platform, _ := args["platform"].(string)
		model, _ := args["model"].(string)

		res, err := svc.Search(ctx, search.Request{
			Query:        query,
			Platform:     platform,
			Model:        model,
			ExtraSources: parseIntArg(args, "extra_sources", 0),
		})
		if err != nil {
			return nil, err
		}
		// Append a normalized Sources section so the agent gets a consistent
		// citation list regardless of how the upstream formatted them. When
		// the answer carried no citations the text is returned unchanged.
		text := res.Content
		if res.Warning != "" {
			text += "\n\n> warning: " + res.Warning
		}
		if len(res.Sources) > 0 {
			var b strings.Builder
			b.WriteString(text)
			b.WriteString(fmt.Sprintf("\n\n---\n**Sources (%d):**\n", len(res.Sources)))
			for i, s := range res.Sources {
				if s.Title != "" {
					b.WriteString(fmt.Sprintf("%d. [%s](%s)", i+1, s.Title, s.URL))
				} else {
					b.WriteString(fmt.Sprintf("%d. %s", i+1, s.URL))
				}
				// extra_sources 补充的来源标注出处，与模型自身引用区分开
				if s.Origin != "" {
					b.WriteString(" — via " + s.Origin)
				}
				b.WriteString("\n")
			}
			text = b.String()
		}
		// 追加模型标识，闭合 fail-loud 可观测性环：agent 可验证实际服务的模型
		if res.Model != "" {
			text += "\n\n> model: " + res.Model
		}
		// 流未确认完整时单独一行披露状态与原因；complete 时不输出，避免改变正常结果的形态。
		// 正文里已经以 > warning: 提示过，这里再给出机器可读的一行，便于 agent 按状态分支。
		if res.CompletionState != grok.StateComplete {
			text += "\n> completion: " + formatCompletion(res.CompletionState, res.CompletionDetail)
		}
		// 服务端工具调用次数与耗时：让 agent 与评测无需旁路手段即可判断检索是否发生、花了多久
		if res.ServerToolCallsKnown {
			text += fmt.Sprintf("\n> tools: %d server-side calls", res.ServerToolCalls)
		}
		text += fmt.Sprintf("\n> elapsed: %.1fs", res.Elapsed.Seconds())
		// extra_sources 结算：要了几条、实际新增几条、几条与模型引用重复、哪家失败
		if r := res.ExtraSources; r != nil {
			text += fmt.Sprintf("\n> extra_sources: requested %d, added %d, %d duplicated model sources", r.Requested, r.Added, r.Duplicates)
			if len(r.Failed) > 0 {
				text += fmt.Sprintf(", failed: %s", strings.Join(r.Failed, ","))
			}
		}
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: text}},
		}, nil
	}

	return def, handler
}

// WebFetchTool builds the `web_fetch` tool: retrieve a URL's content as
// Markdown via the multi-tier fallback chain (Tavily → Firecrawl → Grok →
// basic HTTP). Degradation is visible — the winning tier is noted in the
// output, and a total failure reports the tiers tried.
func WebFetchTool(svc *fetch.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "web_fetch",
		Description: "Fetch a URL and return its content as structured Markdown. Tries multiple " +
			"extractors in order (Tavily, Firecrawl, Grok, then a basic HTTP fallback) under a " +
			"single timeout budget; the first non-empty result wins and the tier used is reported. " +
			"On total failure the tiers tried are listed. When GROK_FETCH_FALLBACK=strict, the " +
			"basic-HTTP fallback is disabled and a failed extractor/model fails loud.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"url": {
					Type:        "string",
					Description: "Absolute http/https URL of the page to fetch.",
				},
				"timeout": {
					Type:        "string",
					Description: "Optional total fetch budget as a Go duration (e.g. \"30s\", \"2m\"). Empty uses the configured fetch timeout.",
				},
			},
			Required:             []string{"url"},
			AdditionalProperties: false,
		},
	}

	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		rawURL, _ := args["url"].(string)
		// timeout 作为显式预算参数交给服务层，而不是包装成请求上下文的截止时间：服务层
		// 要能区分"调用方要求的预算"与"传输层请求上限"，前者才应替代默认档位。
		var budget time.Duration
		if to, _ := args["timeout"].(string); strings.TrimSpace(to) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(to))
			if err != nil {
				return nil, fmt.Errorf("web_fetch: invalid timeout %q (want a Go duration like 30s): %w", to, err)
			}
			budget = d
		}
		res, err := svc.FetchWithTimeout(ctx, rawURL, budget)
		if err != nil {
			return nil, err
		}
		// Prefix a non-rendering HTML comment so the consumer can see which
		// tier produced the content (e.g. low-fidelity "http" vs "grok").
		header := fmt.Sprintf("<!-- openscry web_fetch: tier=%s url=%s -->\n", res.Tier, res.URL)
		if res.FetchedURL != "" && res.FetchedURL != res.URL {
			header = fmt.Sprintf("<!-- openscry web_fetch: tier=%s url=%s fetched=%s -->\n", res.Tier, res.URL, res.FetchedURL)
		}
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: header + res.Content}},
		}, nil
	}

	return def, handler
}

// WebMapTool builds the `web_map` tool: discover a website's URL structure
// by graph traversal. Uses Tavily /map when configured, otherwise pure HTTP
// BFS crawl. Best-effort under the OpMap timeout budget.
func WebMapTool(svc *mapper.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "web_map",
		Description: "Map a website's structure by traversing it as a graph, discovering URLs. " +
			"Uses Tavily /map (when TAVILY_API_KEY is set) for high-quality results including " +
			"JS-rendered pages, otherwise falls back to pure HTTP crawl. Returns discovered URLs " +
			"as a JSON list. Use max_depth, max_breadth, and limit to control traversal scope.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"url": {
					Type:        "string",
					Description: "Root URL to begin the mapping (absolute http/https).",
				},
				"max_depth": {
					Type:        "string",
					Description: "Maximum traversal depth from the root URL (default 1, max 5).",
				},
				"max_breadth": {
					Type:        "string",
					Description: "Maximum links to follow per page (default 20, max 500).",
				},
				"limit": {
					Type:        "string",
					Description: "Total number of URLs to discover before stopping (default 50, max 500).",
				},
				"instructions": {
					Type:        "string",
					Description: "Optional natural-language instructions to filter or focus the crawl (e.g. \"only documentation pages\").",
				},
				"timeout": {
					Type:        "string",
					Description: "Optional total timeout as a Go duration (e.g. \"60s\"). Empty uses the configured OpMap timeout (90s).",
				},
			},
			Required:             []string{"url"},
			AdditionalProperties: false,
		},
	}

	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		rawURL, _ := args["url"].(string)
		// timeout 作为显式预算参数交给服务层（见 web_fetch 的说明）。
		var budget time.Duration
		if to, _ := args["timeout"].(string); strings.TrimSpace(to) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(to))
			if err != nil {
				return nil, fmt.Errorf("web_map: invalid timeout %q (want a Go duration like 60s): %w", to, err)
			}
			budget = d
		}

		req := mapper.Request{
			URL:          rawURL,
			MaxDepth:     parseIntArg(args, "max_depth", 1),
			MaxBreadth:   parseIntArg(args, "max_breadth", 20),
			Limit:        parseIntArg(args, "limit", 50),
			Instructions: stringArg(args, "instructions"),
			Timeout:      budget,
		}

		res, err := svc.Map(ctx, req)
		if err != nil {
			return nil, err
		}

		result := map[string]any{
			"root_url": res.RootURL,
			"tier":     res.Tier,
			"count":    len(res.URLs),
			"urls":     res.URLs,
		}
		if res.Warning != "" {
			result["warning"] = res.Warning
		}
		out, _ := json.Marshal(result)
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: string(out)}},
		}, nil
	}

	return def, handler
}

// parseIntArg extracts a numeric argument from the MCP args map. JSON numbers
// may arrive as float64 or string; this handles both gracefully.
func parseIntArg(args map[string]any, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		if int(n) > 0 {
			return int(n)
		}
	case string:
		if n != "" {
			var i int
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil && i > 0 {
				return i
			}
		}
	}
	return fallback
}

// stringArg extracts a trimmed string argument.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// ResearchPlanTool builds the `research_plan` tool: generate a structured
// offline research plan for a given question. The model produces a JSON plan
// containing intent, complexity, sub-queries, search terms, execution order,
// and research quality strategies (fetch_before_claim, gap_check).
func ResearchPlanTool(svc *planner.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "research_plan",
		Description: "Generate a structured offline research plan for a question. The model " +
			"analyses the question and produces a JSON plan with intent analysis, complexity " +
			"assessment, decomposed sub-queries, concrete search terms, execution order, and " +
			"research quality strategies (fetch_before_claim, gap_check). Use this before " +
			"executing a complex multi-step search to organize the approach.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"question": {
					Type:        "string",
					Description: "The research question to plan for. Be specific and include relevant context.",
				},
				"timeout": {
					Type:        "string",
					Description: "Optional timeout as a Go duration (e.g. \"60s\"). Empty uses the configured search timeout.",
				},
			},
			Required:             []string{"question"},
			AdditionalProperties: false,
		},
	}

	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		question, _ := args["question"].(string)
		// timeout 作为显式预算参数交给服务层（见 web_fetch 的说明）。
		var budget time.Duration
		if to, _ := args["timeout"].(string); strings.TrimSpace(to) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(to))
			if err != nil {
				return nil, fmt.Errorf("research_plan: invalid timeout %q: %w", to, err)
			}
			budget = d
		}

		started := time.Now()
		plan, err := svc.PlanWithTimeout(ctx, question, budget)
		if err != nil {
			return nil, err
		}

		// 与搜索结果一样暴露耗时，评测与调用方无需旁路计时
		out, _ := json.Marshal(struct {
			*planner.Plan
			ElapsedS float64 `json:"elapsed_s"`
		}{plan, roundSeconds(time.Since(started))})
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: string(out)}},
		}, nil
	}

	return def, handler
}
