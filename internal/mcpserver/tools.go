package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/search"
)

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
					Description: "Optional model override for this call. Defaults to the configured model.",
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
			Query:    query,
			Platform: platform,
			Model:    model,
		})
		if err != nil {
			return nil, err
		}
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: res.Content}},
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
			"On total failure the tiers tried are listed.",
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
		if to, _ := args["timeout"].(string); strings.TrimSpace(to) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(to))
			if err != nil {
				return nil, fmt.Errorf("web_fetch: invalid timeout %q (want a Go duration like 30s): %w", to, err)
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		res, err := svc.Fetch(ctx, rawURL)
		if err != nil {
			return nil, err
		}
		// Prefix a non-rendering HTML comment so the consumer can see which
		// tier produced the content (e.g. low-fidelity "http" vs "grok").
		header := fmt.Sprintf("<!-- openscry web_fetch: tier=%s url=%s -->\n", res.Tier, res.URL)
		return &ToolCallResult{
			Content: []ContentItem{{Type: "text", Text: header + res.Content}},
		}, nil
	}

	return def, handler
}
