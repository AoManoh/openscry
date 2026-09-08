// Package mcpserver is the thin MCP adapter that exposes the openscry search
// core over stdio JSON-RPC. The wire types mirror the MCP/JSON-RPC shapes
// validated by openPic-mcp so existing MCP clients (Windsurf/Cascade) work
// without change. Dispatch runs on a single concurrent engine (bounded
// worker pool + bounded queue + bounded wait list + cancellation registry);
// there is no synchronous/concurrent dual track, and the recv loop never
// executes a tool call itself.
package mcpserver

import "encoding/json"

// Protocol constants.
const (
	JSONRPCVersion     = "2.0"
	MCPProtocolVersion = "2024-11-05"
	ServerName         = "openscry-mcp"
)

// JSON-RPC 2.0 error codes (subset) plus an openscry tool-execution code.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
	ErrCodeToolExecution  = -32000
)

// OverloadedMessagePrefix 是引擎在请求队列饱和、无法受理 tools/call 时写回的工具错误文本的
// 固定开头。过载拒绝不复用任何 JSON-RPC 错误码：它以 isError=true 的工具结果返回，与工具
// 自身的执行失败同一形态，调用方的模型能直接读到原因并决定重试；固定前缀让客户端与测试
// 无需解析全文即可把它同其它工具错误区分开。
const OverloadedMessagePrefix = "openscry MCP server overloaded"

// overloadReason 标识拒绝发生在哪条路径。它进入日志字段与拒绝文案，便于运维聚合和测试
// 断言，不进入协议层的错误码。
type overloadReason string

const (
	// overloadWaitTimeout：在等待队列空位的上限内没有等到。
	overloadWaitTimeout overloadReason = "wait_timeout"
	// overloadCapacityExhausted：在飞请求已达 workers + 队列 + 等待名单的总容量，不再受理。
	overloadCapacityExhausted overloadReason = "capacity_exhausted"
	// overloadWaitDisabled：队列已满且配置为不等待（QueueWaitTimeout 为 QueueWaitNone）。
	overloadWaitDisabled overloadReason = "wait_disabled"
	// overloadShutdown：引擎关机时该请求仍在等待空位，关机预算用尽后被拒绝。
	overloadShutdown overloadReason = "shutdown"
)

// JSONRPCRequest is an incoming JSON-RPC 2.0 message.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// JSONRPCResponse is an outgoing JSON-RPC 2.0 message.
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError is the error object of a JSON-RPC response.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolsCapability advertises tool support.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerCapabilities advertises server capabilities.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// InitializeResult is the response to the initialize request.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// Property is a single JSON-Schema property in a tool input schema.
type Property struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// InputSchema is the JSON-Schema for a tool's arguments.
type InputSchema struct {
	Type                 string              `json:"type"`
	Properties           map[string]Property `json:"properties,omitempty"`
	Required             []string            `json:"required,omitempty"`
	AdditionalProperties bool                `json:"additionalProperties"`
}

// Tool is an MCP tool definition.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"inputSchema"`
}

// ToolsListResult is the response to tools/list.
type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

// ToolCallParams are the parameters of a tools/call request.
type ToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ContentItem is one piece of tool result content.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCallResult is the result of a tools/call. IsError=true surfaces a
// tool-level failure visibly to the calling model (per the openscry
// "degradation must be visible" principle) without using a JSON-RPC error.
type ToolCallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}
