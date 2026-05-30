package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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
			ServerInfo:      ServerInfo{Name: ServerName, Version: ServerVersion},
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
