package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
)

// ToolHandler executes a tool call. The ctx propagates request cancellation
// down to the upstream HTTP call.
type ToolHandler func(ctx context.Context, args map[string]any) (*ToolCallResult, error)

type registeredTool struct {
	def     Tool
	handler ToolHandler
}

// Server is a minimal newline-delimited JSON-RPC MCP server over stdio.
//
// Stage S1 processes messages on a single loop (one in-flight tool call at a
// time). This is sufficient for the minimal closed loop; the bounded worker
// pool, cancellation registry, and backpressure land in stage S2.
type Server struct {
	in          io.Reader
	out         io.Writer
	logger      *slog.Logger
	tools       map[string]registeredTool
	initialized bool
}

// New constructs a Server bound to the given reader/writer (typically
// os.Stdin/os.Stdout). A nil logger discards engine events.
func New(in io.Reader, out io.Writer, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		in:     in,
		out:    out,
		logger: logger,
		tools:  make(map[string]registeredTool),
	}
}

// Register adds a tool to the server. Call before Serve.
func (s *Server) Register(def Tool, handler ToolHandler) {
	s.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// Serve runs the read/dispatch loop until ctx is cancelled or the input
// stream closes (EOF). It returns nil on a clean EOF shutdown.
func (s *Server) Serve(ctx context.Context) error {
	lines := make(chan []byte)
	readErr := make(chan error, 1)

	go func() {
		reader := bufio.NewReader(s.in)
		for {
			line, err := reader.ReadBytes('\n')
			if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 {
				select {
				case lines <- trimmed:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case raw := <-lines:
			resp := s.handle(ctx, raw)
			if resp != nil {
				if err := s.write(resp); err != nil {
					s.logger.Error("mcp.write_failed", "err", err.Error())
				}
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, raw []byte) *JSONRPCResponse {
	var req JSONRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return newError(nil, ErrCodeParseError, "parse error", err.Error())
	}
	if req.JSONRPC != JSONRPCVersion {
		return newError(req.ID, ErrCodeInvalidRequest, "invalid JSON-RPC version", req.JSONRPC)
	}

	switch req.Method {
	case "initialize":
		return newSuccess(req.ID, InitializeResult{
			ProtocolVersion: MCPProtocolVersion,
			Capabilities:    ServerCapabilities{Tools: &ToolsCapability{ListChanged: false}},
			ServerInfo:      ServerInfo{Name: ServerName, Version: ServerVersion},
		})
	case "notifications/initialized":
		s.initialized = true
		return nil
	case "notifications/cancelled":
		return nil
	case "ping":
		return newSuccess(req.ID, map[string]any{})
	case "tools/list":
		return newSuccess(req.ID, ToolsListResult{Tools: s.toolDefs()})
	case "tools/call":
		return s.handleToolCall(ctx, &req)
	case "shutdown":
		return newSuccess(req.ID, nil)
	default:
		if req.ID == nil { // unknown notification: no reply
			return nil
		}
		return newError(req.ID, ErrCodeMethodNotFound, "method not found", req.Method)
	}
}

func (s *Server) handleToolCall(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	if req.Params == nil {
		return newError(req.ID, ErrCodeInvalidParams, "missing params", nil)
	}
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return newError(req.ID, ErrCodeInvalidParams, "invalid params", err.Error())
	}
	tool, ok := s.tools[params.Name]
	if !ok {
		return newError(req.ID, ErrCodeMethodNotFound, "unknown tool", params.Name)
	}

	result, err := tool.handler(ctx, params.Arguments)
	if err != nil {
		// Tool failures are surfaced as a visible isError result so the
		// calling model sees the structured error rather than a silent gap.
		s.logger.Info("mcp.tool_error", "tool", params.Name, "err", err.Error())
		return newSuccess(req.ID, ToolCallResult{
			IsError: true,
			Content: []ContentItem{{Type: "text", Text: err.Error()}},
		})
	}
	return newSuccess(req.ID, result)
}

func (s *Server) toolDefs() []Tool {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]Tool, 0, len(names))
	for _, name := range names {
		defs = append(defs, s.tools[name].def)
	}
	return defs
}

func (s *Server) write(resp *JSONRPCResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(payload)+1)
	frame = append(frame, payload...)
	frame = append(frame, '\n')
	_, err = s.out.Write(frame)
	return err
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
