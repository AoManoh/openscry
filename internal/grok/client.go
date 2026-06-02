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

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete performs a streaming chat/completions call with the given system
// prompt and user content, returning the accumulated assistant content. It
// is the shared primitive behind both the search path (SearchPrompt) and the
// Grok-assisted fetch path (FetchPrompt). Any failure is returned as a
// structured *Error; the model is never silently changed.
func (c *Client) Complete(ctx context.Context, model, systemPrompt, userContent string) (string, error) {
	if _, ok := ctx.Deadline(); !ok && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	payload, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Stream: true,
	})
	if err != nil {
		return "", &Error{Code: CodeDecode, Message: fmt.Sprintf("marshal request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", &Error{Code: CodeConnect, Message: fmt.Sprintf("build request: %v", err)}
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
		return "", err
	}
	defer release()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", classifyTransportError(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", classifyStatusError(resp.StatusCode, string(body), resp.Header.Get("Retry-After"))
	}

	content, err := parseSSE(ctx, resp.Body)
	if err != nil {
		return "", err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "", &Error{Code: CodeEmpty, Message: "upstream returned empty content", Retryable: true}
	}
	return content, nil
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
// an explicit error object in the stream is surfaced.
func parseSSE(ctx context.Context, body io.Reader) (string, error) {
	reader := bufio.NewReader(body)
	var sb strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); strings.HasPrefix(trimmed, "data:") {
			data := strings.TrimSpace(trimmed[len("data:"):])
			switch {
			case data == "" || data == "[DONE]":
				if data == "[DONE]" {
					return sb.String(), nil
				}
			default:
				var chunk streamChunk
				if json.Unmarshal([]byte(data), &chunk) == nil {
					if chunk.Error != nil && chunk.Error.Message != "" {
						return "", &Error{Code: CodeUpstreamStatus, Message: "upstream stream error: " + chunk.Error.Message}
					}
					for _, ch := range chunk.Choices {
						sb.WriteString(ch.Delta.Content)
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return sb.String(), nil
			}
			return "", classifyTransportError(ctx, readErr)
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
