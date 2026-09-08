package tasks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// waitTerminal 接受 testing.TB，测试与基准（tasks_bench_test.go）共用同一个等待助手。
func waitTerminal(t testing.TB, s *Store, id string) Snapshot {
	t.Helper()
	snap, ok := s.Get(context.Background(), id, 2*time.Second)
	if !ok {
		t.Fatalf("task %s not found", id)
	}
	if !snap.State.terminal() {
		t.Fatalf("task %s not terminal after wait: %s", id, snap.State)
	}
	return snap
}

func TestSubmitCompletes(t *testing.T) {
	s := NewStore()
	snap := s.Submit(context.Background(), "web_search", map[string]any{"query": "q"},
		func(context.Context) (any, error) { return "the result", nil })
	if snap.State != StateQueued && snap.State != StateRunning {
		t.Fatalf("initial state=%s", snap.State)
	}
	final := waitTerminal(t, s, snap.ID)
	if final.State != StateCompleted {
		t.Fatalf("state=%s want completed", final.State)
	}
	if final.Result != "the result" {
		t.Fatalf("result=%v", final.Result)
	}
	if final.StartedAt == nil || final.FinishedAt == nil {
		t.Fatal("timestamps not set")
	}
}

func TestSubmitFails(t *testing.T) {
	s := NewStore()
	snap := s.Submit(context.Background(), "web_search", nil,
		func(context.Context) (any, error) { return nil, errors.New("boom") })
	final := waitTerminal(t, s, snap.ID)
	if final.State != StateFailed {
		t.Fatalf("state=%s want failed", final.State)
	}
	if final.Err != "boom" {
		t.Fatalf("err=%q", final.Err)
	}
}

func TestCancelInFlight(t *testing.T) {
	s := NewStore()
	started := make(chan struct{})
	snap := s.Submit(context.Background(), "web_search", nil, func(ctx context.Context) (any, error) {
		close(started)
		<-ctx.Done() // block until cancelled
		return nil, ctx.Err()
	})
	<-started
	final, ok := s.Cancel(snap.ID, "user")
	if !ok {
		t.Fatal("cancel returned not-found")
	}
	if final.State != StateCancelled {
		t.Fatalf("state=%s want cancelled", final.State)
	}
	if final.CancelHint != "user" {
		t.Fatalf("hint=%q want user", final.CancelHint)
	}
}

func TestCancelTerminalIsNoop(t *testing.T) {
	s := NewStore()
	snap := s.Submit(context.Background(), "web_search", nil,
		func(context.Context) (any, error) { return "done", nil })
	waitTerminal(t, s, snap.ID)
	final, ok := s.Cancel(snap.ID, "late")
	if !ok || final.State != StateCompleted {
		t.Fatalf("cancel of terminal task changed state: ok=%v state=%s", ok, final.State)
	}
}

func TestGetWaitReturnsOnCompletion(t *testing.T) {
	s := NewStore()
	snap := s.Submit(context.Background(), "web_search", nil, func(context.Context) (any, error) {
		time.Sleep(50 * time.Millisecond)
		return "ok", nil
	})
	// Immediate get should be non-terminal.
	if now, _ := s.Get(context.Background(), snap.ID, 0); now.State.terminal() {
		t.Fatal("expected non-terminal on immediate get")
	}
	// Long-poll should observe completion.
	final, _ := s.Get(context.Background(), snap.ID, time.Second)
	if final.State != StateCompleted {
		t.Fatalf("state=%s want completed", final.State)
	}
}

func TestGetUnknownID(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get(context.Background(), "nope", 0); ok {
		t.Fatal("expected not-found for unknown id")
	}
}

func TestListFilters(t *testing.T) {
	s := NewStore()
	done := s.Submit(context.Background(), "web_search", nil,
		func(context.Context) (any, error) { return "x", nil })
	waitTerminal(t, s, done.ID)
	s.Submit(context.Background(), "web_search_batch", nil, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	all := s.List(nil, nil, time.Time{})
	if len(all) != 2 {
		t.Fatalf("list all=%d want 2", len(all))
	}
	completed := s.List([]State{StateCompleted}, nil, time.Time{})
	if len(completed) != 1 || completed[0].State != StateCompleted {
		t.Fatalf("completed filter=%v", completed)
	}
	batch := s.List(nil, []string{"web_search_batch"}, time.Time{})
	if len(batch) != 1 || batch[0].Kind != "web_search_batch" {
		t.Fatalf("kind filter=%v", batch)
	}
}

func TestEvictionDropsTerminalFirst(t *testing.T) {
	s := NewStore(WithMaxTasks(2))
	// Submit 3 completed tasks; the oldest terminal should be evicted.
	var ids []string
	for i := 0; i < 3; i++ {
		snap := s.Submit(context.Background(), "web_search", map[string]any{"n": i},
			func(context.Context) (any, error) { return "ok", nil })
		ids = append(ids, snap.ID)
		waitTerminal(t, s, snap.ID)
	}
	if _, ok := s.Get(context.Background(), ids[0], 0); ok {
		t.Fatal("oldest terminal task should have been evicted")
	}
	if _, ok := s.Get(context.Background(), ids[2], 0); !ok {
		t.Fatal("newest task should be retained")
	}
}

func TestEvictionKeepsInFlightOverTerminal(t *testing.T) {
	s := NewStore(WithMaxTasks(2))
	// One in-flight task that blocks until cancelled.
	block := s.Submit(context.Background(), "web_search", nil, func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	// A terminal task fills the second slot.
	done1 := s.Submit(context.Background(), "web_search", nil,
		func(context.Context) (any, error) { return "ok1", nil })
	waitTerminal(t, s, done1.ID)
	// A third task forces eviction. Terminal-first means done1 is evicted and
	// the in-flight block task survives.
	done2 := s.Submit(context.Background(), "web_search", nil,
		func(context.Context) (any, error) { return "ok2", nil })
	waitTerminal(t, s, done2.ID)
	if _, ok := s.Get(context.Background(), block.ID, 0); !ok {
		t.Fatal("in-flight task must not be evicted before terminal ones")
	}
	if _, ok := s.Get(context.Background(), done1.ID, 0); ok {
		t.Fatal("oldest terminal task should have been evicted")
	}
	s.Cancel(block.ID, "cleanup")
}

// TestEvictionCancelsOldestInFlightWhenOverCapacity 记录当前的超容策略：容量内全部是运行中
// 任务时再提交一个，store 不拒绝新提交，而是取消并删除最旧的运行中任务（evictLocked 的第二遍
// 驱逐），该任务随即对 Get 不可见；最新提交的任务照常运行并可完成。
//
// 这是对现状的记录性测试，固定的是当前策略而不是对该策略的背书：正在运行的任务会在调用方
// 不知情的情况下消失。后续若把超容行为改为拒绝新提交（或其它策略），必须同步修改本测试。
func TestEvictionCancelsOldestInFlightWhenOverCapacity(t *testing.T) {
	const capacity = 3
	s := NewStore(WithMaxTasks(capacity))

	// release 放行所有仍在运行的任务，让它们以 completed 结束；被驱逐的任务在此之前就会
	// 因 ctx 取消退出，并通过自己的 cancelled 通道报告。
	release := make(chan struct{})
	type inflight struct {
		snap      Snapshot
		cancelled chan struct{}
	}
	submitRunning := func() inflight {
		started := make(chan struct{})
		cancelled := make(chan struct{})
		snap := s.Submit(context.Background(), "web_search", nil, func(ctx context.Context) (any, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(cancelled)
				return nil, ctx.Err()
			case <-release:
				return "ok", nil
			}
		})
		// run() 在调用 runner 之前已把状态置为 running，等到 started 即保证任务处于运行中。
		<-started
		return inflight{snap: snap, cancelled: cancelled}
	}

	var older []inflight
	for i := 0; i < capacity; i++ {
		older = append(older, submitRunning())
	}
	for _, task := range older {
		snap, ok := s.Get(context.Background(), task.snap.ID, 0)
		if !ok || snap.State != StateRunning {
			t.Fatalf("task %s before overflow: ok=%v state=%s want running", task.snap.ID, ok, snap.State)
		}
	}

	// 第 capacity+1 个运行中任务触发超容。
	newest := submitRunning()

	oldest := older[0]
	select {
	case <-oldest.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("oldest in-flight task was not cancelled by over-capacity eviction")
	}
	if _, ok := s.Get(context.Background(), oldest.snap.ID, 0); ok {
		t.Fatal("oldest in-flight task should have been dropped from the store (current policy: cancel + delete)")
	}
	for _, task := range older[1:] {
		snap, ok := s.Get(context.Background(), task.snap.ID, 0)
		if !ok || snap.State != StateRunning {
			t.Fatalf("task %s after overflow: ok=%v state=%s want running", task.snap.ID, ok, snap.State)
		}
	}
	if got := len(s.List(nil, nil, time.Time{})); got != capacity {
		t.Fatalf("store holds %d tasks after overflow, want exactly the capacity %d", got, capacity)
	}

	// 放行后，最新任务与其余幸存任务都正常完成。
	close(release)
	final := waitTerminal(t, s, newest.snap.ID)
	if final.State != StateCompleted || final.Result != "ok" {
		t.Fatalf("newest task state=%s result=%v want completed/ok", final.State, final.Result)
	}
	for _, task := range older[1:] {
		if snap := waitTerminal(t, s, task.snap.ID); snap.State != StateCompleted {
			t.Fatalf("surviving task %s state=%s want completed", task.snap.ID, snap.State)
		}
	}
}

func TestParamsPreservedInSnapshot(t *testing.T) {
	s := NewStore()
	params := map[string]any{"query": "hello", "n": 3}
	snap := s.Submit(context.Background(), "web_search", params,
		func(context.Context) (any, error) { return "x", nil })
	got := waitTerminal(t, s, snap.ID)
	if fmt.Sprint(got.Params["query"]) != "hello" {
		t.Fatalf("params not preserved: %v", got.Params)
	}
}
