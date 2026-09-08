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

	// DefaultQueueWaitTimeout 是队列已满的 tools/call 等待空位的默认上限，与
	// config.DefaultQueueWaitTimeout 保持一致，让直接使用引擎的调用方与通过环境变量配置的
	// 进程得到相同行为。
	DefaultQueueWaitTimeout = 10 * time.Second

	// QueueWaitNone 让 EngineConfig.QueueWaitTimeout 表达“队列满时立即拒绝”。EngineConfig 的
	// 零值约定是回落默认值，所以 0 不能同时表示“不等待”，只能用一个显式的负哨兵值；任何
	// 负值在 normalized 中都会归一到它。
	QueueWaitNone time.Duration = -1

	maxConcurrentRequestsCap = 100
	requestQueueSizeCap      = 10000
	queueWaitTimeoutCap      = 10 * time.Minute

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
	// the worker pool. When full, the request is handed to a bounded wait
	// list (see QueueWaitTimeout / MaxQueueWaiters) or rejected with a
	// visible isError tool result; the recv loop never runs a tool itself.
	RequestQueueSize int
	// RequestTimeout, if > 0, derives every tools/call ctx with a timeout.
	// Zero (the default) leaves per-operation timeouts to the search layer's
	// resilience profiles.
	RequestTimeout time.Duration
	// ShutdownTimeout bounds how long Run waits for in-flight workers after
	// the recv loop stops before forcibly cancelling them.
	ShutdownTimeout time.Duration
	// QueueWaitTimeout 是队列已满时一个 tools/call 最多等待空位多久。零值回落
	// DefaultQueueWaitTimeout；QueueWaitNone（任何负值）表示不等待、立即拒绝；超过
	// 10 分钟裁剪，避免把等待配置成事实上的无限挂起。
	QueueWaitTimeout time.Duration
	// MaxQueueWaiters 是等待名单的宽度：在飞 tools/call 总数达到
	// MaxConcurrentRequests + RequestQueueSize + MaxQueueWaiters 后，新请求立即拒绝。零值
	// 回落为 RequestQueueSize：等待名单与队列同宽，总受理容量 = workers + 2 * 队列容量，
	// 既能吸收一倍队列容量的突发，又不会让等待协程数量失控。准入按在飞总数而不是等待协程
	// 数判断，原因见 recvLoop。
	MaxQueueWaiters int
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
	switch {
	case c.QueueWaitTimeout == 0:
		c.QueueWaitTimeout = DefaultQueueWaitTimeout
	case c.QueueWaitTimeout < 0:
		c.QueueWaitTimeout = QueueWaitNone
	case c.QueueWaitTimeout > queueWaitTimeoutCap:
		c.QueueWaitTimeout = queueWaitTimeoutCap
	}
	if c.MaxQueueWaiters <= 0 {
		c.MaxQueueWaiters = c.RequestQueueSize
	} else if c.MaxQueueWaiters > requestQueueSizeCap {
		c.MaxQueueWaiters = requestQueueSizeCap
	}
	return c
}

type engineMetrics struct {
	inflight  atomic.Int64
	completed atomic.Int64
	rejected  atomic.Int64
	waited    atomic.Int64
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
//
// 队列已满的 tools/call 不会在接收循环上执行：接收循环只做非阻塞入队，满则交给有界的
// 等待协程在 QueueWaitTimeout 内竞争空位，等不到或等待名单也满时以过载工具错误拒绝。
// 这样 ping / tools/list / initialize 等协议请求在任何负载下都能立即得到响应，MCP 客户端
// 不会因为 ping 超时而误判连接失效。
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

	// waiters 追踪等待协程；Run 必须在关闭 workQueue 前等它归零，否则等待协程向已关闭的
	// 通道发送会 panic。waiting 是当前等待协程数量，只由接收循环递增，供日志、拒绝文案与测试
	// 观测（准入本身按在飞总数判断，见 recvLoop）。stopWaiting 在关机预算用尽时关闭，让仍在
	// 等待的请求立即以过载拒绝。
	waiters     sync.WaitGroup
	waiting     atomic.Int64
	stopWaiting chan struct{}
}

// NewEngine constructs an inert engine; call Run to drive it. A nil logger
// discards events; a nil cancels registry disables cancel publishing.
func NewEngine(conn Conn, handler Handler, cancels *CancellationRegistry, cfg EngineConfig, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg = cfg.normalized()
	return &Engine{
		conn:        conn,
		handler:     handler,
		cancels:     cancels,
		cfg:         cfg,
		logger:      logger,
		workQueue:   make(chan job, cfg.RequestQueueSize),
		stopWaiting: make(chan struct{}),
	}
}

// Config returns the effective configuration after defaults/caps.
func (e *Engine) Config() EngineConfig { return e.cfg }

// Inflight returns the number of in-flight requests (executing, queued, or
// waiting for a queue slot).
func (e *Engine) Inflight() int64 { return e.metrics.inflight.Load() }

// Capacity returns the maximum number of tools/call requests the engine admits
// at once: MaxConcurrentRequests + RequestQueueSize + MaxQueueWaiters. Beyond
// it, a tools/call is rejected immediately with an overloaded tool result.
func (e *Engine) Capacity() int64 {
	return int64(e.cfg.MaxConcurrentRequests + e.cfg.RequestQueueSize + e.cfg.MaxQueueWaiters)
}

// Completed returns the number of requests whose lifecycle has ended, whatever
// the outcome: executed (including those cancelled mid-flight), cancelled while
// waiting for a queue slot, or rejected. RejectedCount is the subset that was
// never handed to a handler.
func (e *Engine) Completed() int64 { return e.metrics.completed.Load() }

// RejectedCount returns the number of tools/call requests refused with an
// overloaded tool result because the queue was full and no slot became free
// within the wait budget (or waiting was disabled / the wait list was full /
// the engine was shutting down).
func (e *Engine) RejectedCount() int64 { return e.metrics.rejected.Load() }

// WaitedCount returns the number of tools/call requests that found the queue
// full, waited, and then obtained a slot within QueueWaitTimeout.
func (e *Engine) WaitedCount() int64 { return e.metrics.waited.Load() }

// Waiting returns the number of tools/call requests currently waiting for a
// queue slot.
func (e *Engine) Waiting() int64 { return e.waiting.Load() }

// QueueDepth returns the current work-queue depth.
func (e *Engine) QueueDepth() int { return len(e.workQueue) }

// queueWaitLabel 把等待上限渲染成日志里的可读值：不等待时输出 0s，与配置层
// GROK_QUEUE_WAIT_TIMEOUT=0 的写法对应，负哨兵值不泄漏到运维视角。
func (e *Engine) queueWaitLabel() string {
	if e.cfg.QueueWaitTimeout == QueueWaitNone {
		return "0s"
	}
	return e.cfg.QueueWaitTimeout.String()
}

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
		"queue_wait_timeout", e.queueWaitLabel(),
		"max_queue_waiters", e.cfg.MaxQueueWaiters,
	)

	runErr := e.recvLoop(ctx, engineCtx)

	// 关机分两段，共用一个 ShutdownTimeout 预算。
	//
	// 第一段等等待协程自行退出：此时 workers 仍在消费队列，等待中的请求会陆续拿到空位，
	// 或按自己的 QueueWaitTimeout 超时拒绝。EOF 前刚到达的一批请求因此不会被关机一刀切
	// 拒绝，尽量保留“已受理的请求会被处理”的承诺。预算用尽仍在等待的请求通过 stopWaiting
	// 立即以过载拒绝并写回结果。只有确认没有等待协程还可能发送后才能关闭 workQueue，向已
	// 关闭的通道发送会 panic。
	deadline := time.Now().Add(e.cfg.ShutdownTimeout)
	waitersDone := make(chan struct{})
	go func() {
		e.waiters.Wait()
		close(waitersDone)
	}()
	select {
	case <-waitersDone:
	case <-time.After(time.Until(deadline)):
		e.logger.Warn("mcp.shutdown_rejecting_waiters", "waiting", e.Waiting())
		close(e.stopWaiting)
		<-waitersDone
	}

	// 第二段：关闭队列让 workers 排空缓冲的任务后退出。连接保持打开，在飞 worker 才能在
	// 关闭前把响应写完；剩余预算用尽则强制取消。
	close(e.workQueue)
	inflightDone := make(chan struct{})
	go func() {
		e.inflight.Wait()
		close(inflightDone)
	}()
	select {
	case <-inflightDone:
	case <-time.After(time.Until(deadline)):
		e.logger.Warn("mcp.shutdown_timeout_exceeded", "inflight", e.Inflight())
		engineCancel()
		select {
		case <-inflightDone:
		case <-time.After(forceCancelGrace):
			e.logger.Warn("mcp.shutdown_force_abandon", "inflight", e.Inflight())
		}
	}

	_ = e.conn.Close()
	workersWG.Wait()
	e.logger.Info("mcp.engine_stopped",
		"completed", e.Completed(),
		"rejected", e.RejectedCount(),
		"waited", e.WaitedCount(),
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

		// 准入只看在飞总数（此时已计入当前请求），且在决定放进队列还是等待名单之前统一判断。
		// 不能只在队列满时数等待协程：刚派生的等待协程可能还没来得及阻塞在发送上，此时
		// worker 取走队首任务腾出的空位会被后到的请求直接占走，等待中的请求被越过，在飞总数
		// 就会超过 workers + 队列 + MaxQueueWaiters；反过来，尚未取走队首任务的空闲 worker 又会
		// 让只数等待协程的判定把本应受理的请求误拒。按在飞总数准入，受理容量恒等于
		// Capacity，不随 worker 与等待协程的调度时机漂移。inflight 只由接收循环递增，Load 与
		// 后续放置之间不会有别的协程受理新的请求。
		if e.metrics.inflight.Load() > e.Capacity() {
			e.reject(j, overloadCapacityExhausted, 0)
			continue
		}
		// 只做非阻塞入队。接收循环一旦在这里阻塞或同步执行工具，后面的 ping / tools/list
		// 就要等一个耗时的工具调用结束，客户端会误判连接失效。
		select {
		case e.workQueue <- j:
		default:
			e.admitWaiting(j)
		}
	}
}

// admitWaiting 处理队列已满但仍在受理容量内的 tools/call，只在接收循环上调用：配置为不等待
// 则立即拒绝，否则交给一个等待协程在 QueueWaitTimeout 内竞争空位。
func (e *Engine) admitWaiting(j job) {
	if e.cfg.QueueWaitTimeout == QueueWaitNone {
		e.reject(j, overloadWaitDisabled, 0)
		return
	}
	e.waiting.Add(1)
	e.waiters.Add(1)
	e.logger.Debug("mcp.queue_full_waiting", "id", j.id, "queue_depth", len(e.workQueue), "waiting", e.waiting.Load())
	go e.waitForSlot(j)
}

// waitForSlot 在独立协程里替一个 tools/call 等待队列空位。四种出路互斥且各自闭合登记：
// 拿到空位交给 worker（由 runJob 闭合）；请求被取消则不写回结果，与 worker 内的取消路径
// 一致；等待超时或关机预算用尽则写回过载拒绝。
func (e *Engine) waitForSlot(j job) {
	defer e.waiters.Done()
	defer e.waiting.Add(-1)

	timer := time.NewTimer(e.cfg.QueueWaitTimeout)
	defer timer.Stop()

	select {
	case e.workQueue <- j:
		e.metrics.waited.Add(1)
		e.logger.Debug("mcp.queue_wait_admitted", "id", j.id, "waited_ms", time.Since(j.enqueue).Milliseconds())
	case <-j.ctx.Done():
		// notifications/cancelled 已经通过注册表取消了请求上下文（或 RequestTimeout 在等待
		// 期间到期）。调用方已放弃这次调用，不写回任何结果；finish 会把注册表条目清理干净。
		e.logger.Info("mcp.req_cancelled", "id", j.id, "method", j.method, "reason", j.ctx.Err().Error(), "stage", "queue_wait")
		e.finish(j)
	case <-timer.C:
		e.reject(j, overloadWaitTimeout, time.Since(j.enqueue))
	case <-e.stopWaiting:
		e.reject(j, overloadShutdown, time.Since(j.enqueue))
	}
}

// reject 把一个不会再被执行的 tools/call 以过载工具错误写回，然后闭合它的全部登记。写回
// 必须在 finish 之前：finish 会取消请求上下文，而 Conn.Write 以该上下文判断请求是否仍然
// 有效。在飞数在快照时减去自身，让文案里的“其它在飞请求”不把被拒绝的请求算进去。
func (e *Engine) reject(j job, reason overloadReason, waited time.Duration) {
	defer e.finish(j)
	e.metrics.rejected.Add(1)
	st := overloadStatus{
		reason:      reason,
		inflight:    e.metrics.inflight.Load() - 1,
		workers:     e.cfg.MaxConcurrentRequests,
		queueCap:    e.cfg.RequestQueueSize,
		waitersCap:  e.cfg.MaxQueueWaiters,
		waited:      waited,
		waitTimeout: e.cfg.QueueWaitTimeout,
	}
	e.logger.Warn("mcp.queue_full_rejected",
		"id", j.id,
		"reason", string(reason),
		"inflight", st.inflight,
		"queue_depth", len(e.workQueue),
		"waiting", e.waiting.Load(),
		"waited_ms", waited.Milliseconds(),
	)
	if j.id == nil {
		// 没有 id 的 tools/call 是 JSON-RPC 通知，协议规定不得回复；只留日志。
		return
	}
	e.writeResponse(j.ctx, j.id, newOverloadedResponse(j.id, st))
}

// finish 闭合一个 tools/call 的全部登记：取消注册表条目、请求上下文、在飞计数与完成计数。
// runJob、等待期取消与拒绝三条路径都必须且只能调用一次，否则关机时 inflight.Wait 会提前
// 返回或永久阻塞。
func (e *Engine) finish(j job) {
	if e.cancels != nil && j.id != nil {
		e.cancels.Done(j.id)
	}
	j.cancel()
	e.metrics.inflight.Add(-1)
	e.metrics.completed.Add(1)
	e.inflight.Done()
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

// runJob executes one queued tools/call job under its per-request ctx. The
// deferred finish pairs Done with the cancel so the registry never leaks even
// if the handler panics.
func (e *Engine) runJob(j job) {
	defer e.finish(j)
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
	if !e.writeResponse(ctx, id, resp) {
		return
	}
	e.logger.Debug("mcp.req_completed",
		"id", id, "method", method,
		"duration_ms", time.Since(start).Milliseconds(),
		"queue_wait_ms", start.Sub(enqueueAt).Milliseconds(),
	)
}

// writeResponse 编码并写出一个响应，失败只记日志。返回是否写出成功，供调用方决定是否记录
// 完成日志。execute 与 reject 共用，保证两条路径的编码与写失败处理一致。
func (e *Engine) writeResponse(ctx context.Context, id any, resp *JSONRPCResponse) bool {
	payload, err := json.Marshal(resp)
	if err != nil {
		e.logger.Error("mcp.encode_failed", "id", id, "err", err.Error())
		return false
	}
	if err := e.conn.Write(ctx, payload); err != nil {
		e.logger.Error("mcp.write_failed", "id", id, "err", err.Error())
		return false
	}
	return true
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
