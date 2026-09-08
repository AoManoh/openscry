package refsource

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tavilyStub(results []map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
}

func firecrawlStub(web []map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"web": web}})
	}))
}

func TestAvailableFalseWithoutKeys(t *testing.T) {
	p := New(Options{})
	if p.Available() {
		t.Fatal("expected Available()=false with no keys")
	}
	src, fail := p.Search(context.Background(), "q", 5)
	if src != nil || fail != nil {
		t.Fatalf("expected no-op, got src=%v fail=%v", src, fail)
	}
}

func TestSearchBothTiersMergeAndDedup(t *testing.T) {
	tav := tavilyStub([]map[string]any{
		{"title": "T1", "url": "https://a.com", "content": "ca"},
		{"title": "Dup", "url": "https://dup.com"},
	})
	defer tav.Close()
	fire := firecrawlStub([]map[string]any{
		{"title": "F1", "url": "https://b.com", "description": "db"},
		{"title": "Dup2", "url": "https://dup.com"}, // same URL as a tavily result
	})
	defer fire.Close()

	p := New(Options{
		TavilyAPIKey: "k", TavilyAPIURL: tav.URL,
		FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL,
	})
	src, fail := p.Search(context.Background(), "query", 4)
	if len(fail) != 0 {
		t.Fatalf("unexpected failures: %v", fail)
	}
	// 3 unique URLs (a.com, b.com, dup.com once). Firecrawl is merged first.
	if len(src) != 3 {
		t.Fatalf("got %d sources, want 3: %+v", len(src), src)
	}
	seen := map[string]bool{}
	for _, s := range src {
		if seen[s.URL] {
			t.Fatalf("duplicate URL %s", s.URL)
		}
		seen[s.URL] = true
	}
}

func TestSearchIsolatesTierFailure(t *testing.T) {
	// Tavily fails (500), Firecrawl succeeds. The failure is recorded and the
	// Firecrawl results still come back.
	tav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tav.Close()
	fire := firecrawlStub([]map[string]any{{"title": "F1", "url": "https://b.com"}})
	defer fire.Close()

	p := New(Options{
		TavilyAPIKey: "k", TavilyAPIURL: tav.URL,
		FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL,
	})
	src, fail := p.Search(context.Background(), "query", 4)
	if len(src) != 1 || src[0].URL != "https://b.com" {
		t.Fatalf("expected firecrawl result, got %+v", src)
	}
	if len(fail) != 1 || fail[0].Provider != "tavily" {
		t.Fatalf("expected one tavily failure, got %v", fail)
	}
}

func TestAllocateSplit(t *testing.T) {
	both := New(Options{TavilyAPIKey: "k", FirecrawlAPIKey: "k"})
	tav, fire := both.allocate(5)
	if fire != 3 || tav != 2 {
		t.Fatalf("split(5)=fire %d tav %d, want fire 3 tav 2", fire, tav)
	}
	onlyTav := New(Options{TavilyAPIKey: "k"})
	tav, fire = onlyTav.allocate(5)
	if tav != 5 || fire != 0 {
		t.Fatalf("only-tavily(5)=tav %d fire %d, want tav 5 fire 0", tav, fire)
	}
	onlyFire := New(Options{FirecrawlAPIKey: "k"})
	tav, fire = onlyFire.allocate(5)
	if fire != 5 || tav != 0 {
		t.Fatalf("only-firecrawl(5)=fire %d tav %d, want fire 5 tav 0", fire, tav)
	}
}

// Firecrawl /v1/search 返回 data 为数组（现网默认端点），必须与 v2 的 data.web 形态同样被解析。
func TestFirecrawlSearchAcceptsV1ArrayPayload(t *testing.T) {
	fire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": []map[string]any{
			{"url": "https://blog.rust-lang.org/releases/latest/", "title": "Announcing Rust 1.98.0", "description": "..."},
		}})
	}))
	defer fire.Close()
	p := New(Options{FirecrawlAPIKey: "k", FirecrawlAPIURL: fire.URL})
	src, fail := p.Search(context.Background(), "query", 2)
	if len(fail) != 0 {
		t.Fatalf("v1 array payload must parse, got failures %v", fail)
	}
	if len(src) != 1 || src[0].URL != "https://blog.rust-lang.org/releases/latest/" || src[0].Origin != "firecrawl" {
		t.Fatalf("sources = %+v", src)
	}
}
