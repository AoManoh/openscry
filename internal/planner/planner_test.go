package planner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
)

// validPlanJSON is a well-formed plan response used by most tests.
const validPlanJSON = `{
  "intent": {
    "core_question": "How does Go's garbage collector work?",
    "query_type": "exploratory",
    "time_sensitivity": "historical",
    "domain": "programming languages"
  },
  "complexity": {
    "level": 2,
    "estimated_queries": 3,
    "estimated_calls": 5,
    "justification": "Multiple aspects (tricolor, pacer, STW) need separate investigation"
  },
  "sub_queries": [
    {"id": "sq1", "goal": "Understand tricolor mark-sweep algorithm", "expected_output": "Algorithm description with phases", "boundary": "Excludes pacer/scheduling", "depends_on": "", "tool_hint": "web_search"},
    {"id": "sq2", "goal": "Understand GC pacer and scheduling", "expected_output": "How Go decides when to GC", "boundary": "Excludes mark algorithm details", "depends_on": "", "tool_hint": "web_search"},
    {"id": "sq3", "goal": "Recent GC improvements in Go 1.19+", "expected_output": "Changelog items and benchmark data", "boundary": "Only post-1.19 changes", "depends_on": "sq1,sq2", "tool_hint": "web_search"}
  ],
  "search_terms": [
    {"term": "Go garbage collector tricolor algorithm", "purpose": "sq1", "round": 1},
    {"term": "Go GC pacer scheduling heuristics", "purpose": "sq2", "round": 1},
    {"term": "Go 1.22 GC improvements benchmark", "purpose": "sq3", "round": 2}
  ],
  "execution": {
    "parallel_groups": [["sq1", "sq2"]],
    "sequential": ["sq3"]
  },
  "strategies": {
    "fetch_before_claim": true,
    "gap_check": true,
    "fallback_plan": "If academic sources unavailable, use official Go blog posts"
  }
}`

func sseContent(text string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\ndata: [DONE]\n"
}

func newPlanServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// SSE doesn't handle JSON well in a single delta (quotes need escaping).
		// Send the entire response as one chunk for simplicity in tests.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + jsonEscape(response) + "}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
}

// jsonEscape wraps a string as a JSON string value (with quotes).
func jsonEscape(s string) string {
	// Use a raw approach: escape special chars for embedding in JSON.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return `"` + s + `"`
}

func TestPlanSuccess(t *testing.T) {
	srv := newPlanServer(t, validPlanJSON)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	plan, err := svc.Plan(context.Background(), "How does Go's garbage collector work?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plan.Intent.CoreQuestion != "How does Go's garbage collector work?" {
		t.Fatalf("core_question=%q", plan.Intent.CoreQuestion)
	}
	if plan.Complexity.Level != 2 {
		t.Fatalf("level=%d want 2", plan.Complexity.Level)
	}
	if len(plan.SubQueries) != 3 {
		t.Fatalf("sub_queries=%d want 3", len(plan.SubQueries))
	}
	if len(plan.SearchTerms) != 3 {
		t.Fatalf("search_terms=%d want 3", len(plan.SearchTerms))
	}
	if !plan.Strategies.FetchBeforeClaim {
		t.Fatal("fetch_before_claim should be true")
	}
	if !plan.Strategies.GapCheck {
		t.Fatal("gap_check should be true")
	}
	if len(plan.Execution.ParallelGroups) != 1 || len(plan.Execution.ParallelGroups[0]) != 2 {
		t.Fatalf("parallel_groups=%v", plan.Execution.ParallelGroups)
	}
}

func TestPlanStripsMarkdownFences(t *testing.T) {
	// Some models wrap JSON in ```json ... ```
	fenced := "```json\n" + validPlanJSON + "\n```"
	srv := newPlanServer(t, fenced)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	plan, err := svc.Plan(context.Background(), "test question")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Intent.CoreQuestion == "" {
		t.Fatal("plan parsed but core_question empty")
	}
}

func TestPlanEmptyQuestionRejected(t *testing.T) {
	svc := New(grok.NewClient("http://unused", "k", time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "   ")
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-question error, got %v", err)
	}
}

func TestPlanModelFailureIsExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "bad-model"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil {
		t.Fatal("expected explicit error for model failure, got nil")
	}
	if !strings.Contains(err.Error(), "model call failed") {
		t.Fatalf("error should mention model call failed: %v", err)
	}
}

func TestPlanInvalidJSONFromModel(t *testing.T) {
	srv := newPlanServer(t, "this is not json at all")
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error should mention parse: %v", err)
	}
}

func TestPlanValidationFailsOnEmptySubQueries(t *testing.T) {
	incomplete := `{
		"intent":{"core_question":"q","query_type":"factual","time_sensitivity":"irrelevant"},
		"complexity":{"level":1,"estimated_queries":1,"estimated_calls":1,"justification":"simple"},
		"sub_queries":[],
		"search_terms":[{"term":"t","purpose":"sq1","round":1}],
		"execution":{"parallel_groups":[],"sequential":[]},
		"strategies":{"fetch_before_claim":false,"gap_check":false}
	}`
	srv := newPlanServer(t, incomplete)
	defer srv.Close()

	svc := New(grok.NewClient(srv.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Plan(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "sub_queries must not be empty") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestParsePlanDirect(t *testing.T) {
	plan, err := parsePlan(validPlanJSON)
	if err != nil {
		t.Fatalf("parsePlan failed: %v", err)
	}
	if plan.Intent.QueryType != "exploratory" {
		t.Fatalf("query_type=%q", plan.Intent.QueryType)
	}
	if plan.SubQueries[2].DependsOn != "sq1,sq2" {
		t.Fatalf("depends_on=%q", plan.SubQueries[2].DependsOn)
	}
}
