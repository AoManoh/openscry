package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubConn is a Conn driven by tests: feed frames in, capture frames out.
// Every write is timestamped so tests can assert latency, not just order.
type stubConn struct {
	in    chan []byte
	mu    sync.Mutex
	out   [][]byte
	outAt []time.Time
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
	c.outAt = append(c.outAt, time.Now())
	c.mu.Unlock()
	return nil
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.out)
}

// writtenResponse 是一帧已写出响应的解码视图，只保留测试关心的字段。
type writtenResponse struct {
	id      int
	isError bool
	text    string
	at      time.Time
}

// responses 按写出顺序解码全部响应帧。tools/call 的结果是 ToolCallResult，其它方法的结果
// 是任意对象；两者都先解成 map 再按需取字段。
func (c *stubConn) responses(t *testing.T) []writtenResponse {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]writtenResponse, 0, len(c.out))
	for i, raw := range c.out {
		var resp JSONRPCResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode frame %d %q: %v", i, raw, err)
		}
		wr := writtenResponse{at: c.outAt[i]}
		if f, ok := resp.ID.(float64); ok {
			wr.id = int(f)
		}
		if result, ok := resp.Result.(map[string]any); ok {
			wr.isError, _ = result["isError"].(bool)
			if content, ok := result["content"].([]any); ok && len(content) > 0 {
				if item, ok := content[0].(map[string]any); ok {
					wr.text, _ = item["text"].(string)
				}
			}
		}
		out = append(out, wr)
	}
	return out
}

// waitForWrites 轮询直到写出 n 帧，超时即失败。饱和场景下响应是异步到达的，测试用它
// 来决定何时关闭输入，避免用固定 sleep 让测试在慢机器上抖动。
func (c *stubConn) waitForWrites(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	waitUntil(t, timeout, func() bool { return c.writeCount() >= n },
		fmt.Sprintf("expected %d writes, got %d", n, c.writeCount()))
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(failMsg)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stubHandler is a throughput-oriented Handler with an injectable delay. The
// delay applies to tools/call only: protocol requests (ping, tools/list) are
// answered at once, mirroring the real protocolHandler, so tests can measure
// whether a saturated queue delays them. began counts tools/call executions
// that have actually started on a worker, letting tests build a saturated
// state step by step instead of racing the worker for the queue head. A
// non-nil gate blocks every tools/call until it is closed.
type stubHandler struct {
	calls atomic.Int64
	began atomic.Int64
	delay time.Duration
	gate  chan struct{}
}

func (h *stubHandler) HandleMessage(ctx context.Context, raw []byte) (*JSONRPCResponse, error) {
	h.calls.Add(1)
	method, id := peekMethodAndID(raw)
	if method == methodToolsCall {
		h.began.Add(1)
	}
	if h.gate != nil && method == methodToolsCall {
		select {
		case <-h.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if h.delay > 0 && method == methodToolsCall {
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

func toolCallFrame(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"x"}}`, id)
}

// feedAndWait 送入一帧并等待引擎到达期望状态。饱和场景的测试必须逐帧推进：接收循环读一帧
// 只需微秒，而 worker 从队首取走任务是另一个协程的调度，若一次性灌入所有帧，“第 2 个请求
// 落在队列里还是等待名单里”会随机漂移，被拒绝的 id 也随之不确定。
func feedAndWait(t *testing.T, conn *stubConn, frame string, cond func() bool, failMsg string) {
	t.Helper()
	conn.feed(frame)
	waitUntil(t, 5*time.Second, cond, failMsg)
}

// saturateSingleWorker 把 1 worker / 队列 1 的引擎推到确定的饱和态：id 1 正在执行，id 2 占满
// 队列，随后 waiting 个请求（id 从 3 起）进入等待名单。返回下一个可用的 id。
func saturateSingleWorker(t *testing.T, conn *stubConn, eng *Engine, h *stubHandler, waiting int) int {
	t.Helper()
	feedAndWait(t, conn, toolCallFrame(1), func() bool { return h.began.Load() == 1 }, "id 1 did not start executing")
	feedAndWait(t, conn, toolCallFrame(2), func() bool { return eng.QueueDepth() == 1 }, "id 2 did not fill the queue")
	next := 3
	for i := 0; i < waiting; i++ {
		want := int64(i + 1)
		feedAndWait(t, conn, toolCallFrame(next), func() bool { return eng.Waiting() == want },
			fmt.Sprintf("id %d did not enter the wait list (want waiting=%d)", next, want))
		next++
	}
	return next
}

// idsOf 返回响应 id 的集合，用于断言“每个请求恰好一个响应”。
func idsOf(resps []writtenResponse) map[int]int {
	m := make(map[int]int, len(resps))
	for _, r := range resps {
		m[r.id]++
	}
	return m
}

func assertOneResponseEach(t *testing.T, resps []writtenResponse, ids ...int) {
	t.Helper()
	got := idsOf(resps)
	if len(resps) != len(ids) {
		t.Fatalf("responses=%d want %d: %v", len(resps), len(ids), got)
	}
	for _, id := range ids {
		if got[id] != 1 {
			t.Fatalf("id=%d got %d responses, want exactly 1 (all: %v)", id, got[id], got)
		}
	}
}

func assertOverloaded(t *testing.T, r writtenResponse, mustContain ...string) {
	t.Helper()
	if !r.isError {
		t.Fatalf("id=%d: expected isError=true overload result, got %q", r.id, r.text)
	}
	if !strings.HasPrefix(r.text, OverloadedMessagePrefix) {
		t.Fatalf("id=%d: overload text must start with %q, got %q", r.id, OverloadedMessagePrefix, r.text)
	}
	for _, s := range append([]string{"in flight", "queue capacity", "waited", "Retry later or reduce"}, mustContain...) {
		if !strings.Contains(r.text, s) {
			t.Fatalf("id=%d: overload text must contain %q, got %q", r.id, s, r.text)
		}
	}
}

func TestEngineConcurrentToolCalls(t *testing.T) {
	const n = 20
	conn := newStubConn(n + 4)
	h := &stubHandler{delay: 2 * time.Millisecond}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 4, RequestQueueSize: 8}, nil)

	for i := 0; i < n; i++ {
		conn.feed(toolCallFrame(i + 1))
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

// TestEngineQueueWaitAdmitsAllWhenSlotsFreeUp 取代原来的
// TestEngineQueueFullFallbackProcessesAll：旧测试验证的是队列满时接收循环同步执行工具的
// 回退路径，该路径会让 ping / tools/list 等协议请求等待整个工具调用，已被删除。新语义下
// 队列满的请求交给等待协程，只要处理速度足以在等待上限内腾出空位，全部请求都应各得一个
// 正常响应、零拒绝，与旧实现“不丢请求”的结果一致，但不再阻塞接收循环。
func TestEngineQueueWaitAdmitsAllWhenSlotsFreeUp(t *testing.T) {
	const n = 30
	conn := newStubConn(n + 4)
	h := &stubHandler{delay: time.Millisecond}
	// 等待名单容量放到 n：本测试关心“等得到就不拒绝”，等待名单溢出由其它测试覆盖。
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 2, RequestQueueSize: 1, MaxQueueWaiters: n}, nil)

	for i := 0; i < n; i++ {
		conn.feed(toolCallFrame(i + 1))
	}
	conn.finish()

	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := eng.Completed(); got != n {
		t.Fatalf("completed=%d want %d (waiting for a slot must not drop requests)", got, n)
	}
	if got := h.calls.Load(); got != int64(n) {
		t.Fatalf("handler calls=%d want %d", got, n)
	}
	if got := eng.RejectedCount(); got != 0 {
		t.Fatalf("rejected=%d want 0 when slots free up within the wait timeout", got)
	}
	if eng.WaitedCount() == 0 {
		t.Fatal("expected at least one request to have waited for a slot (queue size 1, 30 requests)")
	}
	resps := conn.responses(t)
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i + 1
	}
	assertOneResponseEach(t, resps, ids...)
	for _, r := range resps {
		if r.isError {
			t.Fatalf("id=%d unexpectedly rejected: %s", r.id, r.text)
		}
	}
}

// TestEngineSaturatedQueueKeepsProtocolRequestsResponsive 是 D7 的核心断言：1 个 worker、
// 队列 1、三个耗时 tools/call 让队列饱和后，ping 与 tools/list 必须在任何工具调用完成之前
// 就得到响应。旧实现会在接收循环上同步执行第三个调用，ping 要等它结束。
func TestEngineSaturatedQueueKeepsProtocolRequestsResponsive(t *testing.T) {
	const delay = 400 * time.Millisecond
	conn := newStubConn(8)
	h := &stubHandler{delay: delay}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 1, RequestQueueSize: 1, MaxQueueWaiters: 4, QueueWaitTimeout: 5 * time.Second}, nil)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()
	saturateSingleWorker(t, conn, eng, h, 1) // id 1 执行中、id 2 在队列、id 3 在等待名单

	// 饱和态下发出协议请求，记录发出时刻以测量响应时延。
	sent := time.Now()
	conn.feed(`{"jsonrpc":"2.0","id":100,"method":"ping"}`)
	conn.feed(`{"jsonrpc":"2.0","id":101,"method":"tools/list"}`)

	// 五个请求都要有响应；等齐后才关闭输入，确保观察到的是运行中的行为而不是关机路径。
	conn.waitForWrites(t, 5, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 1, 2, 3, 100, 101)
	if resps[0].id != 100 || resps[1].id != 101 {
		t.Fatalf("protocol requests must be answered before any tool call completes; write order: %d, %d, %d, %d, %d",
			resps[0].id, resps[1].id, resps[2].id, resps[3].id, resps[4].id)
	}
	for _, r := range resps[:2] {
		if latency := r.at.Sub(sent); latency >= delay/2 {
			t.Fatalf("id=%d answered after %v; must not wait on the %v tool call", r.id, latency, delay)
		}
	}
	for _, r := range resps[2:] {
		if r.isError {
			t.Fatalf("id=%d unexpectedly rejected (wait timeout was 5s): %s", r.id, r.text)
		}
	}
	if got := eng.RejectedCount(); got != 0 {
		t.Fatalf("rejected=%d want 0", got)
	}
	if got := eng.WaitedCount(); got != 1 {
		t.Fatalf("waited=%d want 1 (only the third call found the queue full)", got)
	}
	if got := h.calls.Load(); got != 5 {
		t.Fatalf("handler calls=%d want 5", got)
	}
}

// TestEngineQueueWaitTimeoutRejectsExcess：等待上限远小于处理时间时，超出 workers + 队列
// 容量的请求在等待上限到期后收到 isError=true 的过载结果，其余请求正常完成，每个请求恰好
// 一个响应。
func TestEngineQueueWaitTimeoutRejectsExcess(t *testing.T) {
	const (
		delay    = 500 * time.Millisecond
		waitSpan = 100 * time.Millisecond
	)
	conn := newStubConn(8)
	h := &stubHandler{delay: delay}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 1, RequestQueueSize: 1, MaxQueueWaiters: 8, QueueWaitTimeout: waitSpan}, nil)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()
	start := time.Now()
	saturateSingleWorker(t, conn, eng, h, 2) // id 1 执行中、id 2 在队列、id 3 与 4 在等待名单

	conn.waitForWrites(t, 4, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 1, 2, 3, 4)
	// 拒绝在等待上限到期时写回，早于第一个工具调用完成。
	for _, r := range resps[:2] {
		if r.id != 3 && r.id != 4 {
			t.Fatalf("expected the first two writes to be the rejected ids 3/4, got id=%d", r.id)
		}
		assertOverloaded(t, r, "no processing slot became free", "workers 1, queue capacity 1", waitSpan.String()+" queue wait timeout")
		if waited := r.at.Sub(start); waited < waitSpan || waited >= delay {
			t.Fatalf("id=%d rejected after %v; want between the %v wait timeout and the %v tool delay", r.id, waited, waitSpan, delay)
		}
	}
	for _, r := range resps[2:] {
		if r.isError || (r.id != 1 && r.id != 2) {
			t.Fatalf("ids 1/2 must complete normally, got id=%d isError=%v text=%q", r.id, r.isError, r.text)
		}
	}
	if got := eng.RejectedCount(); got != 2 {
		t.Fatalf("rejected=%d want 2", got)
	}
	if got := eng.WaitedCount(); got != 0 {
		t.Fatalf("waited=%d want 0 (no waiter obtained a slot in time)", got)
	}
	if got := h.calls.Load(); got != 2 {
		t.Fatalf("handler calls=%d want 2 (rejected calls never reach the handler)", got)
	}
	if got := eng.Completed(); got != 4 {
		t.Fatalf("completed=%d want 4 (rejections still close the request lifecycle)", got)
	}
	if eng.Inflight() != 0 {
		t.Fatalf("inflight=%d want 0", eng.Inflight())
	}
}

// TestEngineQueueWaitNoneRejectsImmediately：QueueWaitNone（配置层的 0）下队列满即刻拒绝，
// 不起等待协程。
func TestEngineQueueWaitNoneRejectsImmediately(t *testing.T) {
	const delay = 300 * time.Millisecond
	conn := newStubConn(8)
	h := &stubHandler{delay: delay}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 1, RequestQueueSize: 1, QueueWaitTimeout: QueueWaitNone}, nil)
	if got := eng.Config().QueueWaitTimeout; got != QueueWaitNone {
		t.Fatalf("normalized QueueWaitTimeout=%v want QueueWaitNone", got)
	}

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()
	saturateSingleWorker(t, conn, eng, h, 0) // id 1 执行中、id 2 在队列，等待名单不可用

	sent := time.Now()
	conn.feed(toolCallFrame(3))
	conn.waitForWrites(t, 3, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 1, 2, 3)
	if resps[0].id != 3 {
		t.Fatalf("the rejection for id 3 must be written first, got id=%d", resps[0].id)
	}
	assertOverloaded(t, resps[0], "waiting for a slot is disabled", "(queue wait disabled)")
	if latency := resps[0].at.Sub(sent); latency >= delay/2 {
		t.Fatalf("immediate rejection took %v; must not wait on the %v tool call", latency, delay)
	}
	if got := eng.RejectedCount(); got != 1 {
		t.Fatalf("rejected=%d want 1", got)
	}
	if got := eng.WaitedCount(); got != 0 || eng.Waiting() != 0 {
		t.Fatalf("waited=%d waiting=%d want 0/0 when waiting is disabled", got, eng.Waiting())
	}
	if got := h.calls.Load(); got != 2 {
		t.Fatalf("handler calls=%d want 2", got)
	}
}

// TestEngineWaitListCapRejectsImmediately：等待协程达到 MaxQueueWaiters 后，再来的请求
// 不排队直接拒绝；已在等待的请求仍能在空位出现时正常执行。
func TestEngineWaitListCapRejectsImmediately(t *testing.T) {
	const delay = 300 * time.Millisecond
	conn := newStubConn(8)
	h := &stubHandler{delay: delay}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 1, RequestQueueSize: 1, MaxQueueWaiters: 1, QueueWaitTimeout: 5 * time.Second}, nil)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()
	saturateSingleWorker(t, conn, eng, h, 1) // id 1 执行中、id 2 在队列、id 3 占满等待名单

	sent := time.Now()
	conn.feed(toolCallFrame(4))
	conn.waitForWrites(t, 4, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 1, 2, 3, 4)
	if resps[0].id != 4 {
		t.Fatalf("the rejection for id 4 must be written first, got id=%d", resps[0].id)
	}
	assertOverloaded(t, resps[0], "wait list are both full", "wait list capacity 1")
	if latency := resps[0].at.Sub(sent); latency >= delay/2 {
		t.Fatalf("wait-list-full rejection took %v; must be immediate", latency)
	}
	for _, r := range resps[1:] {
		if r.isError {
			t.Fatalf("id=%d must complete normally, got %s", r.id, r.text)
		}
	}
	if got := eng.RejectedCount(); got != 1 {
		t.Fatalf("rejected=%d want 1", got)
	}
	if got := eng.WaitedCount(); got != 1 {
		t.Fatalf("waited=%d want 1 (id 3 waited and then ran)", got)
	}
	if got := h.calls.Load(); got != 3 {
		t.Fatalf("handler calls=%d want 3", got)
	}
}

// TestEngineBurstAdmissionMatchesCapacity：一批在引擎启动瞬间到达、数量恰好等于
// workers + 队列 + MaxQueueWaiters 的请求必须全部受理，多出的那个才被拒绝，且结果不随
// worker 是否已经开始取任务而变化。用只数等待协程的判定时，未取走队首任务的空闲 worker 会
// 让本应受理的请求被误拒（TestEngineConcurrentToolCalls 在 -race 下间歇失败就是这个原因）。
func TestEngineBurstAdmissionMatchesCapacity(t *testing.T) {
	const capacity = 2 + 2 + 2
	conn := newStubConn(capacity + 4)
	h := &stubHandler{gate: make(chan struct{})}
	eng := NewEngine(conn, h, NewCancellationRegistry(),
		EngineConfig{MaxConcurrentRequests: 2, RequestQueueSize: 2, MaxQueueWaiters: 2, QueueWaitTimeout: 5 * time.Second}, nil)
	if got := eng.Capacity(); got != capacity {
		t.Fatalf("capacity=%d want %d", got, capacity)
	}

	// 全部帧在 Run 之前灌入：接收循环会在 worker 协程尚未调度到取任务处时就读完它们。
	for i := 1; i <= capacity+1; i++ {
		conn.feed(toolCallFrame(i))
	}
	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()

	// 唯一的拒绝在任何工具调用完成之前就写回（工具都被 gate 挡住）。
	conn.waitForWrites(t, 1, 5*time.Second)
	first := conn.responses(t)[0]
	if first.id != capacity+1 {
		t.Fatalf("the request beyond capacity (id %d) must be the one rejected, got id=%d", capacity+1, first.id)
	}
	assertOverloaded(t, first, "wait list are both full")
	// 拒绝写回后才闭合计数，所以这里轮询而不是直接断言。
	waitUntil(t, 5*time.Second, func() bool { return eng.Inflight() == capacity },
		fmt.Sprintf("inflight must settle at %d admitted requests while the gate is closed", capacity))

	close(h.gate)
	conn.waitForWrites(t, capacity+1, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	ids := make([]int, capacity+1)
	for i := range ids {
		ids[i] = i + 1
	}
	assertOneResponseEach(t, conn.responses(t), ids...)
	if got := eng.RejectedCount(); got != 1 {
		t.Fatalf("rejected=%d want exactly 1", got)
	}
	if got := h.calls.Load(); got != capacity {
		t.Fatalf("handler calls=%d want %d", got, capacity)
	}
	// 至少 MaxQueueWaiters 个请求经过了等待名单；更多只可能是 worker 尚未取走队首任务。
	if got := eng.WaitedCount(); got < 2 || got > 4 {
		t.Fatalf("waited=%d want between 2 and 4", got)
	}
}

// TestEngineCancelledWhileWaitingProducesNoResponse 用真实协议处理器验证：等待空位期间收到
// notifications/cancelled 的请求不产生任何响应，取消注册表与在飞计数都被清理。
func TestEngineCancelledWhileWaitingProducesNoResponse(t *testing.T) {
	conn := newStubConn(8)
	cancels := NewCancellationRegistry()
	handler := newProtocolHandler(cancels, nil)

	release := make(chan struct{})
	var began atomic.Int64
	handler.register(
		Tool{Name: "block", InputSchema: InputSchema{Type: "object"}},
		func(ctx context.Context, _ map[string]any) (*ToolCallResult, error) {
			began.Add(1)
			select {
			case <-release:
				return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: "done"}}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	)
	eng := NewEngine(conn, handler, cancels,
		EngineConfig{MaxConcurrentRequests: 1, RequestQueueSize: 1, MaxQueueWaiters: 2, QueueWaitTimeout: 5 * time.Second}, nil)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()

	// 逐帧推进到确定的饱和态：id=1 在 worker 上阻塞，id=2 占满队列，id=3 进入等待名单。
	blockCall := func(id int) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"block"}}`, id)
	}
	feedAndWait(t, conn, blockCall(1), func() bool { return began.Load() == 1 }, "id 1 did not start executing")
	feedAndWait(t, conn, blockCall(2), func() bool { return eng.QueueDepth() == 1 }, "id 2 did not fill the queue")
	feedAndWait(t, conn, blockCall(3), func() bool { return eng.Waiting() == 1 }, "id 3 did not enter the wait list")
	if got := cancels.Len(); got != 3 {
		t.Fatalf("registry len=%d want 3 while all three are in flight", got)
	}

	conn.feed(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`)
	waitUntil(t, 5*time.Second, func() bool { return eng.Waiting() == 0 }, "waiter for id 3 did not exit on cancellation")
	waitUntil(t, 5*time.Second, func() bool { return eng.Inflight() == 2 }, "inflight must drop to 2 after the waiting request is cancelled")
	if got := cancels.Len(); got != 2 {
		t.Fatalf("registry len=%d want 2 after cancelling the waiting request", got)
	}

	close(release)
	conn.waitForWrites(t, 2, 5*time.Second)
	conn.finish()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 1, 2)
	for _, r := range resps {
		if r.isError {
			t.Fatalf("id=%d must complete normally, got %s", r.id, r.text)
		}
	}
	if got := eng.RejectedCount(); got != 0 {
		t.Fatalf("rejected=%d want 0 (a cancelled waiter is not a rejection)", got)
	}
	if got := eng.WaitedCount(); got != 0 {
		t.Fatalf("waited=%d want 0 (the only waiter was cancelled before a slot freed up)", got)
	}
	if eng.Inflight() != 0 || cancels.Len() != 0 {
		t.Fatalf("inflight=%d registry=%d want 0/0 after run", eng.Inflight(), cancels.Len())
	}
}

// TestEngineShutdownRejectsWaitingRequests：关机预算用尽时仍在等待空位的请求收到过载拒绝
// 并写回结果；工作队列只在等待协程全部退出后才关闭，不会向已关闭通道发送而 panic。
func TestEngineShutdownRejectsWaitingRequests(t *testing.T) {
	conn := newStubConn(8)
	// 工具永不自行结束（只响应取消），迫使关机走预算用尽 + 强制取消路径。
	h := &stubHandler{delay: time.Hour}
	eng := NewEngine(conn, h, NewCancellationRegistry(), EngineConfig{
		MaxConcurrentRequests: 1,
		RequestQueueSize:      1,
		MaxQueueWaiters:       4,
		QueueWaitTimeout:      10 * time.Second,
		ShutdownTimeout:       100 * time.Millisecond,
	}, nil)

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background()) }()
	saturateSingleWorker(t, conn, eng, h, 1) // id 1 执行中、id 2 在队列、id 3 在等待名单

	shutdownAt := time.Now()
	conn.finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine did not shut down; a waiter is probably blocked forever")
	}
	// 关机总时长受 ShutdownTimeout + 强制取消宽限约束；远小于 10s 的等待上限说明等待协程
	// 是被关机信号而不是自身超时叫停的。
	if took := time.Since(shutdownAt); took >= 5*time.Second {
		t.Fatalf("shutdown took %v; waiting requests must be rejected once the shutdown budget is spent", took)
	}

	resps := conn.responses(t)
	assertOneResponseEach(t, resps, 3)
	assertOverloaded(t, resps[0], "shutting down")
	if got := eng.RejectedCount(); got != 1 {
		t.Fatalf("rejected=%d want 1", got)
	}
	if eng.Inflight() != 0 || eng.Waiting() != 0 {
		t.Fatalf("inflight=%d waiting=%d want 0/0 after shutdown", eng.Inflight(), eng.Waiting())
	}
	if got := eng.Completed(); got != 3 {
		t.Fatalf("completed=%d want 3 (two force-cancelled executions plus one rejection)", got)
	}
}

// TestEngineConfigNormalizedQueueWait 固定 EngineConfig 的零值与哨兵约定：零值回落默认值，
// 负值归一到 QueueWaitNone，超上限裁剪，等待名单默认与队列同宽。
func TestEngineConfigNormalizedQueueWait(t *testing.T) {
	cases := []struct {
		name        string
		in          EngineConfig
		wantWait    time.Duration
		wantWaiters int
	}{
		{"zero falls back to defaults", EngineConfig{}, DefaultQueueWaitTimeout, DefaultRequestQueueSize},
		{"explicit value kept", EngineConfig{QueueWaitTimeout: 3 * time.Second, RequestQueueSize: 5}, 3 * time.Second, 5},
		{"sentinel kept", EngineConfig{QueueWaitTimeout: QueueWaitNone}, QueueWaitNone, DefaultRequestQueueSize},
		{"any negative becomes sentinel", EngineConfig{QueueWaitTimeout: -time.Hour}, QueueWaitNone, DefaultRequestQueueSize},
		{"over cap is clamped", EngineConfig{QueueWaitTimeout: time.Hour}, queueWaitTimeoutCap, DefaultRequestQueueSize},
		{"waiters explicit", EngineConfig{RequestQueueSize: 2, MaxQueueWaiters: 7}, DefaultQueueWaitTimeout, 7},
		{"waiters over cap clamped", EngineConfig{MaxQueueWaiters: requestQueueSizeCap + 1}, DefaultQueueWaitTimeout, requestQueueSizeCap},
	}
	for _, c := range cases {
		got := c.in.normalized()
		if got.QueueWaitTimeout != c.wantWait {
			t.Errorf("%s: QueueWaitTimeout=%v want %v", c.name, got.QueueWaitTimeout, c.wantWait)
		}
		if got.MaxQueueWaiters != c.wantWaiters {
			t.Errorf("%s: MaxQueueWaiters=%d want %d", c.name, got.MaxQueueWaiters, c.wantWaiters)
		}
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
