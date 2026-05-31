// Package planner implements offline research plan generation: given a
// question, it asks the configured grok model to produce a structured
// research plan (JSON) that downstream tools can execute. The plan structure
// embeds fetch_before_claim and gap_check strategies per PRD F7.
package planner

// Plan is the structured output of a research plan generation.
type Plan struct {
	Intent    Intent     `json:"intent"`
	Complexity Complexity `json:"complexity"`
	SubQueries []SubQuery `json:"sub_queries"`
	SearchTerms []SearchTerm `json:"search_terms"`
	Execution  Execution  `json:"execution"`
	Strategies Strategies `json:"strategies"`
}

// Intent captures the core question analysis.
type Intent struct {
	CoreQuestion    string `json:"core_question"`
	QueryType       string `json:"query_type"`       // factual|comparative|exploratory|analytical
	TimeSensitivity string `json:"time_sensitivity"` // realtime|recent|historical|irrelevant
	Domain          string `json:"domain,omitempty"`
}

// Complexity assesses the research scope.
type Complexity struct {
	Level             int    `json:"level"` // 1-3
	EstimatedQueries  int    `json:"estimated_queries"`
	EstimatedCalls    int    `json:"estimated_calls"`
	Justification     string `json:"justification"`
}

// SubQuery is one decomposed aspect of the research question.
type SubQuery struct {
	ID             string `json:"id"`              // e.g. "sq1"
	Goal           string `json:"goal"`
	ExpectedOutput string `json:"expected_output"`
	Boundary       string `json:"boundary"`        // what this excludes
	DependsOn      string `json:"depends_on,omitempty"`
	ToolHint       string `json:"tool_hint,omitempty"` // web_search|web_fetch|web_map
}

// SearchTerm is one concrete search query to execute.
type SearchTerm struct {
	Term    string `json:"term"`
	Purpose string `json:"purpose"` // which sub-query this serves (e.g. "sq1")
	Round   int    `json:"round"`   // 1=broad, 2+=targeted follow-up
}

// Execution defines the order of operations.
type Execution struct {
	ParallelGroups [][]string `json:"parallel_groups"` // groups of sub-query IDs to run in parallel
	Sequential     []string   `json:"sequential"`      // IDs that must run in order
}

// Strategies embeds research quality control patterns.
type Strategies struct {
	FetchBeforeClaim bool   `json:"fetch_before_claim"` // verify sources before citing
	GapCheck         bool   `json:"gap_check"`          // identify evidence gaps after collection
	FallbackPlan     string `json:"fallback_plan,omitempty"`
}
