package mcpserver

import (
	"context"

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
