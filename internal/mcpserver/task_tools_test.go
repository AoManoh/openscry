package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/search"
	"github.com/AoManoh/openscry/internal/tasks"
)

func stubSearchSvc(t *testing.T) *search.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer body\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	t.Cleanup(srv.Close)
	return search.New(grok.NewClient(srv.URL, "k", 5*time.Second), "grok-test")
}

func callTool(t *testing.T, h ToolHandler, args map[string]any) map[string]any {
	t.Helper()
	res, err := h(context.Background(), args)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("result not JSON: %v (%q)", err, res.Content[0].Text)
	}
	return out
}

func TestSubmitAndGetTaskLifecycle(t *testing.T) {
	svc := stubSearchSvc(t)
	store := tasks.NewStore()
	_, submit := SubmitSearchTaskTool(store, svc)
	_, get := GetSearchTaskResultTool(store)

	out := callTool(t, submit, map[string]any{"kind": "web_search", "query": "hello"})
	id, _ := out["task_id"].(string)
	if id == "" {
		t.Fatalf("no task_id in submit result: %v", out)
	}

	// Long-poll until terminal.
	got := callTool(t, get, map[string]any{"task_id": id, "wait": "2s"})
	if got["state"] != "completed" {
		t.Fatalf("state=%v want completed: %v", got["state"], got)
	}
	result, ok := got["result"].(map[string]any)
	if !ok || result["content"] != "answer body" {
		t.Fatalf("unexpected result: %v", got["result"])
	}
}

func TestSubmitRejectsUnknownKind(t *testing.T) {
	svc := stubSearchSvc(t)
	store := tasks.NewStore()
	_, submit := SubmitSearchTaskTool(store, svc)
	if _, err := submit(context.Background(), map[string]any{"kind": "bogus"}); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestSubmitWebSearchRequiresQuery(t *testing.T) {
	svc := stubSearchSvc(t)
	store := tasks.NewStore()
	_, submit := SubmitSearchTaskTool(store, svc)
	if _, err := submit(context.Background(), map[string]any{"kind": "web_search"}); err == nil {
		t.Fatal("expected error for missing query")
	}
}

func TestWebSearchBatchTool(t *testing.T) {
	svc := stubSearchSvc(t)
	_, batch := WebSearchBatchTool(svc)
	out := callTool(t, batch, map[string]any{"queries": []any{"a", "b"}})
	if out["count"].(float64) != 2 {
		t.Fatalf("count=%v want 2", out["count"])
	}
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results len=%d", len(results))
	}
	first := results[0].(map[string]any)
	if first["status"] != "ok" || first["content"] != "answer body" {
		t.Fatalf("first result=%v", first)
	}
}

func TestGetUnknownTaskErrors(t *testing.T) {
	store := tasks.NewStore()
	_, get := GetSearchTaskResultTool(store)
	if _, err := get(context.Background(), map[string]any{"task_id": "missing"}); err == nil {
		t.Fatal("expected error for unknown task id")
	}
}

func TestCancelTaskTool(t *testing.T) {
	store := tasks.NewStore()
	_, cancel := CancelSearchTaskTool(store)
	// Submit a blocking task directly via the store.
	started := make(chan struct{})
	snap := store.Submit(context.Background(), "web_search", nil, func(ctx context.Context) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	out := callTool(t, cancel, map[string]any{"task_id": snap.ID, "hint": "user"})
	if out["state"] != "cancelled" {
		t.Fatalf("state=%v want cancelled", out["state"])
	}
}

func TestListTasksTool(t *testing.T) {
	svc := stubSearchSvc(t)
	store := tasks.NewStore()
	_, submit := SubmitSearchTaskTool(store, svc)
	_, list := ListSearchTasksTool(store)

	for i := 0; i < 3; i++ {
		callTool(t, submit, map[string]any{"kind": "web_search", "query": "q"})
	}
	time.Sleep(100 * time.Millisecond) // let them finish
	out := callTool(t, list, map[string]any{})
	if out["count"].(float64) != 3 {
		t.Fatalf("count=%v want 3", out["count"])
	}
}

func TestParseWaitClampsAndValidates(t *testing.T) {
	if d, _ := parseWait(""); d != 0 {
		t.Fatalf("empty wait=%v want 0", d)
	}
	if d, _ := parseWait("10m"); d != maxTaskWait {
		t.Fatalf("10m wait=%v want clamp to %v", d, maxTaskWait)
	}
	if _, err := parseWait("nonsense"); err == nil {
		t.Fatal("expected error for invalid wait")
	}
}
