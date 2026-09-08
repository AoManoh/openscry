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
	"regexp"
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
	// Timeout 是调用方的显式预算（CLI --timeout / MCP timeout 参数），0 表示使用服务的
	// 搜索档位。预算以参数而不是父上下文截止时间传递，是为了让服务层能区分"调用方要
	// 求的预算"与"传输层的请求上限"：后者只应收紧预算，不应替代默认预算。
	Timeout time.Duration
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
	// ServerToolCalls 是上游报告的服务端托管工具调用次数（web_search 等）；
	// ServerToolCallsKnown 为 false 表示上游未提供该字段（如 Grok Web 路由）。
	// 暴露它是为了让调用方与评测能直接判断"检索是否真的发生"。
	ServerToolCalls      int
	ServerToolCallsKnown bool
	// Elapsed 是本次搜索（含重试）的端到端耗时，由服务层计时，调用方无需再括号计时。
	Elapsed time.Duration
	// ExtraSources 记录 extra_sources 参考检索的实际效果（请求数、新增数、与模型引用重复数、
	// 按提供方分布）；未请求时为 nil。它让调用方能判断"要了 3 条为什么只多了 1 条"。
	ExtraSources *ExtraSourcesReport
	// CompletionState 是上游流的完整性判定（complete / truncated / filtered / unconfirmed），
	// CompletionDetail 是非 complete 时的一句话原因。正文不因不完整而丢弃：已生成的部分
	// 对调用方仍有价值，但调用方必须把未送达的部分当作未知，而不是当作"答案到此为止"。
	CompletionState  grok.CompletionState
	CompletionDetail string
}

// ExtraSourcesReport 是 extra_sources 的结算单。
type ExtraSourcesReport struct {
	Requested  int            `json:"requested"`
	Added      int            `json:"added"`
	Duplicates int            `json:"duplicates_of_model_sources"`
	ByOrigin   map[string]int `json:"by_origin,omitempty"`
	Failed     []string       `json:"failed_providers,omitempty"`
}

// Search executes one web search through the resilience layer. The model is
// never silently swapped; transport failures are retried (bounded by the
// shared budget) and count against the circuit breaker, while a model error
// or an empty result is surfaced explicitly rather than as a hollow success.
//
// The per-operation search timeout bounds the whole attempt sequence (not
// each attempt), so retries cannot extend total latency beyond the profile.
//
// 预算语义：req.Timeout > 0 时用它，否则用搜索档位；两者之一总会作为操作上下文的
// 截止时间生效。父上下文自带的更早截止时间（例如 MCP HTTP 传输的请求上限）由
// context 自动保留，因此传输层上限只会收紧预算，不会让默认预算失效。
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

	budget := req.Timeout
	if budget <= 0 {
		budget = s.profiles.For(resilience.OpSearch)
	}
	opCtx, cancel := context.WithTimeout(ctx, budget)
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

	started := time.Now()
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
	// 上游 url_citation 的 title 常常只是引用序号（"1"、"2"），不是页面标题，写进
	// Title 会被渲染成 [2](url) 这样的畸形条目，因此纯数字 title 一律丢弃。
	annotated := make([]sources.Source, 0, len(completion.Citations))
	for _, c := range completion.Citations {
		title := strings.TrimSpace(c.Title)
		if numericTitle.MatchString(title) {
			title = ""
		}
		annotated = append(annotated, sources.Source{Title: title, URL: c.URL})
	}
	merged := sources.Merge(grokSrc, annotated, refSrc)
	// 让正文里的 [[n]](url) 编号与最终 Sources 列表的序号一致，下游才能按编号回溯来源；
	// 模型偶尔把同一引用连写两遍（[[3]](u)[[3]](u)），折叠为一个。
	answer = collapseRepeatedCitations(renumberInlineCitations(answer, merged))
	result := &Result{Content: answer, Model: model, Sources: merged, Elapsed: time.Since(started),
		ServerToolCalls: completion.ServerToolCalls, ServerToolCallsKnown: completion.ServerToolCallsKnown,
		CompletionState: completion.State, CompletionDetail: completion.Detail}
	if useRef {
		report := &ExtraSourcesReport{Requested: req.ExtraSources, ByOrigin: map[string]int{}}
		for _, src := range merged {
			if src.Origin != "" {
				report.Added++
				report.ByOrigin[src.Origin]++
			}
		}
		report.Duplicates = len(refSrc) - report.Added
		for _, f := range refFailures {
			report.Failed = append(report.Failed, f.Provider)
		}
		result.ExtraSources = report
	}
	var warnings []string
	// 流未确认完整（截断 / 过滤 / 未收到 [DONE]）时只披露、不重试、不丢弃：
	//  - length 是模型达到输出上限，同样的请求再发一次仍会在同一处截断，重试没有意义；
	//  - 未收到 [DONE] 的 EOF 多半是上游连接中途关闭，此时正文已经生成并计费，重试会把
	//    上游成本加倍，而且新一次响应同样可能残缺；
	//  - 已生成的部分正文对调用方仍有价值，丢弃会把可用信息藏起来。
	// 因此完整性走 Result 字段 + 告警，而不是走 error（走 error 会被 resilience.Retry 当作
	// 失败重试）。告警放在最前面，因为它改变对后面所有内容的解读方式。
	if completion.State != grok.StateComplete {
		warnings = append(warnings, fmt.Sprintf(IncompleteResponseWarningFmt, completion.State, completion.Detail))
	}
	if len(refFailures) > 0 {
		warnings = append(warnings, formatRefFailures(refFailures))
	}
	// 一次"搜索"却没有任何可解析来源时，把它标为可见降级（参照 GrokSearch-rs
	// 的口径）：结果仍然返回，但调用方应把时效性结论视为未经核验。usage 里的
	// 服务端工具计数能区分两种情形：检索根本没发生（工具未声明/未调用/上游不
	// 支持），或检索发生了但模型正文没有落引用。
	//
	// 判定只数模型自身的来源（正文解析出的引用 + 上游 url_citation 注解，Origin 为空），
	// extra_sources 补充的条目（Origin 非空）不计入：补充来源是与答案并行检索出的阅读
	// 候选，模型作答时并未看到它们，不能充当该答案的证据。若按合并后的总数判定，同一个
	// 无引用答案会因为传了 extra_sources 而失去告警，把"答案无据可查"藏在一个看起来
	// 完整的来源列表后面。
	modelSources := 0
	for _, src := range merged {
		if src.Origin == "" {
			modelSources++
		}
	}
	if modelSources == 0 {
		switch {
		case completion.ServerToolCallsKnown && completion.ServerToolCalls > 0:
			warnings = append(warnings, fmt.Sprintf(UncitedSearchWarningFmt, completion.ServerToolCalls))
		default:
			warnings = append(warnings, NoSourcesWarning)
		}
		// 来源列表非空却全是补充项时，紧跟一句说明，否则调用方会把列表当成答案的出处。
		if len(merged) > 0 {
			warnings = append(warnings, ExtraSourcesNotEvidenceWarning)
		}
	}
	result.Warning = strings.Join(warnings, "; ")
	return result, nil
}

var (
	numericTitle     = regexp.MustCompile(`^\d+$`)
	inlineCitation   = regexp.MustCompile(`\[\[(\d+)\]\]\((https?://[^)\s]+)\)`)
	repeatedCitation = regexp.MustCompile(`(\[\[\d+\]\]\([^)\s]+\))(?:\s*\[\[\d+\]\]\([^)\s]+\))+`)
)

// collapseRepeatedCitations 把紧邻连写且指向同一 URL 的重复引用折叠为一个；指向不同 URL 的
// 连续引用（多来源佐证）保持不变。
func collapseRepeatedCitations(answer string) string {
	return repeatedCitation.ReplaceAllStringFunc(answer, func(run string) string {
		parts := inlineCitation.FindAllStringSubmatch(run, -1)
		if len(parts) < 2 {
			return run
		}
		var b strings.Builder
		lastURL := ""
		for _, p := range parts {
			if p[2] == lastURL {
				continue
			}
			b.WriteString(p[0])
			lastURL = p[2]
		}
		return b.String()
	})
}

// renumberInlineCitations 把正文中 [[n]](url) 的 n 改写为 url 在最终来源列表中的
// 1 起始序号；列表中找不到的 URL 保持原样。模型自行编号时常不连续或与尾部列表错位，
// 统一以合并后的列表为准。
func renumberInlineCitations(answer string, srcs []sources.Source) string {
	if len(srcs) == 0 {
		return answer
	}
	index := make(map[string]int, len(srcs))
	for i, s := range srcs {
		index[s.URL] = i + 1
	}
	return inlineCitation.ReplaceAllStringFunc(answer, func(m string) string {
		sub := inlineCitation.FindStringSubmatch(m)
		if n, ok := index[strings.TrimRight(sub[2], ".,;:!?")]; ok {
			return fmt.Sprintf("[[%d]](%s)", n, sub[2])
		}
		if n, ok := index[sub[2]]; ok {
			return fmt.Sprintf("[[%d]](%s)", n, sub[2])
		}
		return m
	})
}

// IncompleteResponseWarningFmt 用于上游流未确认完整的情形，参数为完整性状态与原因。
// 文案必须同时说清三件事：响应未确认完整、为什么、调用方该怎么对待缺失部分——
// 否则 agent 会把截断处当作答案的自然结尾。
const IncompleteResponseWarningFmt = "upstream response was not confirmed complete (%s: %s); the answer may be missing content, treat anything it does not state as unknown rather than absent"

// NoSourcesWarning 是答案不含任何可解析引用、且上游未报告任何服务端工具调用
// 时附加的可见降级提示。
const NoSourcesWarning = "answer carries no source citations and no server-side search tool call was reported; the model likely answered from training knowledge, treat time-sensitive claims as unverified"

// UncitedSearchWarningFmt 用于"检索已执行（%d 次服务端工具调用）但正文没有可
// 解析引用"的情形，提示调用方结论有检索支撑但无法逐条溯源。
const UncitedSearchWarningFmt = "search ran (%d server-side tool calls) but the answer contains no parsable citations; sources cannot be traced per claim"

// ExtraSourcesNotEvidenceWarning 在模型自身没有来源、但来源列表里有 extra_sources 补充项时
// 紧跟无来源告警之后。非空的来源列表会让调用方误以为答案有据可查，必须说明这些条目只是
// 并行参考检索给出的阅读候选，模型作答时并未使用它们。
const ExtraSourcesNotEvidenceWarning = "the sources listed were added by the extra_sources reference search as reading candidates; the model did not use them, so they are not evidence for this answer"

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
