// Package search is the openscry search core. It builds the prompt-guided
// search request and delegates to the grok client. It is transport-agnostic
// so both the CLI (`openscry search`) and the thin MCP adapter call the same
// code path.
package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
)

// Service performs web searches against a grok2api endpoint.
type Service struct {
	client       *grok.Client
	defaultModel string
}

// New constructs a Service. defaultModel is used when a request does not
// specify an explicit model override.
func New(client *grok.Client, defaultModel string) *Service {
	return &Service{client: client, defaultModel: defaultModel}
}

// Request is a single search request.
type Request struct {
	Query    string
	Platform string // optional platform focus (e.g. "GitHub", "Reddit")
	Model    string // optional per-call model override
}

// Result is a successful search result.
type Result struct {
	Content string
	Model   string
}

// Search executes one web search. On any upstream failure it returns the
// structured error as-is: the model is never silently swapped, and an empty
// result is reported as an explicit error rather than a hollow success.
func (s *Service) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search: query must not be empty")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.defaultModel
	}

	var b strings.Builder
	b.WriteString(prompt.TimeContext())
	b.WriteString("\n")
	b.WriteString(query)
	if platform := strings.TrimSpace(req.Platform); platform != "" {
		b.WriteString("\n\nYou should search the web for the information you need, and focus on this platform: ")
		b.WriteString(platform)
		b.WriteString("\n")
	}

	content, err := s.client.ChatSearch(ctx, model, prompt.SearchPrompt, b.String())
	if err != nil {
		return nil, err
	}
	return &Result{Content: content, Model: model}, nil
}
