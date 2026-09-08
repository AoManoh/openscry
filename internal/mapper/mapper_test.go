package mapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AoManoh/openscry/internal/resilience"
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
			"results": []string{"https://example.com/", "https://example.com/page1", "https://example.com/page2"},
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
		json.NewEncoder(w).Encode(map[string]any{"results": []string{}})
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

// slowRootServer 的根页面在 delay 之后才返回一条链接；客户端提前放弃时随请求上下文结束
// 立刻退出（GET 无请求体，net/http 会立即开始后台读连接，断开即取消 r.Context()）。
// 根页面超时会让 httpCrawl 以 "root page unreachable" 失败，是可断言的确定结果。
func slowRootServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, `<html><body><a href="/child">child</a></body></html>`)
	})
	mux.HandleFunc("/child", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body>leaf</body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// requireDeadlineWithin 断言 map 因截止时间失败，且耗时不超过 limit。
func requireDeadlineWithin(t *testing.T, err error, elapsed, limit time.Duration) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline-exceeded failure", err)
	}
	if elapsed > limit {
		t.Fatalf("elapsed = %v, want at most %v (the budget did not bound the crawl)", elapsed, limit)
	}
}

func TestMapProfileAppliesUnderLateParentDeadline(t *testing.T) {
	// 父上下文带 10 分钟截止时间（模拟 MCP HTTP 传输的请求上限），根页面 4s 才响应，
	// OpMap 档位 1s（resilience 的下限）：必须约 1s 内以截止时间失败，而不是等到 4s 后成功。
	srv := slowRootServer(t, 4*time.Second)
	svc := New(Options{Profiles: resilience.Profiles{Map: time.Second}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	_, err := svc.Map(ctx, Request{URL: srv.URL, MaxDepth: 1})
	requireDeadlineWithin(t, err, time.Since(started), 3*time.Second)
}

func TestMapExplicitTimeoutOverridesProfile(t *testing.T) {
	t.Run("shorter than profile", func(t *testing.T) {
		// 默认档位 90s、显式 200ms：显式预算不经档位钳制，必须在 200ms 附近失败。
		srv := slowRootServer(t, 4*time.Second)
		svc := New(Options{})

		started := time.Now()
		_, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 1, Timeout: 200 * time.Millisecond})
		requireDeadlineWithin(t, err, time.Since(started), 1500*time.Millisecond)
	})
	t.Run("longer than profile", func(t *testing.T) {
		// 档位 1s（下限）、根页面 1.3s 后响应、显式 5s：map 必须成功，证明显式预算也能放宽档位。
		srv := slowRootServer(t, 1300*time.Millisecond)
		svc := New(Options{Profiles: resilience.Profiles{Map: time.Second}})

		res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 1, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("an explicit budget longer than the profile must let the crawl finish, got %v", err)
		}
		// 根 + /child = 2
		if res.Tier != "http" || len(res.URLs) != 2 {
			t.Fatalf("res = %+v, want the http tier with root and child", res)
		}
	})
}

func TestMapRootUnreachableFails(t *testing.T) {
	// Regression: when the root page itself is unreachable (here: HTTP 500 on
	// every path, so root link extraction fails), the crawl must fail loud
	// instead of returning the seed URL alone as a successful result.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: srv.URL, MaxDepth: 1})
	if err == nil {
		t.Fatalf("expected fail-loud when the root page is unreachable, got URLs=%v", res.URLs)
	}
}

func TestMapInstructionsWarningWhenTavilyEmpty(t *testing.T) {
	// Tavily returns empty results (instructions filter yields nothing) →
	// HTTP BFS wins but Warning must be set (degradation visible).
	tavilySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []string{}})
	}))
	defer tavilySrv.Close()
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/page">page</a></body></html>`)
	}))
	defer htmlSrv.Close()

	svc := New(Options{TavilyAPIKey: "test-key", TavilyAPIURL: tavilySrv.URL})
	res, err := svc.Map(context.Background(), Request{
		URL:          htmlSrv.URL,
		MaxDepth:     1,
		Instructions: "only API docs",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tier != "http" {
		t.Fatalf("tier=%q want http", res.Tier)
	}
	if res.Warning == "" {
		t.Fatal("expected Warning to be set when instructions are not honored by HTTP tier")
	}
	if !strings.Contains(res.Warning, "unfiltered") {
		t.Fatalf("Warning should mention 'unfiltered', got: %q", res.Warning)
	}
}

func TestMapNoWarningWithoutInstructions(t *testing.T) {
	// Without instructions, HTTP tier result has no warning.
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/a">a</a></body></html>`)
	}))
	defer htmlSrv.Close()

	svc := New(Options{})
	res, err := svc.Map(context.Background(), Request{URL: htmlSrv.URL, MaxDepth: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warning != "" {
		t.Fatalf("expected no Warning without instructions, got: %q", res.Warning)
	}
}

func TestMapNoWarningWhenTavilyHonorsInstructions(t *testing.T) {
	// When Tavily returns non-empty results with instructions → no warning.
	tavilySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []string{"https://example.com/docs/api"},
		})
	}))
	defer tavilySrv.Close()

	svc := New(Options{TavilyAPIKey: "test-key", TavilyAPIURL: tavilySrv.URL})
	res, err := svc.Map(context.Background(), Request{
		URL:          "https://example.com",
		MaxDepth:     1,
		Instructions: "only API docs",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Tier != "tavily" {
		t.Fatalf("tier=%q want tavily", res.Tier)
	}
	if res.Warning != "" {
		t.Fatalf("expected no Warning when Tavily honors instructions, got: %q", res.Warning)
	}
}
