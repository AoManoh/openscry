package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AoManoh/openscry/internal/version"
)

// ToolHandler executes a tool call. ctx carries request cancellation (from a
// `notifications/cancelled`) and any per-operation timeout down to the
// upstream HTTP call.
type ToolHandler func(ctx context.Context, args map[string]any) (*ToolCallResult, error)

type registeredTool struct {
	def     Tool
	handler ToolHandler
}

// Handler decodes and dispatches a single JSON-RPC frame. The engine drives
// it; tests can stub it.
type Handler interface {
	HandleMessage(ctx context.Context, raw []byte) (*JSONRPCResponse, error)
}

// protocolHandler implements Handler: it owns the tool registry and the MCP
// method switch. Tools are registered before Serve and never mutated after,
// but the registry is guarded so concurrent worker reads are race-free.
type protocolHandler struct {
	mu          sync.RWMutex
	tools       map[string]registeredTool
	cancels     *CancellationRegistry
	logger      *slog.Logger
	initialized atomic.Bool
}

func newProtocolHandler(cancels *CancellationRegistry, logger *slog.Logger) *protocolHandler {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &protocolHandler{
		tools:   make(map[string]registeredTool),
		cancels: cancels,
		logger:  logger,
	}
}

func (h *protocolHandler) register(def Tool, handler ToolHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// HandleMessage decodes one frame and returns the response (nil for
// notifications). It returns a non-nil error only when the response should
// be suppressed (e.g. the request ctx was cancelled mid-flight); structured
// protocol/tool failures are returned as JSON-RPC error or isError
// responses, not Go errors.
func (h *protocolHandler) HandleMessage(ctx context.Context, raw []byte) (*JSONRPCResponse, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return newError(nil, ErrCodeParseError, "parse error", err.Error()), nil
	}
	if req.JSONRPC != JSONRPCVersion {
		return newError(req.ID, ErrCodeInvalidRequest, "invalid JSON-RPC version", req.JSONRPC), nil
	}

	switch req.Method {
	case "initialize":
		return newSuccess(req.ID, InitializeResult{
			ProtocolVersion: MCPProtocolVersion,
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{ListChanged: false}},
			ServerInfo:      ServerInfo{Name: ServerName, Version: version.Value()},
		}), nil
	case "notifications/initialized":
		h.initialized.Store(true)
		return nil, nil
	case "notifications/cancelled":
		h.handleCancelled(req.Params)
		return nil, nil
	case "ping":
		return newSuccess(req.ID, map[string]any{}), nil
	case "tools/list":
		return newSuccess(req.ID, ToolsListResult{Tools: h.toolDefs()}), nil
	case "tools/call":
		return h.handleToolCall(ctx, &req)
	case "shutdown":
		return newSuccess(req.ID, nil), nil
	default:
		if req.ID == nil { // unknown notification: no reply
			return nil, nil
		}
		return newError(req.ID, ErrCodeMethodNotFound, "method not found", req.Method), nil
	}
}

func (h *protocolHandler) handleToolCall(ctx context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if req.Params == nil {
		return newError(req.ID, ErrCodeInvalidParams, "missing params", nil), nil
	}
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newError(req.ID, ErrCodeInvalidParams, "invalid params", err.Error()), nil
	}

	h.mu.RLock()
	tool, ok := h.tools[params.Name]
	h.mu.RUnlock()
	if !ok {
		return newError(req.ID, ErrCodeMethodNotFound, "unknown tool", params.Name), nil
	}

	result, err := tool.handler(ctx, params.Arguments)
	if err != nil {
		// Client cancellation / engine timeout: suppress the response so the
		// engine logs a cancellation rather than writing a stale reply.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Otherwise the failure (including a per-operation timeout on a child
		// ctx) is surfaced as a visible isError result so the calling model
		// sees the structured error rather than a silent gap.
		h.logger.Info("mcp.tool_error", "tool", params.Name, "err", err.Error())
		return newSuccess(req.ID, ToolCallResult{
			IsError: true,
			Content: []ContentItem{{Type: "text", Text: err.Error()}},
		}), nil
	}
	return newSuccess(req.ID, result), nil
}

// handleCancelled extracts the target request id from a
// `notifications/cancelled` payload and cancels the matching in-flight call.
func (h *protocolHandler) handleCancelled(params json.RawMessage) {
	if h.cancels == nil || len(params) == 0 {
		return
	}
	var p struct {
		RequestID any `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.RequestID != nil {
		h.cancels.Cancel(p.RequestID)
	}
}

func (h *protocolHandler) toolDefs() []Tool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.tools))
	for name := range h.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]Tool, 0, len(names))
	for _, name := range names {
		defs = append(defs, h.tools[name].def)
	}
	return defs
}

func newSuccess(id any, result any) *JSONRPCResponse {
	return &JSONRPCResponse{JSONRPC: JSONRPCVersion, ID: id, Result: result}
}

func newError(id any, code int, message string, data any) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message, Data: data},
	}
}

// overloadStatus 是引擎在拒绝一个 tools/call 时的容量快照，供拒绝文案与日志共用同一组数字，
// 避免两处各算一遍而出现口径不一致。
type overloadStatus struct {
	reason      overloadReason
	inflight    int64         // 拒绝时刻仍在处理或等待中的其它请求数
	workers     int           // worker 池大小
	queueCap    int           // 有界队列容量
	waitersCap  int           // 等待协程数量上限
	waited      time.Duration // 该请求实际等待了多久（立即拒绝时为 0）
	waitTimeout time.Duration // 配置的等待上限；QueueWaitNone 表示不等待
}

// newOverloadedResponse 构造队列饱和时写回给 tools/call 的拒绝结果。它是 isError=true 的
// 工具结果而不是 JSON-RPC 错误：与 handleToolCall 对工具执行失败的处理形态一致，MCP 客户端会把
// 文本原样交给模型，模型据此才知道是服务端过载、等了多久、该稍后重试还是降低并发；协议级
// 错误码的语义（解析、参数、方法未找到）保持不变。
func newOverloadedResponse(id any, st overloadStatus) *JSONRPCResponse {
	return newSuccess(id, ToolCallResult{
		IsError: true,
		Content: []ContentItem{{Type: "text", Text: overloadedMessage(st)}},
	})
}

// overloadedMessage 渲染拒绝文案：固定前缀 + 拒绝原因 + 容量数字 + 等待时长 + 建议动作。
// 文案面向调用方的模型，所以每个数字都带上含义，而不是只给一串键值。
func overloadedMessage(st overloadStatus) string {
	var b strings.Builder
	b.WriteString(OverloadedMessagePrefix)
	b.WriteString(": ")
	switch st.reason {
	case overloadWaitTimeout:
		b.WriteString("no processing slot became free within the queue wait timeout")
	case overloadCapacityExhausted:
		b.WriteString("the request queue and the wait list are both full")
	case overloadWaitDisabled:
		b.WriteString("the request queue is full and waiting for a slot is disabled")
	case overloadShutdown:
		b.WriteString("the server is shutting down and could not schedule this call before its shutdown budget ran out")
	default:
		b.WriteString("the request queue is full")
	}
	fmt.Fprintf(&b, ". Load: %d other requests in flight (workers %d, queue capacity %d, wait list capacity %d)",
		st.inflight, st.workers, st.queueCap, st.waitersCap)
	if st.waitTimeout == QueueWaitNone {
		fmt.Fprintf(&b, "; this call waited %s (queue wait disabled)", formatWait(st.waited))
	} else {
		fmt.Fprintf(&b, "; this call waited %s of the %s queue wait timeout", formatWait(st.waited), st.waitTimeout)
	}
	b.WriteString(". Retry later or reduce the number of concurrent tool calls.")
	return b.String()
}

// formatWait 以 0.1s 精度渲染等待时长，避免文案里出现纳秒级的长尾数字。
func formatWait(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}
