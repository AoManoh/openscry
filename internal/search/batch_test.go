package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
)

// echoGrokStub answers each query by echoing the user content's last line, so
// per-query results are distinguishable.
func echoGrokStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 系统提示词变长后单次 Read 可能只读到前半段而漏掉末尾的用户查询，必须读完整个 body
		body, _ := io.ReadAll(r.Body)
		answer := "ok"
		if strings.Contains(string(body), "FAIL") {
			w.WriteHeader(http.StatusNotFound) // non-retryable model error
			_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonQuote(answer) + "}}]}\n"))
		// 查询含 CUT 时模拟上游在 [DONE] 之前结束响应：正文已写出，但完整性未确认。
		if strings.Contains(string(body), "CUT") {
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

func TestBatchSearchExposesCompletionPerItem(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	items := svc.BatchSearch(context.Background(), []string{"whole", "CUT short", "FAIL", ""}, BatchOptions{})
	whole, cut, failed, skipped := items[0], items[1], items[2], items[3]

	if whole.Status != "ok" || whole.CompletionState != grok.StateComplete || whole.CompletionDetail != "" {
		t.Fatalf("complete item = %+v, want ok/complete without detail", whole)
	}
	if strings.Contains(whole.Warning, "not confirmed complete") {
		t.Fatalf("complete item must not carry a completeness warning: %q", whole.Warning)
	}

	// 不完整的子查询仍是 ok：正文保留，状态与原因逐项暴露，告警可见。
	if cut.Status != "ok" || cut.Content != "ok" {
		t.Fatalf("incomplete item must stay ok with its content, got %+v", cut)
	}
	if cut.CompletionState != grok.StateUnconfirmed || !strings.Contains(cut.CompletionDetail, "[DONE]") {
		t.Fatalf("incomplete item completion = %q / %q, want unconfirmed mentioning [DONE]", cut.CompletionState, cut.CompletionDetail)
	}
	if !strings.Contains(cut.Warning, "not confirmed complete") || !strings.Contains(cut.Warning, string(grok.StateUnconfirmed)) {
		t.Fatalf("incomplete item warning must disclose the state, got %q", cut.Warning)
	}

	// 失败与跳过的子查询没有响应可判定，完整性字段保持零值。
	if failed.Status != "error" || failed.CompletionState != "" || failed.CompletionDetail != "" {
		t.Fatalf("error item must carry no completion state, got %+v", failed)
	}
	if skipped.Status != "skipped" || skipped.CompletionState != "" {
		t.Fatalf("skipped item must carry no completion state, got %+v", skipped)
	}
}

func TestBatchSearchAllOK(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	items := svc.BatchSearch(context.Background(), []string{"a", "b", "c"}, BatchOptions{})
	if len(items) != 3 {
		t.Fatalf("got %d items want 3", len(items))
	}
	for i, it := range items {
		if it.Status != "ok" {
			t.Fatalf("item[%d] status=%s err=%s", i, it.Status, it.Err)
		}
		if it.Content != "ok" {
			t.Fatalf("item[%d] content=%q", i, it.Content)
		}
	}
}

func TestBatchSearchSkipsBlank(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	items := svc.BatchSearch(context.Background(), []string{"a", "   ", ""}, BatchOptions{})
	if items[0].Status != "ok" {
		t.Fatalf("item0=%s", items[0].Status)
	}
	if items[1].Status != "skipped" || items[2].Status != "skipped" {
		t.Fatalf("blank queries not skipped: %s %s", items[1].Status, items[2].Status)
	}
}

func TestBatchSearchIsolatesFailure(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	// The middle query triggers a non-retryable model error; siblings still OK.
	items := svc.BatchSearch(context.Background(), []string{"good1", "FAIL", "good2"}, BatchOptions{})
	if items[0].Status != "ok" || items[2].Status != "ok" {
		t.Fatalf("siblings should succeed: %s %s", items[0].Status, items[2].Status)
	}
	if items[1].Status != "error" || items[1].Err == "" {
		t.Fatalf("failing query should be error with message: %+v", items[1])
	}
}

func TestBatchSearchPreservesOrder(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	queries := []string{"q0", "q1", "q2", "q3", "q4"}
	items := svc.BatchSearch(context.Background(), queries, BatchOptions{Concurrency: 2})
	for i, it := range items {
		if it.Query != queries[i] {
			t.Fatalf("order broken at %d: got %q want %q", i, it.Query, queries[i])
		}
	}
}

func TestBatchSearchCapsAt32(t *testing.T) {
	srv := echoGrokStub(t)
	defer srv.Close()
	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")

	queries := make([]string, 40)
	for i := range queries {
		queries[i] = "q"
	}
	items := svc.BatchSearch(context.Background(), queries, BatchOptions{})
	okCount, skipCount := 0, 0
	for _, it := range items {
		switch it.Status {
		case "ok":
			okCount++
		case "skipped":
			skipCount++
		}
	}
	if okCount != maxBatchQueries {
		t.Fatalf("ok=%d want %d", okCount, maxBatchQueries)
	}
	if skipCount != 40-maxBatchQueries {
		t.Fatalf("skipped=%d want %d", skipCount, 40-maxBatchQueries)
	}
}

func TestBatchSearchAppliesTimeoutPerQuery(t *testing.T) {
	// 查询含 SLOW 的子查询 4s 后才响应，其余立即响应；BatchOptions.Timeout=200ms 逐条生效：
	// 快的子查询成功，慢的子查询以超时错误结束，整批在预算附近返回而不是等到 4s。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "SLOW") {
			select {
			case <-time.After(4 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer srv.Close()
	svc := NewWithOptions(grok.NewClient(srv.URL, "k", 30*time.Second), "grok-test", Options{MaxAttempts: 1})

	started := time.Now()
	items := svc.BatchSearch(context.Background(), []string{"fast", "SLOW one"}, BatchOptions{Timeout: 200 * time.Millisecond})
	elapsed := time.Since(started)

	if items[0].Status != "ok" || items[0].Content != "ok" {
		t.Fatalf("fast item = %+v, want ok", items[0])
	}
	if items[1].Status != "error" || !strings.Contains(items[1].Err, string(grok.CodeTimeout)) {
		t.Fatalf("slow item = %+v, want a timeout error", items[1])
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("batch took %v, want the per-query budget (200ms) to bound it", elapsed)
	}
}
