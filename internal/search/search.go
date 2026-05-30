// Package search is the openscry search core. It builds the prompt-guided
// search request and delegates to the grok client through the resilience
// layer (per-operation timeout + bounded retry with a shared budget +
// circuit breaker). It is transport-agnostic so both the CLI
// (`openscry search`) and the thin MCP adapter call the same code path.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
	"github.com/AoManoh/openscry/internal/resilience"
)

// Service performs web searches against a grok2api endpoint. It owns the
// process-wide resilience state (circuit breaker + retry budget) so that
// concurrent calls — e.g. an MCP worker pool servicing many tools/call —
// share one breaker and one retry budget rather than amplifying load.
type Service struct {
	client       *grok.Client
	defaultModel string
	profiles     resilience.Profiles
	breaker      *resilience.Breaker
	retryOpts    resilience.RetryOptions
}

// Options tunes the search service's resilience layer. The zero value is
// valid and yields sensible defaults.
type Options struct {
	Profiles                resilience.Profiles
	Breaker                 resilience.BreakerConfig
	MaxAttempts             int           // total attempts incl. first (default 3)
	RetryBaseDelay          time.Duration // first backoff delay (default 500ms)
	RetryBudgetBurst        int           // shared retry-budget burst (default 8)
	RetryBudgetRefillPerSec float64       // budget refill rate (default 2/s)
}

// New constructs a Service with default resilience settings.
func New(client *grok.Client, defaultModel string) *Service {
	return NewWithOptions(client, defaultModel, Options{})
}

// NewWithOptions constructs a Service with explicit resilience tuning.
// Zero-valued fields fall back to defaults.
func NewWithOptions(client *grok.Client, defaultModel string, opt Options) *Service {
	maxAttempts := opt.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 3
	}
	burst := opt.RetryBudgetBurst
	if burst < 1 {
		burst = 8
	}
	refill := opt.RetryBudgetRefillPerSec
	if refill <= 0 {
		refill = 2
	}
	baseDelay := opt.RetryBaseDelay
	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	return &Service{
		client:       client,
		defaultModel: defaultModel,
		profiles:     opt.Profiles.Normalize(),
		breaker:      resilience.NewBreaker(opt.Breaker),
		retryOpts: resilience.RetryOptions{
			MaxAttempts: maxAttempts,
			BaseDelay:   baseDelay,
			Jitter:      true,
			IsRetryable: isRetryable,
			Budget:      resilience.NewRetryBudget(burst, refill),
		},
	}
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

// Search executes one web search through the resilience layer. The model is
// never silently swapped; transport failures are retried (bounded by the
// shared budget) and count against the circuit breaker, while a model error
// or an empty result is surfaced explicitly rather than as a hollow success.
//
// The per-operation search timeout bounds the whole attempt sequence (not
// each attempt), so retries cannot extend total latency beyond the profile.
func (s *Service) Search(ctx context.Context, req Request) (*Result, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, fmt.Errorf("search: query must not be empty")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.defaultModel
	}
	userContent := buildUserContent(query, req.Platform)

	opCtx, cancel := context.WithTimeout(ctx, s.profiles.For(resilience.OpSearch))
	defer cancel()

	content, err := resilience.Retry(opCtx, s.retryOpts, func(c context.Context) (string, error) {
		var out string
		gerr := s.breaker.Guard(func() error {
			var e error
			out, e = s.client.ChatSearch(c, model, prompt.SearchPrompt, userContent)
			return e
		}, isBreakerFailure)
		return out, gerr
	})
	if err != nil {
		return nil, err
	}
	return &Result{Content: content, Model: model}, nil
}

func buildUserContent(query, platform string) string {
	var b strings.Builder
	b.WriteString(prompt.TimeContext())
	b.WriteString("\n")
	b.WriteString(query)
	if p := strings.TrimSpace(platform); p != "" {
		b.WriteString("\n\nYou should search the web for the information you need, and focus on this platform: ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// isRetryable reports whether err warrants a retry. A circuit-open error is
// never retried (fail fast); otherwise the grok client's own retryable
// classification governs (timeout / connect / 5xx / rate limit / empty).
func isRetryable(err error) bool {
	if errors.Is(err, resilience.ErrCircuitOpen) {
		return false
	}
	if ge, ok := grok.AsError(err); ok {
		return ge.Retryable
	}
	return false
}

// isBreakerFailure reports whether err should count against the circuit
// breaker. Only transport-health failures count: timeout, connection, rate
// limit and 5xx. A model error, empty result, auth error or client
// cancellation is NOT an upstream-health signal and must not trip the
// breaker — otherwise a single bad model name could blackhole all traffic.
func isBreakerFailure(err error) bool {
	if errors.Is(err, resilience.ErrCircuitOpen) {
		return false
	}
	ge, ok := grok.AsError(err)
	if !ok {
		return false
	}
	switch ge.Code {
	case grok.CodeTimeout, grok.CodeConnect, grok.CodeRateLimit:
		return true
	case grok.CodeUpstreamStatus:
		return ge.Status >= 500
	default:
		return false
	}
}
