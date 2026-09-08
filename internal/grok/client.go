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
		// FinishReason 是上游对本次生成为何结束的声明：stop / end_turn 为自然结束，
		// length 为达到输出 token 上限，content_filter 为内容被过滤。用指针区分
		// "字段缺失或 null"（非末尾块的常态）与"给出了空串"，只有非空值才参与判定。
		FinishReason *string `json:"finish_reason"`
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

// CompletionState 是对一次流式响应完整性的判定。它回答的是"上游是否确认这段正文
// 是完整答案"，与正文是否为空无关（空正文仍是 CodeEmpty 错误）。
type CompletionState string

const (
	// StateComplete：收到 [DONE] 且最后的 finish_reason 为空、stop 或 end_turn；
	// 或虽未收到 [DONE]，但 finish_reason 已明确给出 stop / end_turn。
	StateComplete CompletionState = "complete"
	// StateTruncated：finish_reason 为 length，模型达到输出 token 上限，正文尾部缺失。
	StateTruncated CompletionState = "truncated"
	// StateFiltered：finish_reason 为 content_filter，部分输出被上游过滤掉。
	StateFiltered CompletionState = "filtered"
	// StateUnconfirmed：流在 [DONE] 之前结束且没有任何表明正常结束的 finish_reason
	// （例如上游中途关闭连接），或出现了未知的非空 finish_reason。这类正文可能完整
	// 也可能残缺，客户端无法裁定，只能如实标注。
	StateUnconfirmed CompletionState = "unconfirmed"
)

// Completion 是一次流式补全的完整结果：除累积正文外，还带回服务端工具调用
// 计数与 url_citation 注解，供搜索层判断"检索是否真的发生"并补齐来源；以及
// 流完整性判定，供调用方决定残缺正文是披露还是拒绝。
type Completion struct {
	Content string
	// ServerToolCalls 是 usage.num_server_side_tools_used；ServerToolCallsKnown
	// 为 false 表示上游没有给出该字段（例如 Grok Web 路由或旧版网关）。
	ServerToolCalls      int
	ServerToolCallsKnown bool
	// Citations 是按出现顺序收集、按 URL 去重的 url_citation 注解。
	Citations []Citation
	// State 是流完整性判定结果；FinishReason 是流中最后一个非空 finish_reason 的原值
	// （未给出时为空串）；Detail 是面向人的一句话说明，State 为 StateComplete 时为空。
	// 不完整不是 error：正文对搜索调用方仍有价值，由各调用方按自身承诺决定取舍。
	State        CompletionState
	FinishReason string
	Detail       string
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
// 调用计数、url_citation 注解与流完整性判定（见 Completion）。搜索层用它区分
// "检索已执行但正文未写引用"与"根本没有检索"两种无来源情形，并据 State 披露
// 截断；Complete / CompleteWithTools 只返回正文，调用它们的代码看不到完整性，
// 因此凡是对"完整"有承诺的调用方都应改用本方法。
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
//
// 完整性判定：[DONE] 与 finish_reason 一起决定 Completion.State（见 resolveCompletion）。
// 普通 EOF 不再等同于正常结束——上游中途关闭连接时正文可能已经缺尾，客户端无法
// 补救，只能把"未确认完整"如实带回给调用方，而不是包装成成功。
func parseSSE(ctx context.Context, body io.Reader) (*Completion, error) {
	reader := bufio.NewReader(body)
	var sb strings.Builder
	res := &Completion{}
	seen := make(map[string]struct{})
	finishReason := ""
	finish := func(gotDone bool) *Completion {
		res.Content = sb.String()
		res.FinishReason = finishReason
		res.State, res.Detail = resolveCompletion(gotDone, finishReason)
		return res
	}
	for {
		line, readErr := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(trimmed[len("data:"):])
			switch {
			case data == "" || data == "[DONE]":
				if data == "[DONE]" {
					return finish(true), nil
				}
			default:
				var chunk streamChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return nil, &Error{Code: CodeUpstreamStatus, Message: "upstream stream error: " + chunk.Error.Message}
					}
					for _, ch := range chunk.Choices {
						sb.WriteString(ch.Delta.Content)
						// 只记录最后一个非空 finish_reason：xAI 在非末尾块也可能给出
						// end_turn 或 null，以最终声明为准。
						if ch.FinishReason != nil {
							if fr := strings.TrimSpace(*ch.FinishReason); fr != "" {
								finishReason = fr
							}
						}
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
				return finish(false), nil
			}
			return nil, classifyTransportError(ctx, readErr)
		}
	}
}

// resolveCompletion 把"是否收到 [DONE]"与最后的 finish_reason 归并为完整性状态和说明。
// finish_reason 优先于 [DONE]：length / content_filter 即使随后跟着 [DONE] 也是残缺；
// stop / end_turn 即使没有 [DONE] 也已由模型确认结束。两者都缺席时才落到 unconfirmed。
func resolveCompletion(gotDone bool, finishReason string) (CompletionState, string) {
	switch finishReason {
	case "stop", "end_turn":
		return StateComplete, ""
	case "length":
		return StateTruncated, "finish_reason=length: the model hit its output token limit, the tail of the answer is missing"
	case "content_filter":
		return StateFiltered, "finish_reason=content_filter: part of the output was removed by the upstream content filter"
	case "":
		if gotDone {
			return StateComplete, ""
		}
		return StateUnconfirmed, "stream ended before the [DONE] terminator and no finish_reason was reported"
	default:
		if gotDone {
			return StateUnconfirmed, fmt.Sprintf("unknown finish_reason %q", finishReason)
		}
		return StateUnconfirmed, fmt.Sprintf("unknown finish_reason %q and the stream ended before the [DONE] terminator", finishReason)
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
	snippet := errorSnippet(body)
	ra := parseRetryAfter(retryAfter)
	switch {
	case status == http.StatusTooManyRequests:
		return &Error{Code: CodeRateLimit, Status: status, Message: "rate limited: " + snippet, Retryable: true, RetryAfter: ra}
	case status == http.StatusNotFound:
		return &Error{Code: CodeModelUnavailable, Status: status, Message: "model or request rejected: " + snippet}
	case status == http.StatusBadRequest:
		// 400 里只有 model_not_found 一类属于"模型不可用"，其余（invalid_tools、参数错误）
		// 归为 invalid_request，避免把请求形态问题误报成模型问题。
		if code := upstreamErrorCode(snippet); code == "" || strings.Contains(code, "model") {
			return &Error{Code: CodeModelUnavailable, Status: status, Message: "model or request rejected: " + snippet}
		}
		return &Error{Code: CodeInvalidRequest, Status: status, Message: "request rejected by upstream: " + snippet}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "auth error: " + snippet}
	case status >= 500:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "upstream server error: " + snippet, Retryable: true, RetryAfter: ra}
	default:
		return &Error{Code: CodeUpstreamStatus, Status: status, Message: "unexpected status: " + snippet}
	}
}

// errorSnippet 从上游错误响应体中截取可读片段：grok2api 在流式请求出错时会在 JSON
// 错误对象后继续追加 SSE 帧（"data: {...}" / "data: [DONE]"），这些帧对调用方没有
// 信息量，只保留第一个 JSON 对象；非 JSON 响应体保留前 500 字节。
func errorSnippet(body string) string {
	snippet := strings.TrimSpace(body)
	if strings.HasPrefix(snippet, "{") {
		var first json.RawMessage
		dec := json.NewDecoder(strings.NewReader(snippet))
		if dec.Decode(&first) == nil {
			snippet = strings.TrimSpace(string(first))
		}
	}
	if idx := strings.Index(snippet, "\ndata:"); idx > 0 {
		snippet = strings.TrimSpace(snippet[:idx])
	}
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	return snippet
}

// upstreamErrorCode 提取 OpenAI 风格错误体 {"error":{"code":...}} 中的 code，失败返回空串。
func upstreamErrorCode(snippet string) string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(snippet), &payload) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(payload.Error.Code))
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
