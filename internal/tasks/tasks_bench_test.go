package tasks

import (
	"context"
	"testing"
	"time"
)

// 本文件的基准只为容量决策提供数据：Store 的容量上限（defaultMaxTasks = 256）与超容驱逐策略
// 是否需要调整，取决于 Submit 在满容时的开销以及运行中任务被驱逐的速率。两条基准分别覆盖
// evictLocked 的两条路径：第一遍（驱逐最旧终态任务）与第二遍（全部未结束时取消并删除最旧任务）。
//
// 两条基准都在 store 已满的稳态下计时，并额外以 ns/submit 报告单独计量的 Submit 耗时：
// 终态路径的迭代需要等新任务结束才能保持"全部终态"的前提，标准 ns/op 会把这段等待算进去，
// ns/submit 只含 Submit 调用本身，两条基准之间用它比较才是同一口径。

// blockUntilCancelled 是阻塞型 runner：只在 ctx 取消时返回，模拟一直未结束的上游搜索。
func blockUntilCancelled(ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// finishImmediately 立即返回，任务几乎在提交后立刻进入终态。
func finishImmediately(context.Context) (any, error) {
	return "ok", nil
}

// BenchmarkSubmitEvictInFlightAtCapacity 在容量 256 的 store 里持续提交阻塞型任务：预填 256 个
// 运行中任务后，每次 Submit 都走第二遍驱逐——扫描全部 256 条记录找不到终态任务，再取消并删除
// 最旧的运行中任务。ns/op 与 ns/submit 是 Submit 的耗时，submits/s 是提交吞吐。
func BenchmarkSubmitEvictInFlightAtCapacity(b *testing.B) {
	// 父上下文在基准结束时取消，让所有仍在运行的任务退出，避免 goroutine 泄漏到后续基准。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := NewStore()
	for i := 0; i < defaultMaxTasks; i++ {
		s.Submit(ctx, "web_search", nil, blockUntilCancelled)
	}

	var inSubmit time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		s.Submit(ctx, "web_search", nil, blockUntilCancelled)
		inSubmit += time.Since(start)
	}
	b.StopTimer()
	b.ReportMetric(float64(inSubmit.Nanoseconds())/float64(b.N), "ns/submit")
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "submits/s")

	// 清理：取消剩余的 256 个运行中任务并等它们进入终态，被驱逐的任务已经各自因取消退出。
	cancel()
	for _, snap := range s.List(nil, nil, time.Time{}) {
		waitTerminal(b, s, snap.ID)
	}
}

// BenchmarkSubmitEvictTerminalAtCapacity 在 256 个终态任务已满的 store 里继续提交：每次 Submit
// 走第一遍驱逐——扫描 order 找到最旧的终态任务并删除。每次迭代都等待新任务结束，下一次提交
// 面对的才仍是"全部终态"的前提；因此 ns/op 包含等待，ns/submit 只含 Submit 调用本身。
func BenchmarkSubmitEvictTerminalAtCapacity(b *testing.B) {
	s := NewStore()
	for i := 0; i < defaultMaxTasks; i++ {
		waitTerminal(b, s, s.Submit(context.Background(), "web_search", nil, finishImmediately).ID)
	}

	var inSubmit time.Duration
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		snap := s.Submit(context.Background(), "web_search", nil, finishImmediately)
		inSubmit += time.Since(start)
		waitTerminal(b, s, snap.ID)
	}
	b.StopTimer()
	b.ReportMetric(float64(inSubmit.Nanoseconds())/float64(b.N), "ns/submit")
}
