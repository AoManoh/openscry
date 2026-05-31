package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMapHTTPCrawlBasic(t *testing.T) {
	// A tiny 3-page site: root links to /a and /b.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/a">A</a><a href="/b">B</a></body></html>`)
	})
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/a/deep">deep</a></body></html>`)
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>leaf</body></html>`)
	})
	mux.HandleFunc("/a/deep", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>deep leaf</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 2, Limit: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http", res.Tier)
	}
	// root + /a + /b + /a/deep = 4
	if len(res.URLs) != 4 {
		t.Fatalf("got %d URLs, want 4: %v", len(res.URLs), res.URLs)
	}
}

func TestMapHTTPRespectsDepthLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/child">child</a></body></html>`)
	})
	mux.HandleFunc("/child", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/grandchild">gc</a></body></html>`)
	})
	mux.HandleFunc("/grandchild", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>gc</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 1, Limit: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// depth=1: root(0) + child(1); grandchild is depth 2, excluded
	if len(res.URLs) != 2 {
		t.Fatalf("got %d URLs (want 2, depth=1 should exclude grandchild): %v", len(res.URLs), res.URLs)
	}
}

func TestMapHTTPRespectsLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		var links strings.Builder
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&links, `<a href="/p%d">p%d</a>`, i, i)
		}
		fmt.Fprintf(w, `<html><body>%s</body></html>`, links.String())
	})
	for i := 0; i < 100; i++ {
		mux.HandleFunc(fmt.Sprintf("/p%d", i), func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `<html><body>leaf</body></html>`)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 2, Limit: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.URLs) > 5 {
		t.Fatalf("got %d URLs, limit was 5", len(res.URLs))
	}
}

func TestMapHTTPSameHostOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>
			<a href="/local">local</a>
			<a href="https://external.example.com/ext">external</a>
		</body></html>`)
	})
	mux.HandleFunc("/local", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>local page</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 2, Limit: 50})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, u := range res.URLs {
		if strings.Contains(u, "external.example.com") {
			t.Fatalf("external URL leaked through same-host filter: %s", u)
		}
	}
	// root + /local = 2
	if len(res.URLs) != 2 {
		t.Fatalf("got %d URLs, want 2: %v", len(res.URLs), res.URLs)
	}
}

func TestMapTavilyTierWins(t *testing.T) {
	// Mock Tavily /map endpoint.
	tavilySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/map" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"urls": []string{"https://example.com/", "https://example.com/page1", "https://example.com/page2"},
		})
	}))
	defer tavilySrv.Close()

	svc := New(Options{TavilyAPIKey: "test-key", TavilyAPIURL: tavilySrv.URL})
	res, err := svc.Map(context.Background(), Request{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tier != "tavily" {
		t.Fatalf("tier=%q want tavily", res.Tier)
	}
	if len(res.URLs) != 3 {
		t.Fatalf("got %d URLs, want 3", len(res.URLs))
	}
}

func TestMapTavilyFallsToHTTP(t *testing.T) {
	// Tavily returns empty → should fall to HTTP tier.
	tavilySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"urls": []string{}})
	}))
	defer tavilySrv.Close()

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/sub">sub</a></body></html>`)
	}))
	defer htmlSrv.Close()

	svc := New(Options{TavilyAPIKey: "test-key", TavilyAPIURL: tavilySrv.URL})
	res, err := svc.Map(context.Background(), Request{URL: htmlSrv.URL, MaxDepth: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http (tavily returned empty)", res.Tier)
	}
}

func TestMapEmptyURLRejected(t *testing.T) {
	svc := New(Options{})
	_, err := svc.Map(context.Background(), Request{URL: ""})
	if err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("expected empty-url error, got %v", err)
	}
}

func TestMapInvalidURLRejected(t *testing.T) {
	svc := New(Options{})
	_, err := svc.Map(context.Background(), Request{URL: "not-a-url"})
	if err == nil || !strings.Contains(err.Error(), "invalid url") {
		t.Fatalf("expected invalid-url error, got %v", err)
	}
}

func TestMapRespectsContextTimeout(t *testing.T) {
	// A server that hangs on every page except root (which links to /slow).
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/slow">slow</a></body></html>`)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := svc.Map(ctx, Request{URL: srv.URL, MaxDepth: 2, Limit: 50})
	if err != nil {
		t.Fatalf("expected partial results, got error: %v", err)
	}
	// Under timeout we get root (visited) + /slow (queued and visited but
	// its link extraction times out). The mapper is best-effort under budget.
	if len(res.URLs) > 2 {
		t.Fatalf("expected at most 2 URLs under timeout, got %d: %v", len(res.URLs), res.URLs)
	}
}
