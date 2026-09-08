package fetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
	"github.com/AoManoh/openscry/internal/resilience"
)

// sseContent renders a minimal OpenAI-style SSE body carrying one content
// delta followed by [DONE], matching what the grok client parses.
func sseContent(text string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n" +
		"data: [DONE]\n"
}

func newSSEServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseContent(text)))
	}))
}

func newStatusServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestFetchGrokTierWins(t *testing.T) {
	grokSrv := newSSEServer(t, "fetched-by-grok")
	defer grokSrv.Close()

	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "grok" {
		t.Fatalf("tier=%q want grok", res.Tier)
	}
	if !strings.Contains(res.Content, "fetched-by-grok") {
		t.Fatalf("content=%q", res.Content)
	}
	if res.Model != "m" {
		t.Fatalf("model=%q want m", res.Model)
	}
}

func TestFetchFallsThroughToBasicHTTP(t *testing.T) {
	// Grok tier fails (500); no Tavily/Firecrawl keys -> those tiers skipped;
	// the basic-HTTP tier must win against the served HTML page.
	grokFail := newStatusServer(http.StatusInternalServerError, "boom")
	defer grokFail.Close()
	html := "<html><head><title>Hello &amp; World</title></head><body>" +
		"<script>var x=1;</script><h1>Heading</h1><p>Para one.</p><p>Para two.</p></body></html>"
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), htmlSrv.URL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http", res.Tier)
	}
	if !strings.Contains(res.Content, "Hello & World") {
		t.Fatalf("expected unescaped title in content, got %q", res.Content)
	}
	for _, want := range []string{"Heading", "Para one.", "Para two."} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("content missing %q: %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "var x=1") {
		t.Fatalf("script content should be stripped: %q", res.Content)
	}
}

func TestFetchTavilyTierWins(t *testing.T) {
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" {
			t.Errorf("unexpected tavily path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"results":[{"raw_content":"# Tavily markdown"}]}`))
	}))
	defer tavily.Close()

	svc := New(nil, Options{TavilyAPIKey: "tk", TavilyAPIURL: tavily.URL})
	res, err := svc.Fetch(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "tavily" || !strings.Contains(res.Content, "Tavily markdown") {
		t.Fatalf("tier=%q content=%q", res.Tier, res.Content)
	}
}

func TestFetchFirecrawlAfterTavilyFails(t *testing.T) {
	tavilyFail := newStatusServer(http.StatusInternalServerError, "down")
	defer tavilyFail.Close()
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scrape" {
			t.Errorf("unexpected firecrawl path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"markdown":"# FC markdown"}}`))
	}))
	defer firecrawl.Close()

	svc := New(nil, Options{
		TavilyAPIKey: "tk", TavilyAPIURL: tavilyFail.URL,
		FirecrawlAPIKey: "fk", FirecrawlAPIURL: firecrawl.URL,
	})
	res, err := svc.Fetch(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "firecrawl" || !strings.Contains(res.Content, "FC markdown") {
		t.Fatalf("tier=%q content=%q", res.Tier, res.Content)
	}
}

func TestFetchAllTiersFailVisibleError(t *testing.T) {
	grokFail := newStatusServer(http.StatusInternalServerError, "boom")
	defer grokFail.Close()
	httpFail := newStatusServer(http.StatusInternalServerError, "nope")
	defer httpFail.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Fetch(context.Background(), httpFail.URL)
	if err == nil {
		t.Fatal("expected failure when every tier fails")
	}
	msg := err.Error()
	if !strings.Contains(msg, "all extractors failed") || !strings.Contains(msg, "grok:") || !strings.Contains(msg, "http:") {
		t.Fatalf("error should list tiers tried, got: %v", err)
	}
}

func TestFetchRejectsInvalidURL(t *testing.T) {
	svc := New(nil, Options{})
	for _, bad := range []string{"", "   ", "not-a-url", "ftp://example.com/x", "/relative/path"} {
		if _, err := svc.Fetch(context.Background(), bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}

func TestStripHTMLToText(t *testing.T) {
	in := "<html><head><title>T</title><style>.x{color:red}</style></head>" +
		"<body><script>bad()</script><h1>H</h1><p>one</p><p>two</p></body></html>"
	out := stripHTMLToText(in)
	if strings.Contains(out, "bad()") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style not stripped: %q", out)
	}
	for _, want := range []string{"H", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestExtractTitle(t *testing.T) {
	if got := extractTitle("<TITLE> Hi &amp; Bye </TITLE>"); got != "Hi & Bye" {
		t.Fatalf("title=%q want \"Hi & Bye\"", got)
	}
	if got := extractTitle("<body>no title</body>"); got != "" {
		t.Fatalf("expected empty title, got %q", got)
	}
}

func TestFetchReservesBudgetForBasicHTTP(t *testing.T) {
	// The Grok tier hangs far longer than the extractor budget; the reserved
	// slice must let the basic-HTTP last resort still succeed rather than the
	// slow tier consuming the whole budget.
	grokSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep past the extractor budget (~600ms) so the Grok tier is
		// time-boxed by the reserve; bounded so test teardown stays quick.
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer grokSlow.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>OK</title></head><body><p>real content</p></body></html>"))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokSlow.URL, "k", 5*time.Second), Options{Model: "m"})
	// 900ms total -> ~300ms reserved for basic HTTP, ~600ms for extractors.
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	res, err := svc.Fetch(ctx, htmlSrv.URL)
	if err != nil {
		t.Fatalf("expected basic-HTTP fallback to succeed, got %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http (grok tier should be time-boxed by the reserve)", res.Tier)
	}
	if !strings.Contains(res.Content, "real content") {
		t.Fatalf("content=%q", res.Content)
	}
}

func TestFetchStrictDisablesBasicHTTP(t *testing.T) {
	// In strict mode (GROK_FETCH_FALLBACK=strict) the basic-HTTP last resort
	// is disabled. When the grok tier fails and no Tavily/Firecrawl is
	// configured, the fetch must fail loud instead of degrading.
	grokFail := newStatusServer(404, `{"error":{"message":"model not found"}}`)
	defer grokFail.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{
		Model:  "m",
		Strict: true,
	})
	_, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err == nil {
		t.Fatal("expected fail-loud in strict mode, got nil")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("error should mention strict mode, got: %v", err)
	}
	if strings.Contains(err.Error(), "http:") {
		t.Fatalf("strict mode should not have tried the http tier, got: %v", err)
	}
}

func TestFetchGrokFailureSentinelFallsThrough(t *testing.T) {
	// Regression: when the Grok tier returns the failure sentinel (the model
	// could not retrieve the page), full mode must NOT accept it as content;
	// the chain falls through to the basic-HTTP last resort.
	grokSentinel := newSSEServer(t, prompt.FetchFailureSentinel)
	defer grokSentinel.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Real</title></head><body><p>real page content</p></body></html>"))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokSentinel.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), htmlSrv.URL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http (grok sentinel must not win)", res.Tier)
	}
	if !strings.Contains(res.Content, "real page content") {
		t.Fatalf("content=%q", res.Content)
	}
}

func TestFetchGrokFailureSentinelStrictFailsLoud(t *testing.T) {
	// Regression: in strict mode the basic-HTTP fallback is disabled, so a
	// Grok sentinel (page unavailable) must fail loud rather than be accepted
	// as content.
	grokSentinel := newSSEServer(t, prompt.FetchFailureSentinel)
	defer grokSentinel.Close()

	svc := New(grok.NewClient(grokSentinel.URL, "k", 5*time.Second), Options{Model: "m", Strict: true})
	_, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err == nil {
		t.Fatal("expected fail-loud when grok signals unavailable in strict mode")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("error should mention strict mode, got: %v", err)
	}
}

// TestFetchGrokTierDeclaresTools 断言 Grok 层请求按 Options.Tools 声明托管工具，
// 且未配置时保持历史请求形态（无 tools / tool_choice）。
func TestFetchGrokTierDeclaresTools(t *testing.T) {
	var mu sync.Mutex
	var last map[string]any
	grokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseContent("# Page")))
	}))
	defer grokSrv.Close()
	client := grok.NewClient(grokSrv.URL, "k", 5*time.Second)

	res, err := New(client, Options{Model: "m", Tools: []string{"web_search"}}).Fetch(context.Background(), "https://example.com/doc")
	if err != nil || res.Tier != "grok" {
		t.Fatalf("fetch = %+v, %v", res, err)
	}
	mu.Lock()
	tools, _ := last["tools"].([]any)
	choice := last["tool_choice"]
	mu.Unlock()
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" || choice != "auto" {
		t.Fatalf("grok tier must declare web_search with tool_choice=auto, got tools=%v tool_choice=%v", tools, choice)
	}

	if _, err := New(client, Options{Model: "m"}).Fetch(context.Background(), "https://example.com/doc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mu.Lock()
	_, hasTools := last["tools"]
	_, hasChoice := last["tool_choice"]
	mu.Unlock()
	if hasTools || hasChoice {
		t.Fatalf("without Tools the grok tier must keep the legacy request shape, got %v", last)
	}
}

func TestFetchGrokTierPartialSentinelIsTierFailure(t *testing.T) {
	grokSrv := newSSEServer(t, "# Partial page\\n\\nOnly the first section...\\n\\nOPENSCRY_FETCH_PARTIAL")
	defer grokSrv.Close()
	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m", Strict: true})
	_, err := svc.Fetch(context.Background(), "https://example.com/long-page")
	if err == nil || !strings.Contains(err.Error(), "partial content") {
		t.Fatalf("partial sentinel must fail the grok tier in strict mode, got %v", err)
	}
}

// newRawSSEServer 逐行原样回放 SSE 行（含 "data: " 前缀），不自动追加 [DONE]，
// 用于模拟上游中途结束响应或给出 finish_reason=length。
func newRawSSEServer(lines ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
}

func TestFetchGrokTierIncompleteStreamStrictFailsLoud(t *testing.T) {
	// 上游写出部分页面正文后未发送 [DONE] 就结束响应：strict 模式下 Grok 层必须以
	// "response incomplete" 失败，残缺正文不得作为成功页面返回。
	cases := map[string][]string{
		"eof before [DONE]": {`data: {"choices":[{"delta":{"content":"# Page\n\nFirst section only"}}]}`},
		"finish_reason=length": {
			`data: {"choices":[{"delta":{"content":"# Page\n\nCut by the token limit"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			"data: [DONE]",
		},
	}
	for name, lines := range cases {
		t.Run(name, func(t *testing.T) {
			grokSrv := newRawSSEServer(lines...)
			defer grokSrv.Close()
			svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m", Strict: true})
			_, err := svc.Fetch(context.Background(), "https://example.com/long-page")
			if err == nil {
				t.Fatal("incomplete grok response must fail loud in strict mode, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, "grok tier: response incomplete") || !strings.Contains(msg, "strict") {
				t.Fatalf("error must name the incomplete grok tier and strict mode, got: %v", err)
			}
			if strings.Contains(msg, "http:") {
				t.Fatalf("strict mode must not try the http tier, got: %v", err)
			}
		})
	}
}

func TestFetchGrokTierIncompleteStreamFallsThroughToBasicHTTP(t *testing.T) {
	// full 模式：Grok 层的残缺响应按该层失败处理，链条降级到 basic HTTP，tier 标注为 http。
	grokSrv := newRawSSEServer(`data: {"choices":[{"delta":{"content":"# Partial page from grok"}}]}`)
	defer grokSrv.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Real</title></head><body><p>real page content</p></body></html>"))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), htmlSrv.URL)
	if err != nil {
		t.Fatalf("full mode must degrade to basic HTTP, got %v", err)
	}
	if res.Tier != "http" || res.Model != "" {
		t.Fatalf("tier=%q model=%q, want http tier without a model (grok partial content must not win)", res.Tier, res.Model)
	}
	if strings.Contains(res.Content, "Partial page from grok") || !strings.Contains(res.Content, "real page content") {
		t.Fatalf("content must come from the http tier, got %q", res.Content)
	}
}

func TestFetchGrokTierCompleteStreamStillWins(t *testing.T) {
	// 正常结束（finish_reason=stop + [DONE]）的 Grok 响应仍然是该层成功，行为不变。
	grokSrv := newRawSSEServer(
		`data: {"choices":[{"delta":{"content":"# Whole page"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	)
	defer grokSrv.Close()
	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m", Strict: true})
	res, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err != nil || res.Tier != "grok" || res.Content != "# Whole page" {
		t.Fatalf("complete grok response must win: res=%+v err=%v", res, err)
	}
}

// slowGrokServer 在 delay 之后才返回一段 SSE 正文；客户端提前放弃时随请求上下文结束立刻
// 退出。先读完请求体是必要的：net/http 只在请求体读尽后才开始后台读连接，否则客户端断开
// 不会取消 r.Context()，处理函数会一直睡到 delay 结束，拖慢测试收尾。
func slowGrokServer(t *testing.T, delay time.Duration, text string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseContent(text)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireBudgetExhaustedWithin 断言抓取以预算耗尽失败，且耗时不超过 limit。
func requireBudgetExhaustedWithin(t *testing.T, err error, elapsed, limit time.Duration) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("err = %v, want a budget-exhausted failure", err)
	}
	if elapsed > limit {
		t.Fatalf("elapsed = %v, want at most %v (the budget did not bound the chain)", elapsed, limit)
	}
}

func TestFetchProfileAppliesUnderLateParentDeadline(t *testing.T) {
	// 父上下文带 10 分钟截止时间（模拟 MCP HTTP 传输的请求上限），Grok 层 4s 才响应，
	// OpFetch 档位 1s（resilience 的下限）：strict 模式下必须约 1s 内以预算耗尽失败。
	// 之前"父上下文已有截止时间就跳过档位"会让这次抓取等到 4s 后成功。
	grokSlow := slowGrokServer(t, 4*time.Second, "# Late page")
	svc := New(grok.NewClient(grokSlow.URL, "k", 30*time.Second), Options{
		Model: "m", Strict: true, Profiles: resilience.Profiles{Fetch: time.Second},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	_, err := svc.Fetch(ctx, "https://example.com/page")
	requireBudgetExhaustedWithin(t, err, time.Since(started), 3*time.Second)
}

func TestFetchExplicitTimeoutOverridesProfile(t *testing.T) {
	t.Run("shorter than profile", func(t *testing.T) {
		// 默认档位 30s、显式 200ms：显式预算不经档位钳制，必须在 200ms 附近以预算耗尽失败。
		grokSlow := slowGrokServer(t, 4*time.Second, "# Late page")
		svc := New(grok.NewClient(grokSlow.URL, "k", 30*time.Second), Options{Model: "m", Strict: true})

		started := time.Now()
		_, err := svc.FetchWithTimeout(context.Background(), "https://example.com/page", 200*time.Millisecond)
		requireBudgetExhaustedWithin(t, err, time.Since(started), 1500*time.Millisecond)
	})
	t.Run("longer than profile", func(t *testing.T) {
		// 档位 1s（下限）、Grok 层 1.3s 后响应、显式 5s：抓取必须成功，证明显式预算也能放宽档位。
		grokSlow := slowGrokServer(t, 1300*time.Millisecond, "# Late page")
		svc := New(grok.NewClient(grokSlow.URL, "k", 30*time.Second), Options{
			Model: "m", Strict: true, Profiles: resilience.Profiles{Fetch: time.Second},
		})

		res, err := svc.FetchWithTimeout(context.Background(), "https://example.com/page", 5*time.Second)
		if err != nil {
			t.Fatalf("an explicit budget longer than the profile must let the chain finish, got %v", err)
		}
		if res.Tier != "grok" || res.Content != "# Late page" {
			t.Fatalf("res = %+v, want the grok tier's page", res)
		}
	})
}

func TestRewriteGitHubBlobURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/langgenius/dify/blob/main/LICENSE":     "https://raw.githubusercontent.com/langgenius/dify/main/LICENSE",
		"https://github.com/o/r/blob/v1.2.3/docs/a%20b.md#L10-L20": "https://raw.githubusercontent.com/o/r/v1.2.3/docs/a%20b.md",
		"https://github.com/o/r/raw/main/README.md":                "https://raw.githubusercontent.com/o/r/main/README.md",
		"https://github.com/o/r":                                   "https://github.com/o/r",
		"https://github.com/o/r/tree/main/docs":                    "https://github.com/o/r/tree/main/docs",
		"https://raw.githubusercontent.com/o/r/main/LICENSE":       "https://raw.githubusercontent.com/o/r/main/LICENSE",
		"https://example.com/blob/main/x":                          "https://example.com/blob/main/x",
	}
	for in, want := range cases {
		if got := RewriteGitHubBlobURL(in); got != want {
			t.Errorf("RewriteGitHubBlobURL(%q) = %q, want %q", in, got, want)
		}
	}
}
