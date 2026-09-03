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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
	"github.com/AoManoh/openscry/internal/refsource"
	"github.com/AoManoh/openscry/internal/resilience"
	"github.com/AoManoh/openscry/internal/sources"
)

// Service performs web searches against a grok2api endpoint. It owns the
// process-wide resilience state (circuit breaker + retry budget) so that
// concurrent calls — e.g. an MCP worker pool servicing many tools/call —
// share one breaker and one retry budget rather than amplifying load.
type Service struct {
	client       *grok.Client
	defaultModel string
	provider     string // resolved provider mode: "chat" (default) or "responses"
	profiles     resilience.Profiles
	breaker      *resilience.Breaker
	retryOpts    resilience.RetryOptions
	refProvider  *refsource.Provider // optional extra_sources fan-out; nil disables it
	// tools 是每次搜索请求声明的托管工具类型（来自 GROK_SEARCH_TOOLS）。空表示
	// 不声明，请求形态与历史版本一致。
	tools []string
}

// Options tunes the search service's resilience layer. The zero value is
// valid and yields sensible defaults.
type Options struct {
	Profiles                resilience.Profiles
	Breaker                 resilience.BreakerConfig
	MaxAttempts             int                 // total attempts incl. first (default 3)
	RetryBaseDelay          time.Duration       // first backoff delay (default 500ms)
	RetryBudgetBurst        int                 // shared retry-budget burst (default 8)
	RetryBudgetRefillPerSec float64             // budget refill rate (default 2/s)
	Provider                string              // resolved provider mode ("chat"|"responses"); empty = chat
	RefProvider             *refsource.Provider // optional extra_sources reference fan-out
	// Tools 是搜索请求声明的托管工具类型，例如 []string{"web_search","x_search"}；
	// 通常直接传入 config.Config.SearchTools。空表示不声明。
	Tools []string
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
	provider := strings.TrimSpace(opt.Provider)
	if provider == "" {
		provider = "chat"
	}
	return &Service{
		client:       client,
		defaultModel: defaultModel,
		provider:     provider,
		refProvider:  opt.RefProvider,
		tools:        append([]string(nil), opt.Tools...),
		profiles:     opt.Profiles.Normalize(),
		breaker:      resilience.NewBreaker(opt.Breaker),
		retryOpts: resilience.RetryOptions{
			MaxAttempts: maxAttempts,
			BaseDelay:   baseDelay,
			Jitter:      true,
			IsRetryable: isRetryable,
			RetryAfter:  retryAfterHint,
			Budget:      resilience.NewRetryBudget(burst, refill),
		},
	}
}

// Request is a single search request.
type Request struct {
	Query    string
	Platform string // optional platform focus (e.g. "GitHub", "Reddit")
	Model    string // optional per-call model override
	// ExtraSources, when > 0, requests that many additional reference
	// sources from Tavily/Firecrawl search (see internal/refsource), fetched
	// concurrently with the Grok answer and merged into Result.Sources. It is
	// a no-op when no RefProvider is configured.
	ExtraSources int
}

// Result is a successful search result. Content is the answer with any
// trailing sources block removed; Sources is the normalized citation list
// extracted from the upstream answer (see internal/sources). Sources is nil
// when the answer carried no recognizable citations.
type Result struct {
	Content string
	Model   string
	Sources []sources.Source
	// Warning is non-empty when a non-fatal degradation occurred — e.g. the
	// Grok answer succeeded but one or more extra_sources tiers failed. The
	// primary result is still valid; the warning makes the gap visible.
	Warning string
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
	// Provider seam (fail-loud): the responses path is not implemented yet.
	// It must not silently fall back to chat; the operator gets an explicit
	// error until annotation-passthrough quality is verified (decision 二c).
	if s.provider == "responses" {
		return nil, fmt.Errorf("search: GROK_SEARCH_PROVIDER=responses is not implemented yet — use chat (default) or unset it; the responses path needs annotation-passthrough verification first")
	}
	// Model selection: an explicit per-call override wins; otherwise use the
	// service's configured model (the user-supplied GROK_MODEL). There is no
	// code default and no fallback to another model on failure -- an
	// unavailable model surfaces as an explicit grok error (fail-loud).
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.defaultModel
	}
	userContent := buildUserContent(query, req.Platform)

	opCtx, cancel := context.WithTimeout(ctx, s.profiles.For(resilience.OpSearch))
	defer cancel()

	// Extra reference sources (Tavily/Firecrawl search) run concurrently with
	// the Grok answer so they overlap the dominant streaming latency. They are
	// strictly augmentation: a refsource failure never fails the search, and
	// the results are only attached when the Grok answer succeeds.
	useRef := req.ExtraSources > 0 && s.refProvider != nil && s.refProvider.Available()
	var (
		refSrc      []sources.Source
		refFailures []refsource.Failure
		refWG       sync.WaitGroup
	)
	if useRef {
		refWG.Add(1)
		go func() {
			defer refWG.Done()
			refSrc, refFailures = s.refProvider.Search(opCtx, query, req.ExtraSources)
		}()
	}

	completion, err := resilience.Retry(opCtx, s.retryOpts, func(c context.Context) (*grok.Completion, error) {
		var out *grok.Completion
		gerr := s.breaker.Guard(func() error {
			var e error
			out, e = s.client.CompleteDetailed(c, model, prompt.SearchPrompt, userContent, s.tools)
			return e
		}, isBreakerFailure)
		return out, gerr
	})
	if useRef {
		refWG.Wait() // join before returning so the goroutine never outlives the call
	}
	if err != nil {
		return nil, err
	}
	// Separate the answer from its citations and normalize heterogeneous
	// upstream source formats into one consistent list. When no citations are
	// present, answer == content and Sources is nil (no behavior change).
	answer, grokSrc := sources.Split(completion.Content)
	// 流中的 url_citation 注解是上游对来源的权威记录：即使模型正文省略了
	// [[n]](url) 标记（例如用户要求"只回答日期"），也能据此补齐来源列表。
	annotated := make([]sources.Source, 0, len(completion.Citations))
	for _, c := range completion.Citations {
		annotated = append(annotated, sources.Source{Title: c.Title, URL: c.URL})
	}
	result := &Result{Content: answer, Model: model, Sources: sources.Merge(grokSrc, annotated, refSrc)}
	var warnings []string
	if len(refFailures) > 0 {
		warnings = append(warnings, formatRefFailures(refFailures))
	}
	// 一次"搜索"却没有任何可解析来源时，把它标为可见降级（参照 GrokSearch-rs
	// 的口径）：结果仍然返回，但调用方应把时效性结论视为未经核验。usage 里的
	// 服务端工具计数能区分两种情形：检索根本没发生（工具未声明/未调用/上游不
	// 支持），或检索发生了但模型正文没有落引用。
	if len(result.Sources) == 0 {
		switch {
		case completion.ServerToolCallsKnown && completion.ServerToolCalls > 0:
			warnings = append(warnings, fmt.Sprintf(UncitedSearchWarningFmt, completion.ServerToolCalls))
		default:
			warnings = append(warnings, NoSourcesWarning)
		}
	}
	result.Warning = strings.Join(warnings, "; ")
	return result, nil
}

// NoSourcesWarning 是答案不含任何可解析引用、且上游未报告任何服务端工具调用
// 时附加的可见降级提示。
const NoSourcesWarning = "answer carries no source citations and no server-side search tool call was reported; the model likely answered from training knowledge, treat time-sensitive claims as unverified"

// UncitedSearchWarningFmt 用于"检索已执行（%d 次服务端工具调用）但正文没有可
// 解析引用"的情形，提示调用方结论有检索支撑但无法逐条溯源。
const UncitedSearchWarningFmt = "search ran (%d server-side tool calls) but the answer contains no parsable citations; sources cannot be traced per claim"

// formatRefFailures renders non-fatal extra_sources tier failures into one
// visible warning line, deduplicating provider names.
func formatRefFailures(failures []refsource.Failure) string {
	seen := make(map[string]struct{})
	var names []string
	for _, f := range failures {
		if _, dup := seen[f.Provider]; dup {
			continue
		}
		seen[f.Provider] = struct{}{}
		names = append(names, f.Provider)
	}
	sort.Strings(names)
	return fmt.Sprintf("extra_sources requested but %s search failed; the Grok answer is unaffected", strings.Join(names, "/"))
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

// retryAfterHint extracts a grok 429/503 Retry-After hint so the resilience
// layer can honor the upstream's requested delay instead of blind backoff.
// Returns (0, false) when the error carries no hint.
func retryAfterHint(err error) (time.Duration, bool) {
	if ge, ok := grok.AsError(err); ok && ge.RetryAfter > 0 {
		return ge.RetryAfter, true
	}
	return 0, false
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
//
// Known limitation (accepted, CR-003): the breaker is process-wide and a
// persistent upstream 5xx counts as a failure regardless of which model
// produced it. Under the single-model norm this is correct (it sheds load
// from a broken upstream); only when callers mix models via per-call
// overrides could a 5xx-ing model trip the shared breaker for healthy
// models. Keying the breaker per model is deferred; see
// docs/code-review/2026-05-31-cr-openscry-s1-s3.md (CR-003).
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
