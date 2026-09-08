package search

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/sources"
)

// maxBatchQueries caps a single batch fan-out, mirroring the GrokSearch
// baseline. Extra queries beyond the cap are dropped and reported as skipped.
const maxBatchQueries = 32

// defaultBatchConcurrency bounds concurrent sub-queries when the caller does
// not specify one. The shared circuit breaker + retry budget already bound
// amplification; this caps simultaneous upstream connections.
const defaultBatchConcurrency = 8

// BatchItem is the outcome of one sub-query in a batch. Status is one of:
// "ok" (answer returned), "error" (the sub-query failed), or "skipped" (an
// empty/blank query, or beyond the batch cap). Failures are isolated: one
// sub-query's error never aborts its siblings.
type BatchItem struct {
	Query   string
	Status  string
	Content string
	Sources []sources.Source
	Warning string
	Err     string
	// 与 Result 相同的可观测字段，供批量/异步结果逐项暴露。
	ServerToolCalls      int
	ServerToolCallsKnown bool
	Elapsed              time.Duration
	ExtraSources         *ExtraSourcesReport
	// 与 Result 相同的完整性字段；只在 Status 为 "ok" 时有值，error / skipped 项没有响应可判定。
	CompletionState  grok.CompletionState
	CompletionDetail string
}

// BatchOptions are applied to every sub-query in a batch. Per-query platform
// is intentionally not supported (call Search individually for that), matching
// the GrokSearch baseline.
type BatchOptions struct {
	Platform     string
	Model        string
	ExtraSources int
	Concurrency  int // max concurrent sub-queries; <= 0 uses the default
	// Timeout 是每个子查询各自的显式预算（语义同 Request.Timeout），0 表示使用搜索档位。
	// 它按子查询而不是按整个批次计量：批次内的子查询并发执行，逐条预算才与单次搜索一致。
	Timeout time.Duration
}

// BatchSearch runs many independent queries concurrently with bounded
// parallelism and returns one BatchItem per input query, in input order.
// Blank queries and any beyond maxBatchQueries are marked "skipped" without
// an upstream call. The whole batch shares the service's breaker and retry
// budget, so a widespread upstream failure cannot multiply into a retry storm.
func (s *Service) BatchSearch(ctx context.Context, queries []string, opt BatchOptions) []BatchItem {
	items := make([]BatchItem, len(queries))
	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = defaultBatchConcurrency
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	processed := 0

	for i, q := range queries {
		query := strings.TrimSpace(q)
		items[i].Query = query
		if query == "" {
			items[i].Status = "skipped"
			items[i].Err = "blank query"
			continue
		}
		if processed >= maxBatchQueries {
			items[i].Status = "skipped"
			items[i].Err = "beyond batch cap (32)"
			continue
		}
		processed++

		wg.Add(1)
		go func(idx int, q string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, err := s.Search(ctx, Request{
				Query:        q,
				Platform:     opt.Platform,
				Model:        opt.Model,
				ExtraSources: opt.ExtraSources,
				Timeout:      opt.Timeout,
			})
			if err != nil {
				items[idx].Status = "error"
				items[idx].Err = err.Error()
				return
			}
			items[idx].Status = "ok"
			items[idx].Content = res.Content
			items[idx].Sources = res.Sources
			items[idx].Warning = res.Warning
			items[idx].ServerToolCalls = res.ServerToolCalls
			items[idx].ServerToolCallsKnown = res.ServerToolCallsKnown
			items[idx].Elapsed = res.Elapsed
			items[idx].ExtraSources = res.ExtraSources
			items[idx].CompletionState = res.CompletionState
			items[idx].CompletionDetail = res.CompletionDetail
		}(i, query)
	}
	wg.Wait()
	return items
}
