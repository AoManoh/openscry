package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/prompt"
)

// sseContent renders a minimal OpenAI-style SSE body carrying one content
// delta followed by [DONE], matching what the grok client parses.
func sseContent(text string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n" +
		"data: [DONE]\n"
}

func newSSEServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseContent(text)))
	}))
}

func newStatusServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestFetchGrokTierWins(t *testing.T) {
	grokSrv := newSSEServer(t, "fetched-by-grok")
	defer grokSrv.Close()

	svc := New(grok.NewClient(grokSrv.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "grok" {
		t.Fatalf("tier=%q want grok", res.Tier)
	}
	if !strings.Contains(res.Content, "fetched-by-grok") {
		t.Fatalf("content=%q", res.Content)
	}
	if res.Model != "m" {
		t.Fatalf("model=%q want m", res.Model)
	}
}

func TestFetchFallsThroughToBasicHTTP(t *testing.T) {
	// Grok tier fails (500); no Tavily/Firecrawl keys -> those tiers skipped;
	// the basic-HTTP tier must win against the served HTML page.
	grokFail := newStatusServer(http.StatusInternalServerError, "boom")
	defer grokFail.Close()
	html := "<html><head><title>Hello &amp; World</title></head><body>" +
		"<script>var x=1;</script><h1>Heading</h1><p>Para one.</p><p>Para two.</p></body></html>"
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), htmlSrv.URL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http", res.Tier)
	}
	if !strings.Contains(res.Content, "Hello & World") {
		t.Fatalf("expected unescaped title in content, got %q", res.Content)
	}
	for _, want := range []string{"Heading", "Para one.", "Para two."} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("content missing %q: %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "var x=1") {
		t.Fatalf("script content should be stripped: %q", res.Content)
	}
}

func TestFetchTavilyTierWins(t *testing.T) {
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/extract" {
			t.Errorf("unexpected tavily path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"results":[{"raw_content":"# Tavily markdown"}]}`))
	}))
	defer tavily.Close()

	svc := New(nil, Options{TavilyAPIKey: "tk", TavilyAPIURL: tavily.URL})
	res, err := svc.Fetch(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "tavily" || !strings.Contains(res.Content, "Tavily markdown") {
		t.Fatalf("tier=%q content=%q", res.Tier, res.Content)
	}
}

func TestFetchFirecrawlAfterTavilyFails(t *testing.T) {
	tavilyFail := newStatusServer(http.StatusInternalServerError, "down")
	defer tavilyFail.Close()
	firecrawl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scrape" {
			t.Errorf("unexpected firecrawl path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"markdown":"# FC markdown"}}`))
	}))
	defer firecrawl.Close()

	svc := New(nil, Options{
		TavilyAPIKey: "tk", TavilyAPIURL: tavilyFail.URL,
		FirecrawlAPIKey: "fk", FirecrawlAPIURL: firecrawl.URL,
	})
	res, err := svc.Fetch(context.Background(), "https://example.com/x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "firecrawl" || !strings.Contains(res.Content, "FC markdown") {
		t.Fatalf("tier=%q content=%q", res.Tier, res.Content)
	}
}

func TestFetchAllTiersFailVisibleError(t *testing.T) {
	grokFail := newStatusServer(http.StatusInternalServerError, "boom")
	defer grokFail.Close()
	httpFail := newStatusServer(http.StatusInternalServerError, "nope")
	defer httpFail.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{Model: "m"})
	_, err := svc.Fetch(context.Background(), httpFail.URL)
	if err == nil {
		t.Fatal("expected failure when every tier fails")
	}
	msg := err.Error()
	if !strings.Contains(msg, "all extractors failed") || !strings.Contains(msg, "grok:") || !strings.Contains(msg, "http:") {
		t.Fatalf("error should list tiers tried, got: %v", err)
	}
}

func TestFetchRejectsInvalidURL(t *testing.T) {
	svc := New(nil, Options{})
	for _, bad := range []string{"", "   ", "not-a-url", "ftp://example.com/x", "/relative/path"} {
		if _, err := svc.Fetch(context.Background(), bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}

func TestStripHTMLToText(t *testing.T) {
	in := "<html><head><title>T</title><style>.x{color:red}</style></head>" +
		"<body><script>bad()</script><h1>H</h1><p>one</p><p>two</p></body></html>"
	out := stripHTMLToText(in)
	if strings.Contains(out, "bad()") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style not stripped: %q", out)
	}
	for _, want := range []string{"H", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestExtractTitle(t *testing.T) {
	if got := extractTitle("<TITLE> Hi &amp; Bye </TITLE>"); got != "Hi & Bye" {
		t.Fatalf("title=%q want \"Hi & Bye\"", got)
	}
	if got := extractTitle("<body>no title</body>"); got != "" {
		t.Fatalf("expected empty title, got %q", got)
	}
}

func TestFetchReservesBudgetForBasicHTTP(t *testing.T) {
	// The Grok tier hangs far longer than the extractor budget; the reserved
	// slice must let the basic-HTTP last resort still succeed rather than the
	// slow tier consuming the whole budget.
	grokSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep past the extractor budget (~600ms) so the Grok tier is
		// time-boxed by the reserve; bounded so test teardown stays quick.
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer grokSlow.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>OK</title></head><body><p>real content</p></body></html>"))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokSlow.URL, "k", 5*time.Second), Options{Model: "m"})
	// 900ms total -> ~300ms reserved for basic HTTP, ~600ms for extractors.
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	res, err := svc.Fetch(ctx, htmlSrv.URL)
	if err != nil {
		t.Fatalf("expected basic-HTTP fallback to succeed, got %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http (grok tier should be time-boxed by the reserve)", res.Tier)
	}
	if !strings.Contains(res.Content, "real content") {
		t.Fatalf("content=%q", res.Content)
	}
}

func TestFetchStrictDisablesBasicHTTP(t *testing.T) {
	// In strict mode (GROK_FETCH_FALLBACK=strict) the basic-HTTP last resort
	// is disabled. When the grok tier fails and no Tavily/Firecrawl is
	// configured, the fetch must fail loud instead of degrading.
	grokFail := newStatusServer(404, `{"error":{"message":"model not found"}}`)
	defer grokFail.Close()

	svc := New(grok.NewClient(grokFail.URL, "k", 5*time.Second), Options{
		Model:  "m",
		Strict: true,
	})
	_, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err == nil {
		t.Fatal("expected fail-loud in strict mode, got nil")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("error should mention strict mode, got: %v", err)
	}
	if strings.Contains(err.Error(), "http:") {
		t.Fatalf("strict mode should not have tried the http tier, got: %v", err)
	}
}

func TestFetchGrokFailureSentinelFallsThrough(t *testing.T) {
	// Regression: when the Grok tier returns the failure sentinel (the model
	// could not retrieve the page), full mode must NOT accept it as content;
	// the chain falls through to the basic-HTTP last resort.
	grokSentinel := newSSEServer(t, prompt.FetchFailureSentinel)
	defer grokSentinel.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Real</title></head><body><p>real page content</p></body></html>"))
	}))
	defer htmlSrv.Close()

	svc := New(grok.NewClient(grokSentinel.URL, "k", 5*time.Second), Options{Model: "m"})
	res, err := svc.Fetch(context.Background(), htmlSrv.URL)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http (grok sentinel must not win)", res.Tier)
	}
	if !strings.Contains(res.Content, "real page content") {
		t.Fatalf("content=%q", res.Content)
	}
}

func TestFetchGrokFailureSentinelStrictFailsLoud(t *testing.T) {
	// Regression: in strict mode the basic-HTTP fallback is disabled, so a
	// Grok sentinel (page unavailable) must fail loud rather than be accepted
	// as content.
	grokSentinel := newSSEServer(t, prompt.FetchFailureSentinel)
	defer grokSentinel.Close()

	svc := New(grok.NewClient(grokSentinel.URL, "k", 5*time.Second), Options{Model: "m", Strict: true})
	_, err := svc.Fetch(context.Background(), "https://example.com/page")
	if err == nil {
		t.Fatal("expected fail-loud when grok signals unavailable in strict mode")
	}
	if !strings.Contains(err.Error(), "strict") {
		t.Fatalf("error should mention strict mode, got: %v", err)
	}
}
