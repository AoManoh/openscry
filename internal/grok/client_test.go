package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// concTracker records the peak number of simultaneous in-flight requests so a
// test can assert how many calls actually reached the upstream at once.
type concTracker struct {
	mu        sync.Mutex
	cur, peak int
}

func (ct *concTracker) enter() {
	ct.mu.Lock()
	ct.cur++
	if ct.cur > ct.peak {
		ct.peak = ct.cur
	}
	ct.mu.Unlock()
}

func (ct *concTracker) leave() {
	ct.mu.Lock()
	ct.cur--
	ct.mu.Unlock()
}

func (ct *concTracker) max() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.peak
}

// blockingGrokServer is an SSE stub that holds every /chat/completions request
// until release is closed, tracking concurrency in ct. /models (the Ping
// target) returns 200 immediately and is never tracked or blocked, so a test
// can verify Ping bypasses the upstream limiter.
func blockingGrokServer(release <-chan struct{}, ct *concTracker) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		ct.enter()
		defer ct.leave()
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

// waitForInFlight blocks until at least n requests have reached the server or
// the deadline elapses, so a test does not race the occupying goroutine.
func waitForInFlight(ct *concTracker, n int) {
	deadline := time.Now().Add(2 * time.Second)
	for ct.max() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUpstreamLimiterCapsConcurrency(t *testing.T) {
	const limit, callers = 3, 12
	release := make(chan struct{})
	ct := &concTracker{}
	srv := blockingGrokServer(release, ct)
	defer srv.Close()

	c := NewClientWithLimit(srv.URL, "k", 5*time.Second, limit)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Complete(context.Background(), "m", "s", "u")
		}()
	}
	// Give the limiter time to saturate: `limit` requests reach the server and
	// block; the rest wait on the client-side semaphore and never arrive.
	time.Sleep(150 * time.Millisecond)
	got := ct.max()
	close(release)
	wg.Wait()

	if got != limit {
		t.Fatalf("peak in-flight upstream = %d, want exactly %d", got, limit)
	}
}

func TestUpstreamLimiterDisabledIsUnlimited(t *testing.T) {
	const callers = 8
	release := make(chan struct{})
	ct := &concTracker{}
	srv := blockingGrokServer(release, ct)
	defer srv.Close()

	// NewClient (no limit) must let every caller reach the upstream at once,
	// preserving the historical unbounded behavior.
	c := NewClient(srv.URL, "k", 5*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Complete(context.Background(), "m", "s", "u")
		}()
	}
	time.Sleep(150 * time.Millisecond)
	got := ct.max()
	close(release)
	wg.Wait()

	if got != callers {
		t.Fatalf("peak in-flight upstream = %d, want all %d concurrent (unlimited)", got, callers)
	}
}

func TestUpstreamLimiterCtxCancelReturnsOverloaded(t *testing.T) {
	release := make(chan struct{})
	ct := &concTracker{}
	srv := blockingGrokServer(release, ct)
	defer srv.Close()
	defer close(release)

	c := NewClientWithLimit(srv.URL, "k", 5*time.Second, 1)
	// Occupy the single slot with a long-held call.
	go func() { _, _ = c.Complete(context.Background(), "m", "s", "u") }()
	waitForInFlight(ct, 1)

	// A second call whose deadline elapses while waiting for a slot must fail
	// with CodeOverloaded — a LOCAL saturation signal that is neither
	// retryable nor (per isBreakerFailure) a breaker failure.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, "m", "s", "u")
	ge, ok := AsError(err)
	if !ok {
		t.Fatalf("err = %v, want a *grok.Error", err)
	}
	if ge.Code != CodeOverloaded {
		t.Fatalf("code = %s, want %s", ge.Code, CodeOverloaded)
	}
	if ge.Retryable {
		t.Fatal("CodeOverloaded must not be retryable")
	}
}

func TestPingBypassesUpstreamLimiter(t *testing.T) {
	release := make(chan struct{})
	ct := &concTracker{}
	srv := blockingGrokServer(release, ct)
	defer srv.Close()
	defer close(release)

	c := NewClientWithLimit(srv.URL, "k", 5*time.Second, 1)
	// Saturate the only Complete slot.
	go func() { _, _ = c.Complete(context.Background(), "m", "s", "u") }()
	waitForInFlight(ct, 1)

	// Liveness/readiness must not be blocked by a saturated upstream limiter.
	pctx, pcancel := context.WithTimeout(context.Background(), time.Second)
	defer pcancel()
	if err := c.Ping(pctx); err != nil {
		t.Fatalf("Ping should bypass the upstream limiter, got %v", err)
	}
}

// capturingGrokServer 记录最近一次 /chat/completions 请求体并返回一个最小 SSE 流，
// 用于断言请求形态（tools / tool_choice）而不依赖真实上游。
func capturingGrokServer(t *testing.T) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestCompleteWithToolsDeclaresHostedTools(t *testing.T) {
	srv, last := capturingGrokServer(t)
	defer srv.Close()
	c := NewClient(srv.URL, "k", 5*time.Second)

	out, err := c.CompleteWithTools(context.Background(), "m", "sys", "user", []string{"web_search", " x_search ", ""})
	if err != nil || out != "ok" {
		t.Fatalf("CompleteWithTools = %q, %v", out, err)
	}
	body := last()
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want two hosted tool declarations", body["tools"])
	}
	for i, want := range []string{"web_search", "x_search"} {
		tool, _ := tools[i].(map[string]any)
		if tool["type"] != want || len(tool) != 1 {
			t.Errorf("tools[%d] = %v, want {type:%s} only", i, tool, want)
		}
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", body["tool_choice"])
	}
	if body["stream"] != true || body["model"] != "m" {
		t.Errorf("stream/model altered: %v / %v", body["stream"], body["model"])
	}
}

func TestCompleteWithoutToolsKeepsLegacyShape(t *testing.T) {
	srv, last := capturingGrokServer(t)
	defer srv.Close()
	c := NewClient(srv.URL, "k", 5*time.Second)

	for name, call := range map[string]func() (string, error){
		"Complete":  func() (string, error) { return c.Complete(context.Background(), "m", "sys", "user") },
		"nil tools": func() (string, error) { return c.CompleteWithTools(context.Background(), "m", "sys", "user", nil) },
		"blank-only tools": func() (string, error) {
			return c.CompleteWithTools(context.Background(), "m", "sys", "user", []string{" ", ""})
		},
	} {
		if out, err := call(); err != nil || out != "ok" {
			t.Fatalf("%s = %q, %v", name, out, err)
		}
		body := last()
		if _, has := body["tools"]; has {
			t.Errorf("%s: request must not carry tools, got %v", name, body["tools"])
		}
		if _, has := body["tool_choice"]; has {
			t.Errorf("%s: request must not carry tool_choice, got %v", name, body["tool_choice"])
		}
		if len(body) != 3 {
			t.Errorf("%s: legacy shape must be exactly model/messages/stream, got keys %d: %v", name, len(body), body)
		}
	}
}

// rawSSEServer 逐行原样回放给定的 SSE 行（含 "data: " 前缀），不自动追加 [DONE]，
// 用于模拟上游中途结束、finish_reason 各取值与流内 error 对象等形态。
func rawSSEServer(lines ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n"))
		}
	}))
}

func TestCompleteDetailedCompletionState(t *testing.T) {
	const body = `data: {"choices":[{"delta":{"content":"partial answer"}}]}`
	cases := []struct {
		name         string
		lines        []string
		wantContent  string // 空表示期望默认正文 "partial answer"
		wantState    CompletionState
		wantReason   string
		wantDetail   []string // 每一项都必须出现在 Detail 中
		wantNoDetail bool     // complete 状态不得携带 Detail
	}{
		{
			name:         "[DONE] with stop",
			lines:        []string{body, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, "data: [DONE]"},
			wantState:    StateComplete,
			wantReason:   "stop",
			wantNoDetail: true,
		},
		{
			name:         "[DONE] without any finish_reason",
			lines:        []string{body, "data: [DONE]"},
			wantState:    StateComplete,
			wantNoDetail: true,
		},
		{
			name:         "[DONE] with null then end_turn then stop",
			lines:        []string{`data: {"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`, `data: {"choices":[{"delta":{"content":"b"},"finish_reason":"end_turn"}]}`, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, "data: [DONE]"},
			wantContent:  "ab",
			wantState:    StateComplete,
			wantReason:   "stop",
			wantNoDetail: true,
		},
		{
			name:       "[DONE] with length",
			lines:      []string{body, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`, "data: [DONE]"},
			wantState:  StateTruncated,
			wantReason: "length",
			wantDetail: []string{"length", "token"},
		},
		{
			name:       "[DONE] with content_filter",
			lines:      []string{body, `data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`, "data: [DONE]"},
			wantState:  StateFiltered,
			wantReason: "content_filter",
			wantDetail: []string{"content_filter"},
		},
		{
			name:       "EOF without [DONE] and without finish_reason",
			lines:      []string{body},
			wantState:  StateUnconfirmed,
			wantDetail: []string{"[DONE]"},
		},
		{
			name:         "EOF without [DONE] but with stop",
			lines:        []string{body, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`},
			wantState:    StateComplete,
			wantReason:   "stop",
			wantNoDetail: true,
		},
		{
			name:       "EOF without [DONE] but with length",
			lines:      []string{body, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`},
			wantState:  StateTruncated,
			wantReason: "length",
			wantDetail: []string{"length"},
		},
		{
			name:       "unknown finish_reason with [DONE]",
			lines:      []string{body, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, "data: [DONE]"},
			wantState:  StateUnconfirmed,
			wantReason: "tool_calls",
			wantDetail: []string{`"tool_calls"`},
		},
		{
			name:       "unknown finish_reason without [DONE]",
			lines:      []string{body, `data: {"choices":[{"delta":{},"finish_reason":"something_else"}]}`},
			wantState:  StateUnconfirmed,
			wantReason: "something_else",
			wantDetail: []string{`"something_else"`, "[DONE]"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := rawSSEServer(tc.lines...)
			defer srv.Close()
			res, err := NewClient(srv.URL, "k", 5*time.Second).CompleteDetailed(context.Background(), "m", "sys", "user", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			wantContent := tc.wantContent
			if wantContent == "" {
				wantContent = "partial answer"
			}
			if res.Content != wantContent {
				t.Fatalf("content must be preserved regardless of completeness: got %q, want %q", res.Content, wantContent)
			}
			if res.State != tc.wantState {
				t.Fatalf("state = %q, want %q (detail %q)", res.State, tc.wantState, res.Detail)
			}
			if res.FinishReason != tc.wantReason {
				t.Fatalf("finish_reason = %q, want %q", res.FinishReason, tc.wantReason)
			}
			if tc.wantNoDetail && res.Detail != "" {
				t.Fatalf("complete response must carry no detail, got %q", res.Detail)
			}
			if !tc.wantNoDetail && res.Detail == "" {
				t.Fatalf("incomplete state %q must carry a detail", res.State)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(res.Detail, want) {
					t.Fatalf("detail %q must mention %q", res.Detail, want)
				}
			}
		})
	}
}

func TestCompleteDetailedPartialBodyKeepsPartialText(t *testing.T) {
	// 上游写出一部分正文后直接结束响应：正文原样保留，状态为 unconfirmed，
	// 不能被当成完整答案，也不能被当成错误丢弃。
	srv := rawSSEServer(
		`data: {"choices":[{"delta":{"content":"The release date is "}}]}`,
		`data: {"choices":[{"delta":{"content":"2025-"}}]}`,
	)
	defer srv.Close()
	res, err := NewClient(srv.URL, "k", 5*time.Second).CompleteDetailed(context.Background(), "m", "sys", "user", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Content != "The release date is 2025-" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.State != StateUnconfirmed || !strings.Contains(res.Detail, "[DONE]") {
		t.Fatalf("state = %q detail = %q, want unconfirmed mentioning the missing [DONE]", res.State, res.Detail)
	}
}

func TestCompleteDetailedStreamErrorObjectStillFails(t *testing.T) {
	// 显式的流内 error 对象是上游明确报错，行为不变：返回结构化错误而不是带状态的正文。
	srv := rawSSEServer(
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: {"error":{"message":"upstream exploded"}}`,
		"data: [DONE]",
	)
	defer srv.Close()
	res, err := NewClient(srv.URL, "k", 5*time.Second).CompleteDetailed(context.Background(), "m", "sys", "user", nil)
	if res != nil {
		t.Fatalf("expected nil completion on stream error, got %+v", res)
	}
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeUpstreamStatus || !strings.Contains(ge.Message, "upstream exploded") {
		t.Fatalf("expected upstream_status error carrying the message, got %v", err)
	}
}

func TestCompleteDetailedEmptyBodyIsStillEmptyError(t *testing.T) {
	// 完整性判定不改变空正文的语义：无正文的流（无论是否收到 [DONE]）仍是 CodeEmpty。
	for name, lines := range map[string][]string{
		"[DONE] only":            {"data: [DONE]"},
		"EOF only":               {},
		"length with no content": {`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`, "data: [DONE]"},
	} {
		srv := rawSSEServer(lines...)
		_, err := NewClient(srv.URL, "k", 5*time.Second).CompleteDetailed(context.Background(), "m", "sys", "user", nil)
		srv.Close()
		if ge, ok := AsError(err); !ok || ge.Code != CodeEmpty {
			t.Fatalf("%s: expected CodeEmpty, got %v", name, err)
		}
	}
}

func TestClassifyStatusErrorInvalidToolsAndSSENoise(t *testing.T) {
	body := `{"error":{"code":"invalid_tools","message":"Grok Web 暂不支持 tools.type=\"x_search\"","type":"invalid_request_error"}}data: {"error":{"code":"upstream_stream_incomplete"},"type":"error"}

data: [DONE]
`
	err := classifyStatusError(400, body, "")
	ge, ok := AsError(err)
	if !ok || ge.Code != CodeInvalidRequest {
		t.Fatalf("400 invalid_tools must classify as invalid_request, got %v", err)
	}
	if strings.Contains(ge.Message, "data:") || strings.Contains(ge.Message, "[DONE]") {
		t.Fatalf("SSE frames must be stripped from the error message: %q", ge.Message)
	}
	if !strings.Contains(ge.Message, "invalid_tools") {
		t.Fatalf("first JSON object must be kept: %q", ge.Message)
	}
	// 404 与 400/model_not_found 仍归为 model_unavailable。
	if ge, _ := AsError(classifyStatusError(404, `{"error":{"code":"model_not_found","message":"模型不存在"}}`, "")); ge.Code != CodeModelUnavailable {
		t.Fatalf("404 must stay model_unavailable, got %v", ge.Code)
	}
	if ge, _ := AsError(classifyStatusError(400, `{"error":{"code":"model_not_found","message":"x"}}`, "")); ge.Code != CodeModelUnavailable {
		t.Fatalf("400 model_not_found must stay model_unavailable, got %v", ge.Code)
	}
}
