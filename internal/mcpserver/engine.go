package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// methodToolsCall is the only JSON-RPC method dispatched onto the worker
// pool; every other method is sub-millisecond and runs on the recv loop.
const methodToolsCall = "tools/call"

// Engine tunables. Defaults target a balanced footprint for an interactive
// MCP client; callers can override via EngineConfig (wired from env in S2).
const (
	DefaultMaxConcurrentRequests = 8
	DefaultRequestQueueSize      = 64
	DefaultShutdownTimeout       = 30 * time.Second

	maxConcurrentRequestsCap = 100
	requestQueueSizeCap      = 10000

	// forceCancelGrace is the brief window the engine still waits after
	// firing the engine-level cancel during a timed-out shutdown.
	forceCancelGrace = 2 * time.Second
)

// EngineConfig tunes the dispatch engine. Zero values fall back to the
// Default* constants.
type EngineConfig struct {
	// MaxConcurrentRequests caps tools/call handlers running in parallel.
	MaxConcurrentRequests int
	// RequestQueueSize bounds the buffered queue between the recv loop and
	// the worker pool. When full, a synchronous fallback runs the job on
	// the recv loop so requests are never silently dropped.
	RequestQueueSize int
	// RequestTimeout, if > 0, derives every tools/call ctx with a timeout.
	// Zero (the default) leaves per-operation timeouts to the search layer's
	// resilience profiles.
	RequestTimeout time.Duration
	// ShutdownTimeout bounds how long Run waits for in-flight workers after
	// the recv loop stops before forcibly cancelling them.
	ShutdownTimeout time.Duration
}

func (c EngineConfig) normalized() EngineConfig {
	if c.MaxConcurrentRequests <= 0 {
		c.MaxConcurrentRequests = DefaultMaxConcurrentRequests
	} else if c.MaxConcurrentRequests > maxConcurrentRequestsCap {
		c.MaxConcurrentRequests = maxConcurrentRequestsCap
	}
	if c.RequestQueueSize <= 0 {
		c.RequestQueueSize = DefaultRequestQueueSize
	} else if c.RequestQueueSize > requestQueueSizeCap {
		c.RequestQueueSize = requestQueueSizeCap
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	return c
}

type engineMetrics struct {
	inflight  atomic.Int64
	completed atomic.Int64
	fallback  atomic.Int64
}

// job is one unit of asynchronous work pushed onto the work queue.
type job struct {
	ctx     context.Context
	cancel  context.CancelFunc
	id      any
	method  string
	raw     []byte
	enqueue time.Time
}

// Engine is the concurrent MCP dispatch engine. A single recv loop reads
// frames from the Conn; tools/call requests run on a bounded worker pool;
// every other method runs synchronously on the recv loop. Per-request ctxs
// are registered in the CancellationRegistry so notifications/cancelled can
// interrupt the matching worker. Ported from openPic-mcp's server engine.
type Engine struct {
	conn    Conn
	handler Handler
	cancels *CancellationRegistry
	cfg     EngineConfig
	logger  *slog.Logger

	workQueue chan job
	inflight  sync.WaitGroup
	started   atomic.Bool
	metrics   engineMetrics
}

// NewEngine constructs an inert engine; call Run to drive it. A nil logger
// discards events; a nil cancels registry disables cancel publishing.
func NewEngine(conn Conn, handler Handler, cancels *CancellationRegistry, cfg EngineConfig, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg = cfg.normalized()
	return &Engine{
		conn:      conn,
		handler:   handler,
		cancels:   cancels,
		cfg:       cfg,
		logger:    logger,
		workQueue: make(chan job, cfg.RequestQueueSize),
	}
}

// Config returns the effective configuration after defaults/caps.
func (e *Engine) Config() EngineConfig { return e.cfg }

// Inflight returns the number of in-flight requests.
func (e *Engine) Inflight() int64 { return e.metrics.inflight.Load() }

// Completed returns the total number of completed requests.
func (e *Engine) Completed() int64 { return e.metrics.completed.Load() }

// FallbackCount returns the number of requests serviced via the queue-full
// synchronous fallback path.
func (e *Engine) FallbackCount() int64 { return e.metrics.fallback.Load() }

// QueueDepth returns the current work-queue depth.
func (e *Engine) QueueDepth() int { return len(e.workQueue) }

// Run drives the engine until ctx is cancelled or the connection ends, then
// performs graceful shutdown. It is single-shot.
func (e *Engine) Run(ctx context.Context) error {
	if !e.started.CompareAndSwap(false, true) {
		return errors.New("mcpserver: engine already started")
	}

	// engineCtx is the long-lived parent for per-request ctxs. It is
	// detached from the caller's ctx so the recv loop can stop on caller
	// cancellation while in-flight workers keep running until
	// ShutdownTimeout elapses.
	engineCtx, engineCancel := context.WithCancel(context.Background())
	defer engineCancel()

	var workersWG sync.WaitGroup
	workersWG.Add(e.cfg.MaxConcurrentRequests)
	for i := 0; i < e.cfg.MaxConcurrentRequests; i++ {
		go func() {
			defer workersWG.Done()
			e.worker()
		}()
	}
	e.logger.Info("mcp.engine_started",
		"workers", e.cfg.MaxConcurrentRequests,
		"queue_size", e.cfg.RequestQueueSize,
	)

	runErr := e.recvLoop(ctx, engineCtx)

	// Graceful shutdown: close the queue so workers drain buffered jobs and
	// exit. The connection stays open so in-flight workers can flush their
	// replies before it is closed.
	close(e.workQueue)
	waitDone := make(chan struct{})
	go func() {
		e.inflight.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(e.cfg.ShutdownTimeout):
		e.logger.Warn("mcp.shutdown_timeout_exceeded", "inflight", e.Inflight())
		engineCancel()
		select {
		case <-waitDone:
		case <-time.After(forceCancelGrace):
			e.logger.Warn("mcp.shutdown_force_abandon", "inflight", e.Inflight())
		}
	}

	_ = e.conn.Close()
	workersWG.Wait()
	e.logger.Info("mcp.engine_stopped",
		"completed", e.Completed(),
		"fallback", e.FallbackCount(),
	)
	return runErr
}

// recvLoop is the single-goroutine read driver.
func (e *Engine) recvLoop(ctx, engineCtx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		raw, err := e.conn.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("mcpserver: read: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		method, id := peekMethodAndID(raw)

		// Non-tools/call traffic stays on the recv loop. inflight.Add is
		// done here in the single recv-loop goroutine so no Add can race
		// with the shutdown-time inflight.Wait().
		if method != methodToolsCall {
			e.inflight.Add(1)
			e.metrics.inflight.Add(1)
			e.runOnLoop(engineCtx, raw, id, method)
			continue
		}

		reqCtx, reqCancel := e.deriveRequestContext(engineCtx)
		if e.cancels != nil && id != nil {
			e.cancels.Register(id, reqCancel)
		}
		j := job{ctx: reqCtx, cancel: reqCancel, id: id, method: method, raw: raw, enqueue: time.Now()}

		e.inflight.Add(1)
		e.metrics.inflight.Add(1)
		select {
		case e.workQueue <- j:
		default:
			// Queue full: synchronous fallback on the recv loop — strictly
			// better than dropping the request.
			e.metrics.fallback.Add(1)
			e.logger.Warn("mcp.queue_full_fallback", "id", id, "queue_depth", len(e.workQueue))
			e.runJob(j)
		}
	}
}

// deriveRequestContext builds the per-request ctx and a single cancel func
// that releases both the cancel and (when configured) the timeout, so the
// worker's defer never leaks a context.
func (e *Engine) deriveRequestContext(parent context.Context) (context.Context, context.CancelFunc) {
	reqCtx, baseCancel := context.WithCancel(parent)
	if e.cfg.RequestTimeout <= 0 {
		return reqCtx, baseCancel
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(reqCtx, e.cfg.RequestTimeout)
	return timeoutCtx, func() {
		timeoutCancel()
		baseCancel()
	}
}

func (e *Engine) worker() {
	for j := range e.workQueue {
		e.runJob(j)
	}
}

// runOnLoop services non-tools/call traffic synchronously on the recv loop.
func (e *Engine) runOnLoop(engineCtx context.Context, raw []byte, id any, method string) {
	defer func() {
		e.metrics.inflight.Add(-1)
		e.metrics.completed.Add(1)
		e.inflight.Done()
	}()
	e.execute(engineCtx, raw, id, method, time.Now())
}

// runJob executes one queued tools/call job under its per-request ctx. It is
// the only place that pairs Done with the cancel so the registry never
// leaks even if the handler panics.
func (e *Engine) runJob(j job) {
	defer func() {
		if e.cancels != nil && j.id != nil {
			e.cancels.Done(j.id)
		}
		j.cancel()
		e.metrics.inflight.Add(-1)
		e.metrics.completed.Add(1)
		e.inflight.Done()
	}()
	e.execute(j.ctx, j.raw, j.id, j.method, j.enqueue)
}

// execute dispatches one request to the handler, writes the response (if
// any), and is resilient to handler panics so a rogue tool cannot tear down
// a worker goroutine.
func (e *Engine) execute(ctx context.Context, raw []byte, id any, method string, enqueueAt time.Time) {
	start := time.Now()
	defer func() {
		if rec := recover(); rec != nil {
			e.logger.Error("mcp.req_panic", "id", id, "method", method, "recovered", fmt.Sprintf("%v", rec))
		}
	}()

	resp, err := e.handler.HandleMessage(ctx, raw)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e.logger.Info("mcp.req_cancelled", "id", id, "method", method, "reason", err.Error())
			return
		}
		e.logger.Error("mcp.req_failed", "id", id, "method", method, "err", err.Error())
		return
	}
	if resp == nil { // notification: no reply by design
		return
	}

	payload, err := json.Marshal(resp)
	if err != nil {
		e.logger.Error("mcp.encode_failed", "id", id, "err", err.Error())
		return
	}
	if err := e.conn.Write(ctx, payload); err != nil {
		e.logger.Error("mcp.write_failed", "id", id, "err", err.Error())
		return
	}
	e.logger.Debug("mcp.req_completed",
		"id", id, "method", method,
		"duration_ms", time.Since(start).Milliseconds(),
		"queue_wait_ms", start.Sub(enqueueAt).Milliseconds(),
	)
}

// peekMethodAndID decodes only the envelope's method and id for routing,
// without allocating the full request body.
func peekMethodAndID(raw []byte) (string, any) {
	var hdr struct {
		Method string `json:"method"`
		ID     any    `json:"id"`
	}
	_ = json.Unmarshal(raw, &hdr)
	return hdr.Method, hdr.ID
}
