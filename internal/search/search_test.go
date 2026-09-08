package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/refsource"
	"github.com/AoManoh/openscry/internal/resilience"
)

// grokStub serves a fixed SSE answer on /chat/completions.
func grokStub(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonQuote(answer) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// firecrawlRefStub 返回一条固定的 Firecrawl 搜索结果，供 extra_sources 用例复用。
func firecrawlRefStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"web": []map[string]any{{"title": "Ref", "url": "https://ref.example.com"}}},
		})
	}))
}

// 无引用答案 + 补充检索成功：补充来源要进入来源列表，但不能抵消无来源告警。此前的断言要求
// 此时 Warning 为空，那把补充来源当成了答案的证据；实际上补充来源是与答案并行检索出的阅读
// 候选，模型作答时并未看到它们，因此同一个无引用答案不能因为传了 extra_sources 就失去告警。
func TestSearchExtraSourcesMerged(t *testing.T) {
	grokSrv := grokStub(t, "The answer.")
	defer grokSrv.Close()
	fire := firecrawlRefStub(t)
	defer fire.Close()

	ref := refsource.New(refsource.Options{FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL})
	svc := NewWithOptions(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test", Options{RefProvider: ref})

	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := NoSourcesWarning + "; " + ExtraSourcesNotEvidenceWarning
	if res.Warning != want {
		t.Fatalf("uncited answer must keep the no-sources warning and explain the extra sources:\n got %q\nwant %q", res.Warning, want)
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://ref.example.com" || res.Sources[0].Origin != "firecrawl" {
		t.Fatalf("sources=%+v want one ref source tagged with its origin", res.Sources)
	}
}

// 检索已执行（usage 报告了服务端工具调用）但正文无引用，且补充检索成功：仍然要给出
// "检索已执行但无法溯源"的告警并追加补充来源说明，两个无来源分支都不能被补充来源掩盖。
func TestSearchExtraSourcesDoNotMaskUncitedSearch(t *testing.T) {
	grokSrv := rawStub(t,
		`{"choices":[{"delta":{"content":"2025-10-07"}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":2}}`,
	)
	defer grokSrv.Close()
	fire := firecrawlRefStub(t)
	defer fire.Close()

	ref := refsource.New(refsource.Options{FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL})
	svc := NewWithOptions(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}, RefProvider: ref})

	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fmt.Sprintf(UncitedSearchWarningFmt, 2) + "; " + ExtraSourcesNotEvidenceWarning
	if res.Warning != want {
		t.Fatalf("warning:\n got %q\nwant %q", res.Warning, want)
	}
	if len(res.Sources) != 1 || res.Sources[0].Origin != "firecrawl" {
		t.Fatalf("sources=%+v want the ref source", res.Sources)
	}
	if r := res.ExtraSources; r == nil || r.Requested != 1 || r.Added != 1 {
		t.Fatalf("extra sources report = %+v", r)
	}
}

// 有引用答案 + 补充检索成功：模型自身有来源，不得出现任何无来源告警；来源列表保持
// "模型引用在前、补充来源在后"的合并顺序。
func TestSearchExtraSourcesWithCitedAnswerCarryNoSourceWarning(t *testing.T) {
	grokSrv := grokStub(t, "Cited.[[1]](https://ex.example.com/a)")
	defer grokSrv.Close()
	fire := firecrawlRefStub(t)
	defer fire.Close()

	ref := refsource.New(refsource.Options{FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL})
	svc := NewWithOptions(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test", Options{RefProvider: ref})

	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warning != "" {
		t.Fatalf("cited answer with extra sources must not warn, got %q", res.Warning)
	}
	if len(res.Sources) != 2 || res.Sources[0].URL != "https://ex.example.com/a" || res.Sources[0].Origin != "" ||
		res.Sources[1].URL != "https://ref.example.com" || res.Sources[1].Origin != "firecrawl" {
		t.Fatalf("sources=%+v want model source first, then the ref source", res.Sources)
	}
}

func TestSearchExtraSourcesFailureWarns(t *testing.T) {
	grokSrv := grokStub(t, "The answer.")
	defer grokSrv.Close()
	fire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fire.Close()

	ref := refsource.New(refsource.Options{FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL})
	svc := NewWithOptions(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test", Options{RefProvider: ref})

	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 3})
	if err != nil {
		t.Fatalf("primary search must succeed despite ref failure, got %v", err)
	}
	if res.Content != "The answer." {
		t.Fatalf("content=%q", res.Content)
	}
	if res.Warning == "" || !strings.Contains(res.Warning, "firecrawl") {
		t.Fatalf("expected a firecrawl warning, got %q", res.Warning)
	}
}

func TestSearchExtraSourcesNoopWithoutProvider(t *testing.T) {
	grokSrv := grokStub(t, "Answer with no refs.")
	defer grokSrv.Close()
	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test")
	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 没有 provider 时 extra_sources 不得产生任何 extra_sources 相关告警；答案本身
	// 无引用，因此唯一允许出现的告警是 NoSourcesWarning。
	if res.Warning != NoSourcesWarning || len(res.Sources) != 0 {
		t.Fatalf("extra_sources should be a no-op without a provider: warning=%q sources=%v", res.Warning, res.Sources)
	}
}

// capturingStub 记录最近一次请求体并返回固定答案，用于断言工具声明是否透传。
func capturingStub(t *testing.T, answer string) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonQuote(answer) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestSearchDeclaresConfiguredTools(t *testing.T) {
	srv, last := capturingStub(t, "Cited.[[1]](https://ex.example.com/a)")
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search", "x_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := last()
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 || body["tool_choice"] != "auto" {
		t.Fatalf("request must declare both tools with tool_choice=auto, got tools=%v tool_choice=%v", body["tools"], body["tool_choice"])
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://ex.example.com/a" {
		t.Fatalf("inline [[n]](url) citation must be parsed, got %+v", res.Sources)
	}
	if res.Warning != "" {
		t.Fatalf("cited answer must not carry a warning, got %q", res.Warning)
	}
}

func TestSearchWithoutToolsKeepsLegacyRequestAndWarns(t *testing.T) {
	srv, last := capturingStub(t, "Answer with no refs.")
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := last()
	if _, has := body["tools"]; has {
		t.Fatalf("no Tools configured, request must not carry tools: %v", body["tools"])
	}
	if res.Warning != NoSourcesWarning {
		t.Fatalf("uncited answer must warn, got %q", res.Warning)
	}
	if res.Content != "Answer with no refs." {
		t.Fatalf("content must be returned unchanged, got %q", res.Content)
	}
}

func TestSearchReturnsAccumulatedContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "test-key", 5*time.Second), "grok-4.3-console")
	res, err := svc.Search(context.Background(), Request{Query: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "Hello world" {
		t.Fatalf("unexpected content: %q", res.Content)
	}
	if res.Model != "grok-4.3-console" {
		t.Fatalf("unexpected model: %q", res.Model)
	}
}

func TestSearchModelFailureIsExplicitNoSilentFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "default-model")
	_, err := svc.Search(context.Background(), Request{Query: "hi", Model: "missing-model"})
	if err == nil {
		t.Fatal("expected explicit error, got nil")
	}
	ge, ok := grok.AsError(err)
	if !ok {
		t.Fatalf("expected *grok.Error, got %T", err)
	}
	if ge.Code != grok.CodeModelUnavailable {
		t.Fatalf("expected model_unavailable, got %q", ge.Code)
	}
}

func TestSearchEmptyContentIsVisibleError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	// MaxAttempts=1 keeps the test fast and focused on "empty is a visible
	// error" rather than exercising the retry-on-empty path.
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m", Options{MaxAttempts: 1})
	_, err := svc.Search(context.Background(), Request{Query: "hi"})
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeEmpty {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestSearchRetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // transient 5xx
			_, _ = w.Write([]byte(`{"error":{"message":"upstream hiccup"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()

	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m",
		Options{MaxAttempts: 3, RetryBaseDelay: time.Millisecond})
	res, err := svc.Search(context.Background(), Request{Query: "hi"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if res.Content != "recovered" {
		t.Fatalf("content=%q want \"recovered\"", res.Content)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls=%d want 3 (2 transient failures + 1 success)", got)
	}
}

// TestSearchCircuitBreakerOpensUnderSustainedFailure is the S2 fault-injection
// gate: sustained upstream 5xx must trip the breaker so further calls fail
// fast with ErrCircuitOpen instead of hammering the dead upstream.
func TestSearchCircuitBreakerOpensUnderSustainedFailure(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer srv.Close()

	// FailureThreshold=2, no retry: the first two searches each record one
	// transport failure and trip the breaker; the third must fail fast with
	// ErrCircuitOpen without reaching upstream.
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "m", Options{
		MaxAttempts: 1,
		Breaker:     resilience.BreakerConfig{FailureThreshold: 2, OpenDuration: time.Minute},
	})

	_, err1 := svc.Search(context.Background(), Request{Query: "q"})
	_, err2 := svc.Search(context.Background(), Request{Query: "q"})
	if err1 == nil || err2 == nil {
		t.Fatalf("expected first two searches to fail, got %v / %v", err1, err2)
	}
	callsAfterTrip := calls.Load()

	_, err3 := svc.Search(context.Background(), Request{Query: "q"})
	if !errors.Is(err3, resilience.ErrCircuitOpen) {
		t.Fatalf("third search err=%v want ErrCircuitOpen", err3)
	}
	if got := calls.Load(); got != callsAfterTrip {
		t.Fatalf("breaker-open call reached upstream: calls %d -> %d", callsAfterTrip, got)
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	svc := New(grok.NewClient("http://example.invalid", "k", time.Second), "m")
	_, err := svc.Search(context.Background(), Request{Query: "   "})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-query error, got %v", err)
	}
}

func TestSearchResponsesProviderFailsLoud(t *testing.T) {
	// The responses provider is a seam, not yet implemented. It must fail
	// loud rather than silently falling back to chat. No HTTP server is
	// needed: the seam short-circuits before any client call.
	svc := NewWithOptions(grok.NewClient("http://unused.invalid", "k", time.Second), "m",
		Options{Provider: "responses", MaxAttempts: 1})
	_, err := svc.Search(context.Background(), Request{Query: "hi"})
	if err == nil || !strings.Contains(err.Error(), "responses") {
		t.Fatalf("expected fail-loud responses error, got %v", err)
	}
}

// rawStub 原样回放给定的 SSE 数据块（不含 "data: " 前缀），用于模拟 grok2api v3
// 在启用托管搜索后下发的 annotations 增量与带工具计数的 usage。
func rawStub(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

func TestSearchRecoversSourcesFromAnnotations(t *testing.T) {
	// 正文没有 [[n]](url) 标记，但流里带 url_citation 注解且 usage 报告了工具调用。
	srv := rawStub(t,
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"2025-10-07 "}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url":"https://www.python.org/downloads/release/python-3140/","title":"1"}]}}]}`,
		`{"choices":[{"delta":{"content":"https://www.python.org/"}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url":"https://www.python.org/downloads/release/python-3140/","title":"1"}]}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":3}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://www.python.org/downloads/release/python-3140/" {
		t.Fatalf("annotation citations must become sources (deduplicated), got %+v", res.Sources)
	}
	if res.Warning != "" {
		t.Fatalf("sourced answer must not warn, got %q", res.Warning)
	}
	if !strings.HasPrefix(res.Content, "2025-10-07") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestSearchWarnsDistinctlyWhenSearchRanButUncited(t *testing.T) {
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"2025-10-07"}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":2}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := fmt.Sprintf(UncitedSearchWarningFmt, 2)
	if res.Warning != want {
		t.Fatalf("warning = %q, want %q", res.Warning, want)
	}
}

func TestSearchWarnsNoSearchWhenUsageReportsZeroTools(t *testing.T) {
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"From memory."}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":0}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warning != NoSourcesWarning {
		t.Fatalf("warning = %q, want NoSourcesWarning", res.Warning)
	}
}

func TestSearchDropsNumericAnnotationTitlesAndRenumbersInline(t *testing.T) {
	// 模型内联编号 [[2]] 与列表顺序错位，且注解 title 只是序号 "2"。
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"Teapot.[[2]](https://http.dev/418) and RFC.[[1]](https://www.rfc-editor.org/rfc/rfc2324)"}}]}`,
		`{"choices":[{"delta":{"annotations":[{"type":"url_citation","url":"https://http.dev/418","title":"2"},{"type":"url_citation","url":"https://www.rfc-editor.org/rfc/rfc2324","title":"RFC 2324"}]}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":2}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})
	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Sources) != 2 || res.Sources[0].URL != "https://http.dev/418" || res.Sources[1].URL != "https://www.rfc-editor.org/rfc/rfc2324" {
		t.Fatalf("sources = %+v", res.Sources)
	}
	if res.Sources[0].Title != "" || res.Sources[1].Title != "RFC 2324" {
		t.Fatalf("numeric title must be dropped, real title kept: %+v", res.Sources)
	}
	want := "Teapot.[[1]](https://http.dev/418) and RFC.[[2]](https://www.rfc-editor.org/rfc/rfc2324)"
	if res.Content != want {
		t.Fatalf("inline citations must be renumbered to list order:\n got %q\nwant %q", res.Content, want)
	}
	if !res.ServerToolCallsKnown || res.ServerToolCalls != 2 || res.Elapsed <= 0 {
		t.Fatalf("observability fields missing: %+v", res)
	}
}

func TestCollapseRepeatedCitations(t *testing.T) {
	in := "A.[[1]](https://a.org/x)[[1]](https://a.org/x) B.[[2]](https://b.org/y) [[2]](https://b.org/y) C.[[1]](https://a.org/x)[[2]](https://b.org/y)"
	want := "A.[[1]](https://a.org/x) B.[[2]](https://b.org/y) C.[[1]](https://a.org/x)[[2]](https://b.org/y)"
	if got := collapseRepeatedCitations(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// openStreamStub 原样回放给定数据块但不发送 [DONE]，并统计上游被调用的次数，
// 用于模拟上游中途结束响应，同时断言不完整响应没有触发重试。
func openStreamStub(t *testing.T, calls *atomic.Int64, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n"))
		}
	}))
}

func TestSearchUnconfirmedStreamKeepsContentAndWarns(t *testing.T) {
	var calls atomic.Int64
	srv := openStreamStub(t, &calls,
		`{"choices":[{"delta":{"content":"Cited fact.[[1]](https://a.org/x) Then the stream "}}]}`,
		`{"choices":[{"delta":{"content":"stops mid-"}}]}`,
	)
	defer srv.Close()
	// MaxAttempts=3：证明不完整响应不会被当作失败重试（上游只应被调用一次）。
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test",
		Options{MaxAttempts: 3, RetryBaseDelay: time.Millisecond, Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("incomplete response must not be an error, got %v", err)
	}
	if res.Content != "Cited fact.[[1]](https://a.org/x) Then the stream stops mid-" {
		t.Fatalf("partial content must be returned unchanged, got %q", res.Content)
	}
	if res.CompletionState != grok.StateUnconfirmed || res.CompletionDetail == "" {
		t.Fatalf("completion = %q / %q, want unconfirmed with a detail", res.CompletionState, res.CompletionDetail)
	}
	want := fmt.Sprintf(IncompleteResponseWarningFmt, grok.StateUnconfirmed, res.CompletionDetail)
	if res.Warning != want {
		t.Fatalf("warning = %q\nwant %q", res.Warning, want)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("sources must still be extracted from partial content, got %+v", res.Sources)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 (incomplete responses must not be retried)", got)
	}
}

func TestSearchTruncatedStreamWarnsAlongsideOtherWarnings(t *testing.T) {
	// finish_reason=length 且随后有 [DONE]：状态为 truncated；正文无引用，无来源告警也要保留，
	// 两条告警以 "; " 拼接，完整性告警在前。
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"Long answer that got cut"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":0}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "Long answer that got cut" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.CompletionState != grok.StateTruncated || !strings.Contains(res.CompletionDetail, "length") {
		t.Fatalf("completion = %q / %q, want truncated mentioning length", res.CompletionState, res.CompletionDetail)
	}
	want := fmt.Sprintf(IncompleteResponseWarningFmt, grok.StateTruncated, res.CompletionDetail) + "; " + NoSourcesWarning
	if res.Warning != want {
		t.Fatalf("warning = %q\nwant %q", res.Warning, want)
	}
}

func TestSearchFilteredStreamIsDisclosed(t *testing.T) {
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"Partially filtered.[[1]](https://a.org/x)"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CompletionState != grok.StateFiltered {
		t.Fatalf("completion state = %q, want filtered", res.CompletionState)
	}
	if !strings.Contains(res.Warning, "not confirmed complete") || !strings.Contains(res.Warning, string(grok.StateFiltered)) {
		t.Fatalf("warning must disclose the filtered state, got %q", res.Warning)
	}
}

func TestSearchCompleteStreamHasNoCompletionWarning(t *testing.T) {
	// 正常结束（[DONE] + stop）的响应：状态 complete、无 detail，告警里不得出现完整性文案，
	// 现有正常输出形态不变。
	srv := rawStub(t,
		`{"choices":[{"delta":{"content":"Whole answer.[[1]](https://a.org/x)"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":1}}`,
	)
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}})

	res, err := svc.Search(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CompletionState != grok.StateComplete || res.CompletionDetail != "" {
		t.Fatalf("completion = %q / %q, want complete with empty detail", res.CompletionState, res.CompletionDetail)
	}
	if res.Warning != "" {
		t.Fatalf("complete, cited answer must carry no warning, got %q", res.Warning)
	}
}

func TestSearchReportsExtraSourcesOutcome(t *testing.T) {
	// 模型引用 a.org；参考检索给出 a.org（重复）与 b.org（新增）；firecrawl 失败
	grokSrv := rawStub(t,
		`{"choices":[{"delta":{"content":"Answer.[[1]](https://a.org/x)"}}]}`,
		`{"choices":[],"usage":{"num_server_side_tools_used":1}}`,
	)
	defer grokSrv.Close()
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"url": "https://a.org/x", "title": "A"}, {"url": "https://b.org/y", "title": "B"}}})
	}))
	defer tavily.Close()
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer firecrawl.Close()
	ref := refsource.New(refsource.Options{TavilyAPIKey: "k", TavilyAPIURL: tavily.URL, FirecrawlAPIKey: "k", FirecrawlAPIURL: firecrawl.URL})
	svc := NewWithOptions(grok.NewClient(grokSrv.URL, "k", 5*time.Second), "grok-test", Options{Tools: []string{"web_search"}, RefProvider: ref})
	res, err := svc.Search(context.Background(), Request{Query: "q", ExtraSources: 4})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := res.ExtraSources
	if r == nil || r.Requested != 4 || r.Added != 1 || r.Duplicates != 1 || r.ByOrigin["tavily"] != 1 || len(r.Failed) != 1 || r.Failed[0] != "firecrawl" {
		t.Fatalf("extra sources report = %+v", r)
	}
	if len(res.Sources) != 2 || res.Sources[1].Origin != "tavily" {
		t.Fatalf("sources = %+v", res.Sources)
	}
}

// slowGrokStub 在 delay 之后才返回一段完整答案；客户端提前放弃时随请求上下文结束立刻
// 退出，httptest.Server.Close 不必等待被放弃的处理函数。用于验证预算是否真的约束了调用：
// 预算生效时调用在预算处返回，预算失效时调用会等到 delay 之后成功。
func slowGrokStub(t *testing.T, delay time.Duration, answer string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 必须先读完请求体：net/http 只在请求体读尽后才开始后台读连接，否则客户端断开
		// 不会取消 r.Context()，处理函数会一直睡到 delay 结束，拖慢测试收尾。
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonQuote(answer) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireTimeoutWithin 断言 err 是 grok 的超时分类，且调用耗时不超过 limit。
func requireTimeoutWithin(t *testing.T, err error, elapsed, limit time.Duration) {
	t.Helper()
	ge, ok := grok.AsError(err)
	if !ok || ge.Code != grok.CodeTimeout {
		t.Fatalf("err = %v, want a grok timeout error", err)
	}
	if elapsed > limit {
		t.Fatalf("elapsed = %v, want at most %v (the budget did not bound the call)", elapsed, limit)
	}
}

// 档位是搜索的默认预算。resilience 把档位下限钳制为 1s，因此用 1s 档位配合 4s 才响应的
// 上游：预算生效时约 1s 内以超时错误返回；重试保持默认次数，证明重试不能把总耗时拖过预算。
func TestSearchProfileBoundsCallWithoutParentDeadline(t *testing.T) {
	srv := slowGrokStub(t, 4*time.Second, "late")
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 30*time.Second), "m",
		Options{Profiles: resilience.Profiles{Search: time.Second}})

	started := time.Now()
	_, err := svc.Search(context.Background(), Request{Query: "q"})
	requireTimeoutWithin(t, err, time.Since(started), 3*time.Second)
}

// 父上下文带很晚的截止时间（模拟 MCP HTTP 传输的 10 分钟请求上限）时，档位仍然是预算：
// 传输层上限只能收紧预算，不能替代它。
func TestSearchProfileBoundsCallUnderLateParentDeadline(t *testing.T) {
	srv := slowGrokStub(t, 4*time.Second, "late")
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 30*time.Second), "m",
		Options{Profiles: resilience.Profiles{Search: time.Second}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	_, err := svc.Search(ctx, Request{Query: "q"})
	requireTimeoutWithin(t, err, time.Since(started), 3*time.Second)
}

func TestSearchExplicitTimeoutOverridesProfile(t *testing.T) {
	t.Run("shorter than profile", func(t *testing.T) {
		// 档位 30s、显式 200ms：显式预算不经档位钳制，必须在 200ms 附近超时。
		srv := slowGrokStub(t, 4*time.Second, "late")
		svc := NewWithOptions(grok.NewClient(srv.URL, "k", 30*time.Second), "m",
			Options{MaxAttempts: 1, Profiles: resilience.Profiles{Search: 30 * time.Second}})

		started := time.Now()
		_, err := svc.Search(context.Background(), Request{Query: "q", Timeout: 200 * time.Millisecond})
		requireTimeoutWithin(t, err, time.Since(started), 1500*time.Millisecond)
	})
	t.Run("longer than profile", func(t *testing.T) {
		// 档位 1s（下限）、上游 1.3s 后响应、显式 5s：调用必须成功，证明显式预算也能放宽档位。
		srv := slowGrokStub(t, 1300*time.Millisecond, "late but whole")
		svc := NewWithOptions(grok.NewClient(srv.URL, "k", 30*time.Second), "m",
			Options{MaxAttempts: 1, Profiles: resilience.Profiles{Search: time.Second}})

		res, err := svc.Search(context.Background(), Request{Query: "q", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("an explicit budget longer than the profile must let the call finish, got %v", err)
		}
		if res.Content != "late but whole" {
			t.Fatalf("content = %q", res.Content)
		}
	})
}
