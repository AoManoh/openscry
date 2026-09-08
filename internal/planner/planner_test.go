package planner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/resilience"
)

// validPlanJSON is a well-formed plan response used by most tests.
const validPlanJSON = `{
  "intent": {
    "core_question": "How does Go's garbage collector work?",
    "query_type": "exploratory",
    "time_sensitivity": "historical",
    "domain": "programming languages"
  },
  "complexity": {
    "level": 2,
    "estimated_queries": 3,
    "estimated_calls": 5,
    "justification": "Multiple aspects (tricolor, pacer, STW) need separate investigation"
  },
  "sub_queries": [
    {"id": "sq1", "goal": "Understand tricolor mark-sweep algorithm", "expected_output": "Algorithm description with phases", "boundary": "Excludes pacer/scheduling", "depends_on": "", "tool_hint": "web_search"},
    {"id": "sq2", "goal": "Understand GC pacer and scheduling", "expected_output": "How Go decides when to GC", "boundary": "Excludes mark algorithm details", "depends_on": "", "tool_hint": "web_search"},
    {"id": "sq3", "goal": "Recent GC improvements in Go 1.19+", "expected_output": "Changelog items and benchmark data", "boundary": "Only post-1.19 changes", "depends_on": "sq1,sq2", "tool_hint": "web_search"}
  ],
  "search_terms": [
    {"term": "Go garbage collector tricolor algorithm", "purpose": "sq1", "round": 1},
    {"term": "Go GC pacer scheduling heuristics", "purpose": "sq2", "round": 1},
    {"term": "Go 1.22 GC improvements benchmark", "purpose": "sq3", "round": 2}
  ],
  "execution": {
    "parallel_groups": [["sq1", "sq2"]],
    "sequential": ["sq3"]
  },
  "strategies": {
    "fetch_before_claim": true,
    "gap_check": true,
    "fallback_plan": "If academic sources unavailable, use official Go blog posts"
  }
}`

func sseContent(text string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\ndata: [DONE]\n"
}

func newPlanServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// SSE doesn't handle JSON well in a single delta (quotes need escaping).
		// Send the entire response as one chunk for simplicity in tests.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(response) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

// jsonEscape wraps a string as a JSON string value (with quotes).
func jsonEscape(s string) string {
	// Use a raw approach: escape special chars for embedding in JSON.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return `"` + s + `"`
}

func TestPlanSuccess(t *testing.T) {
	srv := newPlanServer(t, validPlanJSON)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	plan, err := svc.Plan(context.Background(), "How does Go's garbage collector work?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plan.Intent.CoreQuestion != "How does Go's garbage collector work?" {
		t.Fatalf("core_question=%q", plan.Intent.CoreQuestion)
	}
	if plan.Complexity.Level != 2 {
		t.Fatalf("level=%d want 2", plan.Complexity.Level)
	}
	if len(plan.SubQueries) != 3 {
		t.Fatalf("sub_queries=%d want 3", len(plan.SubQueries))
	}
	if len(plan.SearchTerms) != 3 {
		t.Fatalf("search_terms=%d want 3", len(plan.SearchTerms))
	}
	if !plan.Strategies.FetchBeforeClaim {
		t.Fatal("fetch_before_claim should be true")
	}
	if !plan.Strategies.GapCheck {
		t.Fatal("gap_check should be true")
	}
	// normalizePlan 按 depends_on 重新分层：sq1/sq2 无依赖为第 1 层，sq3 依赖两者为第 2 层
	if len(plan.Execution.ParallelGroups) != 2 || len(plan.Execution.ParallelGroups[0]) != 2 || plan.Execution.ParallelGroups[1][0] != "sq3" {
		t.Fatalf("parallel_groups=%v", plan.Execution.ParallelGroups)
	}
	if len(plan.Execution.Sequential) != 1 || plan.Execution.Sequential[0] != "sq3" {
		t.Fatalf("sequential=%v", plan.Execution.Sequential)
	}
}

// 模型把只依赖同一前置的多个子查询全部塞进 sequential 时，规范化应让它们并行；
// 没有搜索词的 web_search 子查询要补出兜底搜索词。
func TestNormalizePlanParallelizesSiblingsAndBackfillsTerms(t *testing.T) {
	p := &Plan{
		SubQueries: []SubQuery{
			{ID: "sq1", Goal: "discover candidates"},
			{ID: "sq2", Goal: "check licenses", DependsOn: "sq1"},
			{ID: "sq3", Goal: "check agent features", DependsOn: "sq1"},
			{ID: "sq4", Goal: "check MCP support", DependsOn: "sq1", ToolHint: "web_search"},
			{ID: "sq5", Goal: "fetch official docs page", DependsOn: "sq2, sq3", ToolHint: "web_fetch"},
		},
		SearchTerms: []SearchTerm{{Term: "open source agent platform", Purpose: "sq1", Round: 1}},
		Execution:   Execution{ParallelGroups: [][]string{{"sq1"}}, Sequential: []string{"sq2", "sq3", "sq4", "sq5"}},
	}
	normalizePlan(p)
	want := [][]string{{"sq1"}, {"sq2", "sq3", "sq4"}, {"sq5"}}
	if len(p.Execution.ParallelGroups) != 3 {
		t.Fatalf("groups=%v want %v", p.Execution.ParallelGroups, want)
	}
	for i := range want {
		if strings.Join(p.Execution.ParallelGroups[i], ",") != strings.Join(want[i], ",") {
			t.Fatalf("groups=%v want %v", p.Execution.ParallelGroups, want)
		}
	}
	if strings.Join(p.Execution.Sequential, ",") != "sq2,sq3,sq4,sq5" {
		t.Fatalf("sequential=%v", p.Execution.Sequential)
	}
	// sq2/sq3/sq4 缺搜索词 -> 用 goal 兜底；sq5 是 web_fetch 不需要搜索词
	purposes := map[string]bool{}
	for _, t := range p.SearchTerms {
		purposes[t.Purpose] = true
	}
	for _, id := range []string{"sq1", "sq2", "sq3", "sq4"} {
		if !purposes[id] {
			t.Fatalf("missing search term for %s: %+v", id, p.SearchTerms)
		}
	}
	if purposes["sq5"] {
		t.Fatalf("web_fetch sub-query must not get a synthesized search term: %+v", p.SearchTerms)
	}
}

// planJSONWithSubQueries 只替换 sub_queries，其余字段保持合法，让用例聚焦依赖关系本身；
// search_terms 覆盖 sq1，避免触发无关的校验分支。
func planJSONWithSubQueries(subQueries string) string {
	return `{
  "intent": {"core_question": "q", "query_type": "factual", "time_sensitivity": "irrelevant"},
  "complexity": {"level": 1, "estimated_queries": 2, "estimated_calls": 2, "justification": "j"},
  "sub_queries": ` + subQueries + `,
  "search_terms": [{"term": "t", "purpose": "sq1", "round": 1}],
  "execution": {"parallel_groups": [], "sequential": []},
  "strategies": {"fetch_before_claim": false, "gap_check": false}
}`
}

// 依赖关系不可执行的规划必须以校验错误失败，而不是被 normalizePlan 静默修正后当作成功返回：
// 互相依赖会分出 null 层、缺失依赖与自依赖会被忽略、重复 ID 会在分组里出现两次。错误文案
// 必须点名相关 ID 与失败类型，调用方才知道计划为何不可执行。
func TestPlanRejectsUnexecutableDependencies(t *testing.T) {
	cases := map[string]struct {
		subQueries string
		wantKind   string
		wantIDs    []string
	}{
		"mutual dependency": {
			subQueries: `[{"id":"sq1","goal":"a","depends_on":"sq2"},{"id":"sq2","goal":"b","depends_on":"sq1"}]`,
			wantKind:   "cycle",
			wantIDs:    []string{"sq1", "sq2"},
		},
		"indirect cycle": {
			subQueries: `[{"id":"sq1","goal":"a"},{"id":"sq2","goal":"b","depends_on":"sq1,sq4"},{"id":"sq3","goal":"c","depends_on":"sq2"},{"id":"sq4","goal":"d","depends_on":"sq3"}]`,
			wantKind:   "cycle",
			wantIDs:    []string{"sq2", "sq3", "sq4"},
		},
		"missing dependency": {
			subQueries: `[{"id":"sq1","goal":"a"},{"id":"sq2","goal":"b","depends_on":"sq1, sq9"}]`,
			wantKind:   "unknown",
			wantIDs:    []string{"sq2", "sq9"},
		},
		"duplicate id": {
			subQueries: `[{"id":"sq1","goal":"a"},{"id":"sq1","goal":"b"}]`,
			wantKind:   "duplicate",
			wantIDs:    []string{"sq1"},
		},
		"self dependency": {
			subQueries: `[{"id":"sq1","goal":"a","depends_on":"sq1"}]`,
			wantKind:   "itself",
			wantIDs:    []string{"sq1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newPlanServer(t, planJSONWithSubQueries(tc.subQueries))
			defer srv.Close()
			svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
			plan, err := svc.Plan(context.Background(), "test")
			if err == nil {
				t.Fatalf("unexecutable plan must be rejected, got execution %+v", plan.Execution)
			}
			msg := err.Error()
			if !strings.Contains(msg, "planner: plan validation failed:") {
				t.Fatalf("error must be reported as a validation failure, got: %v", err)
			}
			if !strings.Contains(msg, tc.wantKind) {
				t.Fatalf("error must name the failure kind %q, got: %v", tc.wantKind, err)
			}
			for _, id := range tc.wantIDs {
				if !strings.Contains(msg, id) {
					t.Fatalf("error must name %s, got: %v", id, err)
				}
			}
		})
	}
}

// 合法的多层依赖计划（同层并行、跨层顺序、前向引用、depends_on 里的空 token）必须通过校验，
// 且规范化结果与引入校验前一致：校验只拒绝不可执行的计划，不改变可执行计划的行为。
func TestValidatePlanAcceptsMultiLevelDependencies(t *testing.T) {
	header := func(sqs []SubQuery) *Plan {
		return &Plan{
			Intent:      Intent{CoreQuestion: "q"},
			Complexity:  Complexity{Level: 2},
			SubQueries:  sqs,
			SearchTerms: []SearchTerm{{Term: "open source agent platform", Purpose: "sq1", Round: 1}},
		}
	}
	requireGroups := func(t *testing.T, p *Plan, want [][]string) {
		t.Helper()
		if len(p.Execution.ParallelGroups) != len(want) {
			t.Fatalf("groups=%v want %v", p.Execution.ParallelGroups, want)
		}
		for i := range want {
			if strings.Join(p.Execution.ParallelGroups[i], ",") != strings.Join(want[i], ",") {
				t.Fatalf("groups=%v want %v", p.Execution.ParallelGroups, want)
			}
		}
	}

	t.Run("three levels with parallel siblings", func(t *testing.T) {
		p := header([]SubQuery{
			{ID: "sq1", Goal: "discover candidates"},
			{ID: "sq2", Goal: "check licenses", DependsOn: "sq1"},
			{ID: "sq3", Goal: "check agent features", DependsOn: "sq1"},
			{ID: "sq4", Goal: "check MCP support", DependsOn: "sq1", ToolHint: "web_search"},
			{ID: "sq5", Goal: "fetch official docs page", DependsOn: "sq2, sq3", ToolHint: "web_fetch"},
		})
		if err := validatePlan(p); err != nil {
			t.Fatalf("legal multi-level plan must validate, got %v", err)
		}
		normalizePlan(p)
		requireGroups(t, p, [][]string{{"sq1"}, {"sq2", "sq3", "sq4"}, {"sq5"}})
		if strings.Join(p.Execution.Sequential, ",") != "sq2,sq3,sq4,sq5" {
			t.Fatalf("sequential=%v", p.Execution.Sequential)
		}
	})
	t.Run("forward reference and blank tokens", func(t *testing.T) {
		// 依赖声明在被依赖项之前，depends_on 带空白与尾随逗号：都是合法写法，不得被当成缺失依赖。
		p := header([]SubQuery{
			{ID: "sq2", Goal: "b", DependsOn: " sq1 ,"},
			{ID: "sq1", Goal: "a"},
		})
		if err := validatePlan(p); err != nil {
			t.Fatalf("forward reference must validate, got %v", err)
		}
		normalizePlan(p)
		requireGroups(t, p, [][]string{{"sq1"}, {"sq2"}})
	})
}

func TestPlanStripsMarkdownFences(t *testing.T) {
	// Some models wrap JSON in ```json ... ```
	fenced := "```json\n" + validPlanJSON + "\n```"
	srv := newPlanServer(t, fenced)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	plan, err := svc.Plan(context.Background(), "test question")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Intent.CoreQuestion == "" {
		t.Fatal("plan parsed but core_question empty")
	}
}

func TestPlanEmptyQuestionRejected(t *testing.T) {
	svc := New(grok.NewClient("http://unused", "k", time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "   ")
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-question error, got %v", err)
	}
}

func TestPlanModelFailureIsExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "bad-model"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil {
		t.Fatal("expected explicit error for model failure, got nil")
	}
	if !strings.Contains(err.Error(), "model call failed") {
		t.Fatalf("error should mention model call failed: %v", err)
	}
}

func TestPlanIncompleteResponseIsExplicitError(t *testing.T) {
	// 规划 JSON 在流中途被切断（无 [DONE]）或因 finish_reason=length 截断时，必须以完整性
	// 错误失败，而不是把残缺 JSON 交给解析器产生误导性的格式错误。
	half := validPlanJSON[:len(validPlanJSON)/2]
	cases := map[string]func(w http.ResponseWriter){
		"eof before [DONE]": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(half) + "}}]}\n"))
		},
		"finish_reason=length": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(half) + "}}]}\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n"))
			_, _ = w.Write([]byte("data: [DONE]\n"))
		},
		// 恰好完整可解析的 JSON 但流未确认完整：同样拒绝，完整性判据不依赖解析结果。
		"parsable json without [DONE]": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(validPlanJSON) + "}}]}\n"))
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				write(w)
			}))
			defer srv.Close()
			svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
			plan, err := svc.Plan(context.Background(), "test")
			if err == nil {
				t.Fatalf("incomplete response must be an error, got plan %+v", plan)
			}
			if !strings.Contains(err.Error(), "planner: model response incomplete") {
				t.Fatalf("error must name the incompleteness, got: %v", err)
			}
			if strings.Contains(err.Error(), "parse") {
				t.Fatalf("incomplete response must not be reported as a parse failure: %v", err)
			}
		})
	}
}

func TestPlanCompleteResponseWithStopFinishReason(t *testing.T) {
	// 带 finish_reason=stop 与 [DONE] 的正常响应仍然成功解析，行为不变。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(validPlanJSON) + "}}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	plan, err := svc.Plan(context.Background(), "test")
	if err != nil || len(plan.SubQueries) != 3 {
		t.Fatalf("complete response must parse: plan=%+v err=%v", plan, err)
	}
}

// slowPlanServer 在 delay 之后才返回完整的规划 JSON；客户端提前放弃时随请求上下文结束
// 立刻退出。先读完请求体是必要的：net/http 只在请求体读尽后才开始后台读连接，否则客户端
// 断开不会取消 r.Context()，处理函数会一直睡到 delay 结束，拖慢测试收尾。
func slowPlanServer(t *testing.T, delay time.Duration, response string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(response) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireTimeoutWithin 断言规划因模型调用超时失败（保留 "model call failed" 前缀与 grok 的
// 超时分类），且耗时不超过 limit。
func requireTimeoutWithin(t *testing.T, err error, elapsed, limit time.Duration) {
	t.Helper()
	ge, ok := grok.AsError(err)
	if err == nil || !strings.Contains(err.Error(), "model call failed") || !ok || ge.Code != grok.CodeTimeout {
		t.Fatalf("err = %v, want a model-call timeout", err)
	}
	if elapsed > limit {
		t.Fatalf("elapsed = %v, want at most %v (the budget did not bound the call)", elapsed, limit)
	}
}

func TestPlanProfileAppliesUnderLateParentDeadline(t *testing.T) {
	// 父上下文带 10 分钟截止时间（模拟 MCP HTTP 传输的请求上限），模型 4s 才响应，
	// 搜索档位 1s（resilience 的下限）：必须约 1s 内以超时失败，而不是等到 4s 后成功。
	srv := slowPlanServer(t, 4*time.Second, validPlanJSON)
	svc := New(grok.NewClient(srv.URL, "k", 30*time.Second), Options{Model: "m", Profiles: resilience.Profiles{Search: time.Second}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	_, err := svc.Plan(ctx, "test")
	requireTimeoutWithin(t, err, time.Since(started), 3*time.Second)
}

func TestPlanExplicitTimeoutOverridesProfile(t *testing.T) {
	t.Run("shorter than profile", func(t *testing.T) {
		// 默认档位 120s、显式 200ms：显式预算不经档位钳制，必须在 200ms 附近超时。
		srv := slowPlanServer(t, 4*time.Second, validPlanJSON)
		svc := New(grok.NewClient(srv.URL, "k", 30*time.Second), Options{Model: "m"})

		started := time.Now()
		_, err := svc.PlanWithTimeout(context.Background(), "test", 200*time.Millisecond)
		requireTimeoutWithin(t, err, time.Since(started), 1500*time.Millisecond)
	})
	t.Run("longer than profile", func(t *testing.T) {
		// 档位 1s（下限）、模型 1.3s 后响应、显式 5s：规划必须成功，证明显式预算也能放宽档位。
		srv := slowPlanServer(t, 1300*time.Millisecond, validPlanJSON)
		svc := New(grok.NewClient(srv.URL, "k", 30*time.Second), Options{Model: "m", Profiles: resilience.Profiles{Search: time.Second}})

		plan, err := svc.PlanWithTimeout(context.Background(), "test", 5*time.Second)
		if err != nil {
			t.Fatalf("an explicit budget longer than the profile must let the call finish, got %v", err)
		}
		if len(plan.SubQueries) != 3 {
			t.Fatalf("plan = %+v, want the 3 sub-queries of validPlanJSON", plan)
		}
	})
}

func TestPlanInvalidJSONFromModel(t *testing.T) {
	srv := newPlanServer(t, "this is not json at all")
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error should mention parse: %v", err)
	}
}

func TestPlanValidationFailsOnEmptySubQueries(t *testing.T) {
	incomplete := `{
		"intent":{"core_question":"q","query_type":"factual","time_sensitivity":"irrelevant"},
		"complexity":{"level":1,"estimated_queries":1,"estimated_calls":1,"justification":"simple"},
		"sub_queries":[],
		"search_terms":[{"term":"t","purpose":"sq1","round":1}],
		"execution":{"parallel_groups":[],"sequential":[]},
		"strategies":{"fetch_before_claim":false,"gap_check":false}
	}`
	srv := newPlanServer(t, incomplete)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "sub_queries must not be empty") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestParsePlanDirect(t *testing.T) {
	plan, err := parsePlan(validPlanJSON)
	if err != nil {
		t.Fatalf("parsePlan failed: %v", err)
	}
	if plan.Intent.QueryType != "exploratory" {
		t.Fatalf("query_type=%q", plan.Intent.QueryType)
	}
	if plan.SubQueries[2].DependsOn != "sq1,sq2" {
		t.Fatalf("depends_on=%q", plan.SubQueries[2].DependsOn)
	}
}
