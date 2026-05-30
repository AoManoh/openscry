package mcpserver

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubConn is a Conn driven by tests: feed frames in, capture frames out.
type stubConn struct {
	in  chan []byte
	mu  sync.Mutex
	out [][]byte
}

func newStubConn(capacity int) *stubConn {
	return &stubConn{in: make(chan []byte, capacity)}
}

func (c *stubConn) feed(raw string) { c.in <- []byte(raw) }
func (c *stubConn) finish()         { close(c.in) }

func (c *stubConn) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case f, ok := <-c.in:
		if !ok {
			return nil, io.EOF
		}
		return f, nil
	}
}

func (c *stubConn) Write(ctx context.Context, p []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	c.mu.Lock()
	c.out = append(c.out, cp)
	c.mu.Unlock()
	return nil
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.out)
}

// stubHandler is a throughput-oriented Handler with an injectable delay.
type stubHandler struct {
	calls atomic.Int64
	delay time.Duration
}

func (h *stubHandler) HandleMessage(ctx context.Context, raw []byte) (*JSONRPCResponse, error) {
	h.calls.Add(1)
	_, id := peekMethodAndID(raw)
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if id == nil {
		return nil, nil
	}
	return &JSONRPCResponse{JSONRPC: JSONRPCVersion, ID: id, Result: map[string]any{"ok": true}}, nil
}

func TestEngineConcurrentToolCalls(t *testing.T) {
	const n = 20
	conn := newStubConn(n + 4)
	h := &stubHandler{delay: 2 * time.Millisecond}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 4, RequestQueueSize: 8}, nil)

	for i := 0; i < n; i++ {
		conn.feed(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"x"}}`, i+1))
	}
	conn.finish()

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := eng.Completed(); got != n {
		t.Fatalf("completed=%d want %d", got, n)
	}
	if got := conn.writeCount(); got != n {
		t.Fatalf("writes=%d want %d", got, n)
	}
	if got := h.calls.Load(); got != int64(n) {
		t.Fatalf("handler calls=%d want %d", got, n)
	}
	if eng.Inflight() != 0 {
		t.Fatalf("inflight=%d want 0 after run", eng.Inflight())
	}
}

func TestEngineQueueFullFallbackProcessesAll(t *testing.T) {
	const n = 30
	conn := newStubConn(n + 4)
	// Small pool + tiny queue + a handler delay forces the queue-full
	// synchronous fallback path; all requests must still complete.
	h := &stubHandler{delay: time.Millisecond}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 2, RequestQueueSize: 1}, nil)

	for i := 0; i < n; i++ {
		conn.feed(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"x"}}`, i+1))
	}
	conn.finish()

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := eng.Completed(); got != n {
		t.Fatalf("completed=%d want %d (queue-full fallback must not drop requests)", got, n)
	}
	if got := conn.writeCount(); got != n {
		t.Fatalf("writes=%d want %d", got, n)
	}
}

// TestEngineCancellationInterruptsHandler exercises the real protocol handler
// so notifications/cancelled actually drives the cancellation registry that
// the engine populated, interrupting the in-flight tools/call worker.
func TestEngineCancellationInterruptsHandler(t *testing.T) {
	conn := newStubConn(8)
	cancels := NewCancellationRegistry()
	handler := newProtocolHandler(cancels, nil)

	started := make(chan struct{})
	interrupted := make(chan struct{})
	handler.register(
		Tool{Name: "block", InputSchema: InputSchema{Type: "object"}},
		func(ctx context.Context, _ map[string]any) (*ToolCallResult, error) {
			close(started)
			<-ctx.Done() // block until the cancellation arrives
			close(interrupted)
			return nil, ctx.Err()
		},
	)

	eng := NewEngine(conn, handler, cancels,
		EngineConfig{MaxConcurrentRequests: 2, RequestQueueSize: 4}, nil)

	conn.feed(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"block"}}`)
	go func() {
		<-started
		conn.feed(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
		<-interrupted
		conn.finish()
	}()

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()

	select {
	case <-interrupted:
	case <-time.After(3 * time.Second):
		t.Fatal("tools/call handler was not interrupted by notifications/cancelled")
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}
