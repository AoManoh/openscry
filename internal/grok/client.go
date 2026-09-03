// Package grok is a minimal client for grok2api's OpenAI-compatible
// /chat/completions endpoint. It exposes a single streaming primitive
// (Complete) shared by the search and web_fetch paths; the resilience layer
// (circuit breaker, retry budget, timeouts) wraps it in internal/search and
// internal/fetch. Failures are returned as a structured *Error and the model
// is never silently changed.
package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client talks to a grok2api OpenAI-compatible endpoint.
type Client struct {
	baseURL    string
	apiKey     string
	timeout    time.Duration
	httpClient *http.Client

	// sem is a global upstream-concurrency limiter shared across every
	// Complete call on this client (and thus across the search, batch and
	// fetch paths, which all share one client instance). nil means
	// unlimited — the historical behavior, preserved by NewClient so tests
	// and any non-production caller are unaffected. A buffered channel is
	// used (not x/sync/semaphore) to honor the zero-dependency constraint
	// while staying context-aware on acquire. Ping deliberately bypasses it
	// so a saturated upstream still answers liveness/readiness checks.
	sem chan struct{}
}

// NewClient constructs a Client with no upstream-concurrency limit. baseURL
// should include the version prefix (e.g. https://host/v1). timeout is applied
// defensively when the caller's context carries no deadline.
func NewClient(baseURL, apiKey string, timeout time.Duration) *Client {
	return NewClientWithLimit(baseURL, apiKey, timeout, 0)
}

// NewClientWithLimit is NewClient plus a global upstream-concurrency cap:
// maxConcurrent bounds how many Complete calls may be in flight against
// grok2api at once, across every consumer sharing this client. maxConcurrent
// <= 0 disables the limiter (unlimited, identical to NewClient). Production
// paths wire this from GROK_UPSTREAM_CONCURRENCY so a request flood (or a
// web_search_batch fan-out) cannot amplify into unbounded simultaneous
// upstream calls regardless of transport.
func NewClientWithLimit(baseURL, apiKey string, timeout time.Duration, maxConcurrent int) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     apiKey,
		timeout:    timeout,
		httpClient: &http.Client{},
	}
	if maxConcurrent > 0 {
		c.sem = make(chan struct{}, maxConcurrent)
	}
	return c
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// hostedTool 是 OpenAI/xAI 风格的托管（服务端）工具声明，例如
// {"type":"web_search"}、{"type":"x_search"}。openscry 只声明类型，细粒度参数
// （enable_image_understanding 等）由上游按默认值规范化。
type hostedTool struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// Tools / ToolChoice 仅在调用方声明了托管工具时出现（omitempty），
	// 未声明时请求形态与历史版本逐字节一致。
	Tools      []hostedTool `json:"tools,omitempty"`
	ToolChoice string       `json:"tool_choice,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
			// Annotations 是 grok2api 在启用托管搜索后随正文下发的 url_citation
			// 增量；即使模型正文里没有写 [[n]](url) 标记，它们也能证明并定位来源。
			Annotations []Citation `json:"annotations"`
		} `json:"delta"`
	} `json:"choices"`
	// Usage 只在最后一个数据块出现；num_server_side_tools_used 是 xAI/grok2api
	// 的扩展字段，记录本次请求服务端执行了多少次托管工具调用（web_search 等）。
	Usage *struct {
		NumServerSideToolsUsed *int `json:"num_server_side_tools_used"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Citation 是流中下发的一条 url_citation 注解（OpenAI Responses 风格）。
type Citation struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Completion 是一次流式补全的完整结果：除累积正文外，还带回服务端工具调用
// 计数与 url_citation 注解，供搜索层判断"检索是否真的发生"并补齐来源。
type Completion struct {
	Content string
	// ServerToolCalls 是 usage.num_server_side_tools_used；ServerToolCallsKnown
	// 为 false 表示上游没有给出该字段（例如 Grok Web 路由或旧版网关）。
	ServerToolCalls      int
	ServerToolCallsKnown bool
	// Citations 是按出现顺序收集、按 URL 去重的 url_citation 注解。
	Citations []Citation
}

// Complete performs a streaming chat/completions call with the given system
// prompt and user content, returning the accumulated assistant content. It
// is the shared primitive behind both the search path (SearchPrompt) and the
// Grok-assisted fetch path (FetchPrompt). Any failure is returned as a
// structured *Error; the model is never silently changed.
func (c *Client) Complete(ctx context.Context, model, systemPrompt, userContent string) (string, error) {
	return c.CompleteWithTools(ctx, model, systemPrompt, userContent, nil)
}

// CompleteWithTools 与 Complete 相同，但会在请求中声明给定的托管工具类型
// （tools:[{"type":...}] 与 tool_choice:"auto"）。搜索路径用它让上游模型
// 真正执行 web_search / x_search；tools 为空时退化为 Complete 的历史请求形态。
// 上游不支持的工具类型会以 HTTP 4xx 返回并被 classifyStatusError 原样透出，
// openscry 不做任何静默剔除。
func (c *Client) CompleteWithTools(ctx context.Context, model, systemPrompt, userContent string, tools []string) (string, error) {
	res, err := c.CompleteDetailed(ctx, model, systemPrompt, userContent, tools)
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// CompleteDetailed 是 CompleteWithTools 的完整版本：除正文外返回服务端工具
// 调用计数与 url_citation 注解（见 Completion）。搜索层用它区分"检索已执行
// 但正文未写引用"与"根本没有检索"两种无来源情形。
func (c *Client) CompleteDetailed(ctx context.Context, model, systemPrompt, userContent string, tools []string) (*Completion, error) {
	if _, ok := ctx.Deadline(); !ok && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	body := chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Stream: true,
	}
	for _, name := range tools {
		if name = strings.TrimSpace(name); name != "" {
			body.Tools = append(body.Tools, hostedTool{Type: name})
		}
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Code: CodeDecode, Message: fmt.Sprintf("marshal request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Code: CodeConnect, Message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// Bound concurrent upstream calls (no-op when the limiter is disabled).
	// The slot is held for the whole streaming exchange — Do plus parseSSE —
	// because a streaming response keeps the upstream connection busy until
	// the stream is fully consumed, so that is the true unit of upstream load.
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, classifyTransportError(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyStatusError(resp.StatusCode, string(body), resp.Header.Get("Retry-After"))
	}

	res, err := parseSSE(ctx, resp.Body)
	if err != nil {
		return nil, err
	}
	res.Content = strings.TrimSpace(res.Content)
	if res.Content == "" {
		return nil, &Error{Code: CodeEmpty, Message: "upstream returned empty content", Retryable: true}
	}
	return res, nil
}

// Ping checks upstream reachability with a lightweight GET {base}/models. It
// tests the network path, not endpoint correctness: any HTTP response (even a
// 404) means the server is reachable and returns nil; only a transport-level
// failure (DNS, connection refused, timeout) is reported as an error. It is
// used by the HTTP /ready endpoint and the get_config_info diagnostic, neither
// of which should depend on grok2api exposing a specific /models contract.
func (c *Client) Ping(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return &Error{Code: CodeConnect, Message: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return classifyTransportError(ctx, err)
	}
	_ = resp.Body.Close()
	return nil
}

// parseSSE accumulates `data:` delta content from an OpenAI-style SSE
// stream until [DONE] or EOF. Unparsable keepalive lines are ignored;
// an explicit error object in the stream is surfaced. 同时收集 url_citation
// 注解与最终 usage 中的服务端工具调用计数（见 Completion）。
func parseSSE(ctx context.Context, body io.Reader) (*Completion, error) {
	reader := bufio.NewReader(body)
	var sb strings.Builder
	res := &Completion{}
	seen := make(map[string]struct{})
	for {
		line, readErr := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(trimmed[len("data:"):])
			switch {
			case data == "" || data == "[DONE]":
				if data == "[DONE]" {
					res.Content = sb.String()
					return res, nil
				}
			default:
				var chunk streamChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return nil, &Error{Code: CodeUpstreamStatus, Message: "upstream stream error: " + chunk.Error.Message}
					}
					for _, ch := range chunk.Choices {
						sb.WriteString(ch.Delta.Content)
						for _, a := range ch.Delta.Annotations {
							url := strings.TrimSpace(a.URL)
							if url == "" {
								continue
							}
							if _, dup := seen[url]; dup {
								continue
							}
							seen[url] = struct{}{}
							res.Citations = append(res.Citations, Citation{Type: a.Type, URL: url, Title: strings.TrimSpace(a.Title)})
						}
					}
					if chunk.Usage != nil && chunk.Usage.NumServerSideToolsUsed != nil {
						res.ServerToolCalls = *chunk.Usage.NumServerSideToolsUsed
						res.ServerToolCallsKnown = true
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				res.Content = sb.String()
				return res, nil
			}
			return nil, classifyTransportError(ctx, readErr)
		}
	}
}

// acquire takes one upstream-concurrency slot, blocking until a slot is free
// or ctx is done. It returns a release function (always safe to call) and a
// nil error on success. When the limiter is disabled (sem == nil) it is a
// no-op. On ctx expiry it returns a structured *Error classified so a local
// saturation never trips the breaker (see classifyAcquireError).
func (c *Client) acquire(ctx context.Context) (release func(), err error) {
	if c.sem == nil {
		return func() {}, nil
	}
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, classifyAcquireError(ctx)
	}
}

// classifyAcquireError maps a context cancellation that occurred while waiting
// for an upstream slot. A deadline expiry becomes CodeOverloaded (a LOCAL
// condition: we were saturated past the deadline) which is deliberately NOT a
// breaker failure and NOT retryable — the upstream may be perfectly healthy.
// An explicit cancellation becomes CodeCanceled.
func classifyAcquireError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return &Error{Code: CodeCanceled, Message: "request canceled while awaiting an upstream slot"}
	}
	return &Error{Code: CodeOverloaded, Message: "upstream concurrency limit saturated: deadline elapsed before a slot was free"}
}

func classifyTransportError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Message: "request timed out: " + err.Error(), Retryable: true}
	case ctx.Err() == context.Canceled || errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Message: "request canceled: " + err.Error()}
	default:
		return &Error{Code: CodeConnect, Message: "connection error: " + err.Error(), Retryable: true}
	}
}

func classifyStatusError(status int, body, retryAfter string) error {
	snippet := strings.TrimSpace(body)
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	ra := parseRetryAfter(retryAfter)
	switch {
	case status == http.StatusTooManyRequests:
		return &Error{Code: CodeRateLimit, Status: status, Message: "rate limited: " + snippet, Retryable: true, RetryAfter: ra}
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return &Error{Code: CodeModelUnavailable, Status: status, Message: "model or request rejected: " + snippet}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "auth error: " + snippet}
	case status >= 500:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "upstream server error: " + snippet, Retryable: true, RetryAfter: ra}
	default:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "unexpected status: " + snippet}
	}
}

// parseRetryAfter parses an HTTP Retry-After header value, which may be either
// a non-negative integer number of seconds or an HTTP-date. It returns 0 when
// the header is absent or unparsable (the resilience layer then falls back to
// its normal exponential backoff).
func parseRetryAfter(value string) time.Duration {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
