package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/resilience"
)

// Service generates structured research plans via the configured grok model.
// It is safe for concurrent use.
type Service struct {
	client   *grok.Client
	model    string
	profiles resilience.Profiles
}

// Options configures the planner service.
type Options struct {
	Model    string
	Profiles resilience.Profiles
}

// New builds a planner Service.
func New(client *grok.Client, opt Options) *Service {
	return &Service{
		client:   client,
		model:    strings.TrimSpace(opt.Model),
		profiles: opt.Profiles.Normalize(),
	}
}

// Plan generates a structured research plan for the given question. It calls
// the grok model with PlanPrompt and parses the returned JSON into a Plan.
// Errors are fail-loud: a model failure or unparseable response surfaces
// explicitly rather than returning a partial/empty plan.
func (s *Service) Plan(ctx context.Context, question string) (*Plan, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("planner: question must not be empty")
	}

	// Apply OpSearch timeout (planning is a search-class operation in terms
	// of latency expectations) when caller has no deadline.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.profiles.For(resilience.OpSearch))
		defer cancel()
	}

	raw, err := s.client.Complete(ctx, s.model, PlanPrompt, question)
	if err != nil {
		return nil, fmt.Errorf("planner: model call failed: %w", err)
	}

	plan, err := parsePlan(raw)
	if err != nil {
		return nil, fmt.Errorf("planner: failed to parse model response as plan: %w\nraw response: %.500s", err, raw)
	}

	if err := validatePlan(plan); err != nil {
		return nil, fmt.Errorf("planner: plan validation failed: %w", err)
	}

	return plan, nil
}

// parsePlan extracts a Plan from the model's raw response. It handles common
// model quirks: markdown code fences wrapping the JSON, leading/trailing
// whitespace, and BOM characters.
func parsePlan(raw string) (*Plan, error) {
	cleaned := strings.TrimSpace(raw)

	// Strip markdown JSON code fences if the model wrapped the output.
	if strings.HasPrefix(cleaned, "```") {
		// Remove opening fence (```json or ```)
		if idx := strings.Index(cleaned, "\n"); idx >= 0 {
			cleaned = cleaned[idx+1:]
		}
		// Remove closing fence
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}

	var plan Plan
	if err := json.Unmarshal([]byte(cleaned), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// validatePlan performs basic structural validation on a parsed plan.
func validatePlan(p *Plan) error {
	if p.Intent.CoreQuestion == "" {
		return fmt.Errorf("intent.core_question is empty")
	}
	if p.Complexity.Level < 1 || p.Complexity.Level > 3 {
		return fmt.Errorf("complexity.level must be 1-3, got %d", p.Complexity.Level)
	}
	if len(p.SubQueries) == 0 {
		return fmt.Errorf("sub_queries must not be empty")
	}
	for i, sq := range p.SubQueries {
		if sq.ID == "" {
			return fmt.Errorf("sub_queries[%d].id is empty", i)
		}
		if sq.Goal == "" {
			return fmt.Errorf("sub_queries[%d].goal is empty", i)
		}
	}
	if len(p.SearchTerms) == 0 {
		return fmt.Errorf("search_terms must not be empty")
	}
	return nil
}
