package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/search"
	"github.com/AoManoh/openscry/internal/tasks"
)

// maxTaskWait caps the long-poll duration for get_search_task_result, matching
// the GrokSearch baseline's 5-minute ceiling.
const maxTaskWait = 5 * time.Minute

// validTaskKinds are the task kinds that submit_search_task accepts.
var validTaskKinds = map[string]struct{}{
	"web_search":       {},
	"web_search_batch": {},
}

// searchResultMap renders a single search result into the task/JSON shape
// shared by the async runner and the synchronous batch tool.
func searchResultMap(res *search.Result) map[string]any {
	m := map[string]any{
		"content":       res.Content,
		"sources_count": len(res.Sources),
	}
	if len(res.Sources) > 0 {
		m["sources"] = res.Sources
	}
	if res.Warning != "" {
		m["warning"] = res.Warning
	}
	return m
}

// batchItemsResult renders batch items into a JSON-friendly result map.
func batchItemsResult(items []search.BatchItem) map[string]any {
	out := make([]map[string]any, len(items))
	for i, it := range items {
		entry := map[string]any{
			"query":  it.Query,
			"status": it.Status,
		}
		if it.Content != "" {
			entry["content"] = it.Content
			entry["sources_count"] = len(it.Sources)
		}
		if len(it.Sources) > 0 {
			entry["sources"] = it.Sources
		}
		if it.Warning != "" {
			entry["warning"] = it.Warning
		}
		if it.Err != "" {
			entry["error"] = it.Err
		}
		out[i] = entry
	}
	return map[string]any{"count": len(items), "results": out}
}

// buildSearchRunner constructs a task Runner for the given kind from its
// params. It returns an error for an unknown kind so submit_search_task can
// fail loud rather than enqueue an unrunnable task.
func buildSearchRunner(svc *search.Service, kind string, params map[string]any) (tasks.Runner, error) {
	switch kind {
	case "web_search":
		query, _ := params["query"].(string)
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("submit_search_task: web_search requires a non-empty 'query'")
		}
		return func(ctx context.Context) (any, error) {
			res, err := svc.Search(ctx, search.Request{
				Query:        query,
				Platform:     asString(params["platform"]),
				Model:        asString(params["model"]),
				ExtraSources: asInt(params["extra_sources"]),
			})
			if err != nil {
				return nil, err
			}
			return searchResultMap(res), nil
		}, nil
	case "web_search_batch":
		queries := asStringSlice(params["queries"])
		if len(queries) == 0 {
			return nil, fmt.Errorf("submit_search_task: web_search_batch requires a non-empty 'queries' list")
		}
		return func(ctx context.Context) (any, error) {
			items := svc.BatchSearch(ctx, queries, search.BatchOptions{
				Platform:     asString(params["platform"]),
				Model:        asString(params["model"]),
				ExtraSources: asInt(params["extra_sources"]),
			})
			return batchItemsResult(items), nil
		}, nil
	default:
		return nil, fmt.Errorf("submit_search_task: unknown kind %q (want web_search or web_search_batch)", kind)
	}
}

// WebSearchBatchTool builds the synchronous `web_search_batch` tool: run many
// independent queries concurrently and return all results at once.
func WebSearchBatchTool(svc *search.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "web_search_batch",
		Description: "Run multiple independent web searches concurrently and return all results " +
			"at once. Use when you have several unrelated queries; failures are isolated per " +
			"query. For a single query use web_search. The batch is capped at 32 queries.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"queries": {
					Type:        "array",
					Description: "List of independent natural-language search queries (max 32).",
				},
				"platform":      {Type: "string", Description: "Optional shared platform focus applied to every query."},
				"model":         {Type: "string", Description: "Optional shared per-call model override."},
				"extra_sources": {Type: "integer", Description: "Extra reference sources per query from Tavily/Firecrawl (0 disables)."},
			},
			Required:             []string{"queries"},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		queries := asStringSlice(args["queries"])
		if len(queries) == 0 {
			return nil, fmt.Errorf("web_search_batch: 'queries' must be a non-empty array of strings")
		}
		items := svc.BatchSearch(ctx, queries, search.BatchOptions{
			Platform:     stringArg(args, "platform"),
			Model:        stringArg(args, "model"),
			ExtraSources: parseIntArg(args, "extra_sources", 0),
		})
		out, _ := json.Marshal(batchItemsResult(items))
		return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: string(out)}}}, nil
	}
	return def, handler
}

// SubmitSearchTaskTool builds `submit_search_task`: enqueue a web_search or
// web_search_batch to run in the background, returning a task id immediately.
func SubmitSearchTaskTool(store *tasks.Store, svc *search.Service) (Tool, ToolHandler) {
	def := Tool{
		Name: "submit_search_task",
		Description: "Submit a web_search or web_search_batch to run in the background, returning a " +
			"task_id immediately. Poll get_search_task_result for the outcome. Use for long " +
			"searches you do not want to block on.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"kind":          {Type: "string", Description: "Task kind: 'web_search' or 'web_search_batch'."},
				"query":         {Type: "string", Description: "Query for kind=web_search."},
				"queries":       {Type: "array", Description: "Queries for kind=web_search_batch (max 32)."},
				"platform":      {Type: "string", Description: "Optional platform focus."},
				"model":         {Type: "string", Description: "Optional per-call model override."},
				"extra_sources": {Type: "integer", Description: "Extra reference sources from Tavily/Firecrawl (0 disables)."},
			},
			Required:             []string{"kind"},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		kind := stringArg(args, "kind")
		if _, ok := validTaskKinds[kind]; !ok {
			return nil, fmt.Errorf("submit_search_task: unknown kind %q (want web_search or web_search_batch)", kind)
		}
		params := taskParams(args)
		runner, err := buildSearchRunner(svc, kind, params)
		if err != nil {
			return nil, err
		}
		// Detach from the request context so the task outlives this tools/call;
		// cancellation flows through the store's own cancel, not the RPC.
		snap := store.Submit(context.Background(), kind, params, runner)
		return jsonResult(snap)
	}
	return def, handler
}

// GetSearchTaskResultTool builds `get_search_task_result`: read a task's state,
// optionally long-polling until it reaches a terminal state.
func GetSearchTaskResultTool(store *tasks.Store) (Tool, ToolHandler) {
	def := Tool{
		Name: "get_search_task_result",
		Description: "Read a submitted task's state and result by task_id. Pass wait (a Go duration " +
			"like '30s', max 5m) to long-poll until the task finishes; wait='0s' (default) returns " +
			"the current snapshot immediately.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"task_id": {Type: "string", Description: "The task id returned by submit_search_task."},
				"wait":    {Type: "string", Description: "Optional Go-style duration to long-poll (e.g. '30s'); max 5m; default '0s'."},
			},
			Required:             []string{"task_id"},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		id := stringArg(args, "task_id")
		if id == "" {
			return nil, fmt.Errorf("get_search_task_result: 'task_id' is required")
		}
		wait, err := parseWait(stringArg(args, "wait"))
		if err != nil {
			return nil, err
		}
		snap, ok := store.Get(ctx, id, wait)
		if !ok {
			return nil, fmt.Errorf("get_search_task_result: no task with id %q", id)
		}
		return jsonResult(snap)
	}
	return def, handler
}

// CancelSearchTaskTool builds `cancel_search_task`.
func CancelSearchTaskTool(store *tasks.Store) (Tool, ToolHandler) {
	def := Tool{
		Name:        "cancel_search_task",
		Description: "Cancel a queued or running task by task_id. Terminal tasks are returned unchanged.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"task_id": {Type: "string", Description: "The task id to cancel."},
				"hint":    {Type: "string", Description: "Optional cancellation reason, stored verbatim (default 'client')."},
			},
			Required:             []string{"task_id"},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		id := stringArg(args, "task_id")
		if id == "" {
			return nil, fmt.Errorf("cancel_search_task: 'task_id' is required")
		}
		snap, ok := store.Cancel(id, stringArg(args, "hint"))
		if !ok {
			return nil, fmt.Errorf("cancel_search_task: no task with id %q", id)
		}
		return jsonResult(snap)
	}
	return def, handler
}

// ListSearchTasksTool builds `list_search_tasks`.
func ListSearchTasksTool(store *tasks.Store) (Tool, ToolHandler) {
	def := Tool{
		Name: "list_search_tasks",
		Description: "List submitted tasks (oldest first), optionally filtered by state and kind. " +
			"States: queued/running/completed/failed/cancelled. Kinds: web_search/web_search_batch.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"states": {Type: "array", Description: "Optional state filter (e.g. ['running','completed'])."},
				"kinds":  {Type: "array", Description: "Optional kind filter (e.g. ['web_search'])."},
			},
			AdditionalProperties: false,
		},
	}
	handler := func(ctx context.Context, args map[string]any) (*ToolCallResult, error) {
		var states []tasks.State
		for _, s := range asStringSlice(args["states"]) {
			states = append(states, tasks.State(s))
		}
		kinds := asStringSlice(args["kinds"])
		list := store.List(states, kinds, time.Time{})
		return jsonResult(map[string]any{"count": len(list), "tasks": list})
	}
	return def, handler
}

// jsonResult marshals v into a text ToolCallResult.
func jsonResult(v any) (*ToolCallResult, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return &ToolCallResult{Content: []ContentItem{{Type: "text", Text: string(out)}}}, nil
}

// taskParams collects the search params from the tool args into the map stored
// on the task record and passed to the runner.
func taskParams(args map[string]any) map[string]any {
	p := map[string]any{}
	if q := stringArg(args, "query"); q != "" {
		p["query"] = q
	}
	if qs := asStringSlice(args["queries"]); len(qs) > 0 {
		p["queries"] = qs
	}
	if v := stringArg(args, "platform"); v != "" {
		p["platform"] = v
	}
	if v := stringArg(args, "model"); v != "" {
		p["model"] = v
	}
	if v := parseIntArg(args, "extra_sources", 0); v > 0 {
		p["extra_sources"] = v
	}
	return p
}

// parseWait parses an optional Go-style duration, clamped to [0, maxTaskWait].
func parseWait(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid wait %q (want a Go duration like 30s): %w", v, err)
	}
	if d < 0 {
		return 0, nil
	}
	if d > maxTaskWait {
		d = maxTaskWait
	}
	return d, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// asStringSlice coerces a JSON array argument into a []string.
// 只做类型强转，不做策略决策（不丢弃空白）——空白语义由下游 BatchSearch
// 统一管辖（标记 skipped），保持与 GrokSearch 基线契约一致。
func asStringSlice(v any) []string {
	var out []string
	switch arr := v.(type) {
	case []string:
		out = append(out, arr...)
	case []any:
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
