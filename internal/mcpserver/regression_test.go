package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/mapper"
	"github.com/AoManoh/openscry/internal/planner"
	"github.com/AoManoh/openscry/internal/search"
)

// TestRegressionAllToolsEndToEnd is the S5 regression gate: an MCP session
// that initializes, lists all 4 tools, and calls each one with a mock
// backend, verifying the full wire path from JSON-RPC to tool handler to
// structured response.
func TestRegressionAllToolsEndToEnd(t *testing.T) {
	// Mock grok2api: returns different SSE content based on the system prompt.
	grokSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		body.ReadFrom(r.Body)
		raw := body.String()

		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(raw, "research planning strategist") {
			// Planner call: return a minimal valid plan JSON.
			plan := `{"intent":{"core_question":"test","query_type":"factual","time_sensitivity":"irrelevant"},"complexity":{"level":1,"estimated_queries":1,"estimated_calls":1,"justification":"simple"},"sub_queries":[{"id":"sq1","goal":"answer test","expected_output":"answer","boundary":"none"}],"search_terms":[{"term":"test query","purpose":"sq1","round":1}],"execution":{"parallel_groups":[["sq1"]],"sequential":[]},"strategies":{"fetch_before_claim":true,"gap_check":false}}`
			writeSSE(w, plan)
		} else {
			// Search/fetch call: return simple content.
			writeSSE(w, "mock result content")
		}
	}))
	defer grokSrv.Close()

	// Mock HTML site for web_map.
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/page1">Page 1</a><a href="/page2">Page 2</a></body></html>`)
	}))
	defer htmlSrv.Close()

	// Build all 4 services with mock backends.
	client := grok.NewClient(grokSrv.URL, "test-key", 10*time.Second)
	searchSvc := search.NewWithOptions(client, "test-model", search.Options{MaxAttempts: 1})
	fetchSvc := fetch.New(client, fetch.Options{Model: "test-model"})
	mapSvc := mapper.New(mapper.Options{})
	planSvc := planner.New(client, planner.Options{Model: "test-model"})

	// Build MCP input: initialize → initialized → tools/list → 4 x tools/call.
	messages := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"regression","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		// web_search
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"test search"}}}`,
		// web_fetch (fetch the HTML site to avoid model-refusal on reserved domains)
		fmt.Sprintf(`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"web_fetch","arguments":{"url":"%s"}}}`, htmlSrv.URL),
		// web_map
		fmt.Sprintf(`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"web_map","arguments":{"url":"%s","max_depth":"1","limit":"5"}}}`, htmlSrv.URL),
		// research_plan
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"research_plan","arguments":{"question":"What is unit testing?"}}}`,
	}
	input := strings.Join(messages, "\n") + "\n"

	var out bytes.Buffer
	srv := New(strings.NewReader(input), &out, nil)
	srv.Register(WebSearchTool(searchSvc))
	srv.Register(WebFetchTool(fetchSvc))
	srv.Register(WebMapTool(mapSvc))
	srv.Register(ResearchPlanTool(planSvc))

	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	lines := splitNonEmpty(out.String())
	// initialize(1) + tools/list(2) + 4 tools/call(10,11,12,13) = 6 responses
	// (notifications/initialized produces no reply)
	if len(lines) != 6 {
		t.Fatalf("expected 6 responses, got %d:\n%s", len(lines), out.String())
	}

	// The concurrent engine may return tools/call responses out of order.
	// Build a map by response ID for order-independent assertions.
	byID := make(map[int]string)
	for _, line := range lines {
		var peek struct {
			ID any `json:"id"`
		}
		mustJSON(t, line, &peek)
		if f, ok := peek.ID.(float64); ok {
			byID[int(f)] = line
		}
	}

	// 1. Verify initialize response (id=1).
	var initResp JSONRPCResponse
	mustJSON(t, byID[1], &initResp)
	initResult, _ := initResp.Result.(map[string]any)
	if initResult["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("init: bad protocol version: %v", initResult["protocolVersion"])
	}
	si, _ := initResult["serverInfo"].(map[string]any)
	if si["version"] != ServerVersion {
		t.Fatalf("init: version=%v want %s", si["version"], ServerVersion)
	}

	// 2. Verify tools/list (id=2) contains all 4 tools.
	for _, name := range []string{"web_search", "web_fetch", "web_map", "research_plan"} {
		if !strings.Contains(byID[2], `"`+name+`"`) {
			t.Fatalf("tools/list missing %q: %s", name, byID[2])
		}
	}

	// 3. Verify web_search (id=10).
	assertToolCallOK(t, byID[10], 10, "mock result content")

	// 4. Verify web_fetch (id=11) — no error (tier may vary).
	assertToolCallNoError(t, byID[11], 11)

	// 5. Verify web_map (id=12) — should contain root URL.
	assertToolCallNoError(t, byID[12], 12)
	if !strings.Contains(byID[12], htmlSrv.URL) {
		t.Fatalf("web_map response should contain root URL: %s", byID[12])
	}

	// 6. Verify research_plan (id=13) — should contain plan JSON.
	assertToolCallNoError(t, byID[13], 13)
	if !strings.Contains(byID[13], "core_question") {
		t.Fatalf("research_plan response should contain plan JSON: %s", byID[13])
	}
}

// TestRegressionUnknownToolFailsLoud verifies that calling a non-existent
// tool returns a JSON-RPC error (not a silent empty response).
func TestRegressionUnknownToolFailsLoud(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nonexistent","arguments":{}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	srv := New(strings.NewReader(input), &out, nil)
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	lines := splitNonEmpty(out.String())
	if len(lines) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(lines))
	}
	if !strings.Contains(lines[1], "unknown tool") {
		t.Fatalf("expected unknown-tool error, got: %s", lines[1])
	}
}

func writeSSE(w http.ResponseWriter, content string) {
	escaped := strings.ReplaceAll(content, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n", escaped)
	fmt.Fprint(w, "data: [DONE]\n")
}

func assertToolCallOK(t *testing.T, line string, expectedID int, mustContain string) {
	t.Helper()
	var resp JSONRPCResponse
	mustJSON(t, line, &resp)
	id, _ := resp.ID.(float64)
	if int(id) != expectedID {
		t.Fatalf("response id=%v want %d", resp.ID, expectedID)
	}
	result, _ := resp.Result.(map[string]any)
	if result == nil {
		t.Fatalf("id=%d: result is nil (error: %+v)", expectedID, resp.Error)
	}
	isErr, _ := result["isError"].(bool)
	if isErr {
		t.Fatalf("id=%d: isError=true, content=%v", expectedID, result["content"])
	}
	if mustContain != "" && !strings.Contains(line, mustContain) {
		t.Fatalf("id=%d: response should contain %q: %s", expectedID, mustContain, line)
	}
}

func assertToolCallNoError(t *testing.T, line string, expectedID int) {
	t.Helper()
	var resp JSONRPCResponse
	mustJSON(t, line, &resp)
	id, _ := resp.ID.(float64)
	if int(id) != expectedID {
		t.Fatalf("response id=%v want %d", resp.ID, expectedID)
	}
	if resp.Error != nil {
		t.Fatalf("id=%d: unexpected JSON-RPC error: %+v", expectedID, resp.Error)
	}
	result, _ := resp.Result.(map[string]any)
	if result == nil {
		t.Fatalf("id=%d: result is nil", expectedID)
	}
	isErr, _ := result["isError"].(bool)
	if isErr {
		content, _ := result["content"].([]any)
		t.Fatalf("id=%d: tool returned isError=true: %v", expectedID, content)
	}
}
