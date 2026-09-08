package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/resilience"
)

const testAPIKey = "secret-token-123"

func httpTestServer(t *testing.T, opt HTTPOptions) *httptest.Server {
	t.Helper()
	srv := New(nil, nil, nil)
	srv.Register(Tool{Name: "echo", InputSchema: InputSchema{Type: "object"}},
		func(_ context.Context, args map[string]any) (*ToolCallResult, error) {
			msg, _ := args["msg"].(string)
			return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: msg}}}, nil
		})
	mux, err := srv.httpMux(opt)
	if err != nil {
		t.Fatalf("httpMux: %v", err)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postRPC(t *testing.T, ts *httptest.Server, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewBufferString(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
	}
	return m
}

func TestHTTPMuxRequiresAPIKey(t *testing.T) {
	srv := New(nil, nil, nil)
	if _, err := srv.httpMux(HTTPOptions{APIKey: ""}); err == nil {
		t.Fatal("expected error when API key is empty")
	}
	if _, err := srv.httpMux(HTTPOptions{APIKey: "  "}); err == nil {
		t.Fatal("expected error when API key is blank")
	}
}

func TestMCPAuth(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

	// No key -> 401.
	if resp := postRPC(t, ts, "", init); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status=%d want 401", resp.StatusCode)
	}
	// Wrong key -> 401.
	if resp := postRPC(t, ts, "wrong", init); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: status=%d want 401", resp.StatusCode)
	}
	// Correct key -> 200.
	resp := postRPC(t, ts, testAPIKey, init)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct key: status=%d want 200", resp.StatusCode)
	}
	m := decode(t, resp)
	result, _ := m["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("initialize result missing protocolVersion: %v", m)
	}
}

func TestMCPAuthViaXAPIKey(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-API-Key auth: status=%d want 200", resp.StatusCode)
	}
}

func TestMCPToolCall(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi there"}}}`
	resp := postRPC(t, ts, testAPIKey, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	m := decode(t, resp)
	result, _ := m["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content in result: %v", m)
	}
	first, _ := content[0].(map[string]any)
	if first["text"] != "hi there" {
		t.Fatalf("echo text=%v want 'hi there'", first["text"])
	}
}

func TestMCPNotificationReturns202(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	// A notification (no id) yields no response body.
	resp := postRPC(t, ts, testAPIKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: status=%d want 202", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestMCPMethodNotAllowed(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp: status=%d want 405", resp.StatusCode)
	}
}

func TestHealthEndpoint(t *testing.T) {
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d want 200", resp.StatusCode)
	}
	if decode(t, resp)["status"] != "ok" {
		t.Fatal("health status field not ok")
	}
}

func TestReadyEndpoint(t *testing.T) {
	// No probe -> always ready.
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey})
	resp, _ := http.Get(ts.URL + "/ready")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready (no probe) status=%d want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Failing probe -> 503.
	tsFail := httpTestServer(t, HTTPOptions{
		APIKey:         testAPIKey,
		ReadinessProbe: func(context.Context) error { return errors.New("upstream down") },
	})
	resp, _ = http.Get(tsFail.URL + "/ready")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready (failing probe) status=%d want 503", resp.StatusCode)
	}
	if !strings.Contains(decode(t, resp)["error"].(string), "upstream down") {
		t.Fatal("ready error not surfaced")
	}
}

func TestWellKnownConfig(t *testing.T) {
	info := ConfigInfo{Name: "openscry", Version: "test", Transport: "http"}.Map()
	ts := httpTestServer(t, HTTPOptions{APIKey: testAPIKey, ConfigInfo: info})
	resp, err := http.Get(ts.URL + "/.well-known/mcp-config")
	if err != nil {
		t.Fatalf("get well-known: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("well-known status=%d want 200", resp.StatusCode)
	}
	m := decode(t, resp)
	if m["name"] != "openscry" || m["endpoint"] != "/mcp" {
		t.Fatalf("well-known body=%v", m)
	}
}

func TestGetConfigInfoToolNoSecrets(t *testing.T) {
	info := ConfigInfo{
		Name: "openscry", Version: "test", Model: "grok-test",
		BaseURL: "https://host/v1", Provider: "chat", Transport: "stdio",
	}
	_, handler := GetConfigInfoTool(info, func(context.Context) error { return nil })
	res, err := handler(context.Background(), nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := res.Content[0].Text
	if strings.Contains(text, "api_key") || strings.Contains(text, "Bearer ") {
		t.Fatalf("config info leaked a secret-looking field: %s", text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	conn, _ := m["connectivity"].(map[string]any)
	if conn == nil || conn["reachable"] != true {
		t.Fatalf("connectivity not reported reachable: %v", m)
	}
}

func TestGetConfigInfoToolProbeFailure(t *testing.T) {
	info := ConfigInfo{Name: "openscry"}
	_, handler := GetConfigInfoTool(info, func(context.Context) error { return errors.New("dial tcp: refused") })
	res, _ := handler(context.Background(), nil)
	var m map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].Text), &m)
	conn, _ := m["connectivity"].(map[string]any)
	if conn["reachable"] != false || !strings.Contains(conn["error"].(string), "refused") {
		t.Fatalf("probe failure not reported: %v", conn)
	}
}

func TestHTTPAdmissionControl(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	defer releaseAll()

	srv := New(nil, nil, nil)
	srv.Register(Tool{Name: "block", InputSchema: InputSchema{Type: "object"}},
		func(ctx context.Context, _ map[string]any) (*ToolCallResult, error) {
			<-release
			return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: "done"}}}, nil
		})
	// MaxInFlight=1: a single request may process at a time; the next must be
	// shed immediately rather than queued.
	mux, err := srv.httpMux(HTTPOptions{APIKey: testAPIKey, MaxInFlight: 1})
	if err != nil {
		t.Fatalf("httpMux: %v", err)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()

	callBlock := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"block","arguments":{}}}`

	// First request occupies the single admission slot (blocks in the tool).
	// Issued from a goroutine; avoid t.Fatalf off the test goroutine.
	firstStatus := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewBufferString(callBlock))
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			firstStatus <- -1
			return
		}
		firstStatus <- resp.StatusCode
		resp.Body.Close()
	}()
	// Let the first request be admitted and reach the blocking tool.
	time.Sleep(150 * time.Millisecond)

	// Second concurrent request must be rejected with 503 + Retry-After.
	resp := postRPC(t, ts, testAPIKey, callBlock)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-capacity request status=%d want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("503 response must carry a Retry-After header")
	}

	// Release the first request; it must complete successfully (the slot was
	// genuinely occupied by an admitted request, not itself shed).
	releaseAll()
	if s := <-firstStatus; s != http.StatusOK {
		t.Fatalf("first (admitted) request status=%d want 200", s)
	}
}

// slowExtractorServer 模拟 4s 才响应的 Grok 提取端；客户端提前放弃时随请求上下文结束立刻
// 退出。先读完请求体是必要的：net/http 只在请求体读尽后才开始后台读连接，否则客户端断开
// 不会取消 r.Context()，处理函数会一直睡到 4s 结束，拖慢测试收尾。
func slowExtractorServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(4 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"# Late page\"}}]}\ndata: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// toolErrorText 解出 tools/call 响应里 isError=true 的错误文案；不是错误结果时直接失败。
func toolErrorText(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200 (tool failures are isError results, not HTTP errors)", resp.StatusCode)
	}
	m := decode(t, resp)
	result, _ := m["result"].(map[string]any)
	if result == nil || result["isError"] != true {
		t.Fatalf("expected an isError=true tool result, got %v", m)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("isError result without content: %v", m)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// TestHTTPRequestCeilingDoesNotReplaceFetchBudget：HTTP 传输给每个请求设 10 分钟上限，
// 但 web_fetch 的预算必须仍由 fetch 服务自己决定。resilience 把档位下限钳制为 1s，因此用
// 1s 档位、4s 才响应的提取端、strict 模式：约 1s 内返回 isError=true 且文案含预算耗尽；
// 显式 timeout 参数则按传入值（200ms，不经钳制）生效。之前 fetch 见父上下文已有截止时间
// 就跳过档位，HTTP 下这次抓取会等到 4s 后成功，与 stdio 下约 30s 失败的行为不一致。
func TestHTTPRequestCeilingDoesNotReplaceFetchBudget(t *testing.T) {
	extractor := slowExtractorServer(t)
	fetchSvc := fetch.New(grok.NewClient(extractor.URL, "k", 30*time.Second), fetch.Options{
		Model: "m", Strict: true, Profiles: resilience.Profiles{Fetch: time.Second},
	})
	srv := New(nil, nil, nil)
	srv.Register(WebFetchTool(fetchSvc))
	// 不设 RequestTimeout：走默认的 10 分钟请求上限，正是被验证的传输层截止时间。
	mux, err := srv.httpMux(HTTPOptions{APIKey: testAPIKey})
	if err != nil {
		t.Fatalf("httpMux: %v", err)
	}
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	call := func(args string) (string, time.Duration) {
		started := time.Now()
		resp := postRPC(t, ts, testAPIKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_fetch","arguments":{`+args+`}}}`)
		elapsed := time.Since(started)
		return toolErrorText(t, resp), elapsed
	}

	t.Run("profile applies under the request ceiling", func(t *testing.T) {
		text, elapsed := call(`"url":"https://example.com/page"`)
		if !strings.Contains(text, "budget exhausted") {
			t.Fatalf("error text must report the exhausted budget, got %q", text)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("elapsed = %v, want about the 1s fetch profile (the HTTP ceiling replaced the budget)", elapsed)
		}
	})
	t.Run("explicit timeout argument applies as given", func(t *testing.T) {
		text, elapsed := call(`"url":"https://example.com/page","timeout":"200ms"`)
		if !strings.Contains(text, "budget exhausted") {
			t.Fatalf("error text must report the exhausted budget, got %q", text)
		}
		if elapsed > 1500*time.Millisecond {
			t.Fatalf("elapsed = %v, want about the explicit 200ms budget", elapsed)
		}
	})
}

func TestServeHTTPRefusesEmptyKey(t *testing.T) {
	srv := New(nil, nil, nil)
	if err := srv.ServeHTTP(context.Background(), HTTPOptions{Addr: "127.0.0.1:0", APIKey: ""}); err == nil {
		t.Fatal("ServeHTTP must refuse to start without an API key")
	}
}

func TestServeHTTPLifecycle(t *testing.T) {
	// Grab a free port, then hand the address to ServeHTTP.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeHTTP(ctx, HTTPOptions{Addr: addr, APIKey: testAPIKey}) }()

	// Poll /health until the server is up.
	base := "http://" + addr
	up := false
	for i := 0; i < 50; i++ {
		if resp, e := http.Get(base + "/health"); e == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				up = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !up {
		cancel()
		t.Fatal("server did not become healthy")
	}

	// Cancelling ctx must trigger a graceful shutdown returning nil.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP did not shut down after ctx cancel")
	}
}
