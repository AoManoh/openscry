package mcpserver

import (
	"context"
	"io"
	"log/slog"
)

// Server is the public facade for the MCP adapter: register tools, then
// Serve over stdio. It builds the concurrent Engine (bounded worker pool +
// bounded queue + cancellation registry) internally — there is a single
// dispatch implementation, with no synchronous/concurrent dual track.
type Server struct {
	in      io.Reader
	out     io.Writer
	logger  *slog.Logger
	handler *protocolHandler
	cancels *CancellationRegistry
	cfg     EngineConfig
}

// New constructs a Server with default engine tunables, bound to the given
// reader/writer (typically os.Stdin/os.Stdout). A nil logger discards
// engine events.
func New(in io.Reader, out io.Writer, logger *slog.Logger) *Server {
	return NewWithConfig(in, out, logger, EngineConfig{})
}

// NewWithConfig is New with explicit engine tunables (worker pool size,
// queue size, request/shutdown timeouts). Zero-valued fields fall back to
// the Default* constants.
func NewWithConfig(in io.Reader, out io.Writer, logger *slog.Logger, cfg EngineConfig) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cancels := NewCancellationRegistry()
	return &Server{
		in:      in,
		out:     out,
		logger:  logger,
		handler: newProtocolHandler(cancels, logger),
		cancels: cancels,
		cfg:     cfg,
	}
}

// Register adds a tool to the server. Call before Serve.
func (s *Server) Register(def Tool, handler ToolHandler) {
	s.handler.register(def, handler)
}

// Serve builds the stdio connection and concurrent engine and runs it until
// ctx is cancelled or the input stream closes (EOF). It returns nil on a
// clean EOF shutdown.
func (s *Server) Serve(ctx context.Context) error {
	conn := newStdioConn(s.in, s.out)
	engine := NewEngine(conn, s.handler, s.cancels, s.cfg, s.logger)
	return engine.Run(ctx)
}
