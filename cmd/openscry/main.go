// Command openscry is the single-binary entry point for the openscry search
// tool. It dispatches to subcommands:
//
//	openscry search [--model M] [--platform P] [--timeout D] "query"
//	openscry mcp     # run the thin MCP adapter over stdio
//	openscry version
//
// The CLI core and the MCP adapter share the same search code path
// (internal/search), per the CLI-core + thin-MCP architecture decision.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/AoManoh/openscry/internal/config"
	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/mapper"
	"github.com/AoManoh/openscry/internal/mcpserver"
	"github.com/AoManoh/openscry/internal/planner"
	"github.com/AoManoh/openscry/internal/refsource"
	"github.com/AoManoh/openscry/internal/resilience"
	"github.com/AoManoh/openscry/internal/search"
	"github.com/AoManoh/openscry/internal/tasks"
	versioninfo "github.com/AoManoh/openscry/internal/version"
)

// version 来自 Go build 信息（发布 tag / 伪版本 / devel），见 internal/version。
var version = versioninfo.Value()

// searchProfiles 把 GROK_REQUEST_TIMEOUT 注入搜索类操作的档位。搜索服务与规划服务都
// 以此作为默认预算，CLI、MCP stdio、MCP HTTP、批量与异步搜索、research_plan 才会遵循同
// 一个配置值；fetch / map 档位不在这里设置，由 Normalize 补齐默认的 30s / 90s。
// 之前 runMCP 没有把配置值传给服务层，MCP 下的搜索实际用的是 120s 默认档位，与 CLI 和
// get_config_info 报告的 request_timeout 不一致。
func searchProfiles(cfg *config.Config) resilience.Profiles {
	return resilience.Profiles{Search: cfg.RequestTimeout}
}

// logLevel 把 GROK_DEBUG 映射为 slog 级别。之前 runMCP 把级别固定为 Info，config 虽然读取了
// GROK_DEBUG 却从未使用，引擎的 mcp.req_completed 等 Debug 级日志永远不会输出，.env.example
// 对该变量的承诺落空。级别只影响过滤阈值，不改变任何日志的字段内容，密钥不会因此进入日志。
func logLevel(debug bool) slog.Level {
	if debug {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// engineQueueWait 把配置层的 GROK_QUEUE_WAIT_TIMEOUT 换成引擎参数。配置里 0 表示“队列满时
// 立即拒绝”，而 EngineConfig 沿用“零值回落默认值”的约定，所以立即拒绝必须用引擎的显式哨兵
// 值表达；直接把 0 传过去会被引擎当成未设置而替换成 10s 默认等待，与用户配置相反。
func engineQueueWait(d time.Duration) time.Duration {
	if d == 0 {
		return mcpserver.QueueWaitNone
	}
	return d
}

// newRefProvider builds the extra_sources reference provider from config. It
// is always constructed; refsource.Provider.Available() is false (and the
// fan-out a no-op) when neither Tavily nor Firecrawl key is set.
func newRefProvider(cfg *config.Config) *refsource.Provider {
	return refsource.New(refsource.Options{
		TavilyAPIKey:    cfg.TavilyAPIKey,
		TavilyAPIURL:    cfg.TavilyAPIURL,
		FirecrawlAPIKey: cfg.FirecrawlAPIKey,
		FirecrawlAPIURL: cfg.FirecrawlAPIURL,
	})
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch cmd := os.Args[1]; cmd {
	case "search":
		os.Exit(runSearch(os.Args[2:]))
	case "fetch":
		os.Exit(runFetch(os.Args[2:]))
	case "map":
		os.Exit(runMap(os.Args[2:]))
	case "plan":
		os.Exit(runPlan(os.Args[2:]))
	case "mcp":
		os.Exit(runMCP(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("openscry " + version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `openscry `+version+` - Grok-backed web search (CLI core + thin MCP)

usage:
  openscry search [--model M] [--platform P] [--timeout D] "your query"
  openscry fetch  [--timeout D] <url>
  openscry map    [--depth N] [--breadth N] [--limit N] [--instructions S] [--timeout D] <url>
  openscry plan   [--timeout D] "your research question"
  openscry mcp [--http :8080] [--tools core|all]  run the MCP adapter over stdio (default) or HTTP
                               core (default): web_search, web_fetch, web_map, research_plan,
                                               web_search_batch, get_config_info
                               all: core + async submit/get/cancel/list_search_tasks
  openscry version    print version

environment:
  GROK_API_URL          required, OpenAI-compatible base URL of grok2api (e.g. https://host/v1)
  GROK_API_KEY          required
  GROK_MODEL            required, the model to use (user-supplied; never defaulted)
  GROK_REQUEST_TIMEOUT  optional, default 120s (accepts "120" seconds or "2m")
  GROK_SEARCH_PROVIDER  optional, auto|chat|responses (default auto; resolves to chat for grok2api)
  GROK_FETCH_FALLBACK   optional, full|strict (default full; strict disables web_fetch's basic-HTTP fallback)
  GROK_CONCURRENCY      optional, MCP worker pool size (default 8)
  GROK_QUEUE_SIZE       optional, MCP request queue size (default 64)
  GROK_QUEUE_WAIT_TIMEOUT optional, how long a tools/call waits for a queue slot when the queue is full
                        before it is rejected with an isError result (default 10s; 0 rejects at once; max 10m; stdio only)
  GROK_HTTP_ADDR        optional, serve MCP over HTTP at this address (e.g. :8080); --http overrides it
  GROK_HTTP_API_KEY     required for HTTP, bearer token clients must present (Authorization or X-API-Key)
  GROK_MCP_TOOLS        optional, core|all (default core; all adds async task tools); --tools overrides it
  TAVILY_API_KEY        optional, enables Tavily tiers (web_fetch extract, web_map, web_search extra_sources)
  FIRECRAWL_API_KEY     optional, enables Firecrawl tiers (web_fetch scrape, web_search extra_sources)
`)
}

func runSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	model := fs.String("model", "", "per-call model override (default: the configured $GROK_MODEL)")
	platform := fs.String("platform", "", "platform focus (e.g. GitHub, Reddit)")
	extraSources := fs.Int("extra-sources", 0, "extra reference sources from Tavily/Firecrawl search (0 disables)")
	timeout := fs.Duration("timeout", 0, "request timeout override (e.g. 60s); 0 uses the config default")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(os.Stderr, `error: missing query
usage: openscry search [--model M] [--platform P] [--timeout D] "your query"`)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	client := grok.NewClientWithLimit(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout, cfg.UpstreamConcurrency)
	provider := config.ResolveSearchProvider(cfg.SearchProvider, cfg.APIBaseURL)
	svc := search.NewWithOptions(client, cfg.Model, search.Options{
		Profiles:    searchProfiles(cfg),
		Provider:    provider,
		RefProvider: newRefProvider(cfg),
		Tools:       cfg.SearchTools,
	})

	// --timeout 作为显式预算参数交给服务层；不给上下文设截止时间，服务层才能区分
	// "调用方要求的预算"与"传输层上限"。
	res, err := svc.Search(context.Background(), search.Request{Query: query, Platform: *platform, Model: *model, ExtraSources: *extraSources, Timeout: *timeout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "search failed:", err)
		return 1
	}
	fmt.Println(res.Content)
	if res.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", res.Warning)
	}
	// 流未确认完整时在 stderr 单独给一行状态，与 MCP 尾注 > completion: 同一口径；
	// stdout 仍只有正文，管道消费方不受影响。
	if res.CompletionState != grok.StateComplete {
		fmt.Fprintf(os.Stderr, "completion: %s (%s)\n", res.CompletionState, res.CompletionDetail)
	}
	if res.ServerToolCallsKnown {
		fmt.Fprintf(os.Stderr, "tools: %d server-side calls; elapsed: %.1fs\n", res.ServerToolCalls, res.Elapsed.Seconds())
	} else {
		fmt.Fprintf(os.Stderr, "elapsed: %.1fs\n", res.Elapsed.Seconds())
	}
	if r := res.ExtraSources; r != nil {
		fmt.Fprintf(os.Stderr, "extra_sources: requested %d, added %d, %d duplicated model sources", r.Requested, r.Added, r.Duplicates)
		if len(r.Failed) > 0 {
			fmt.Fprintf(os.Stderr, ", failed: %s", strings.Join(r.Failed, ","))
		}
		fmt.Fprintln(os.Stderr)
	}
	// Sources go to stderr so stdout stays clean for piping the answer.
	if len(res.Sources) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d sources:\n", len(res.Sources))
		for i, s := range res.Sources {
			if s.Origin != "" {
				fmt.Fprintf(os.Stderr, "  [%d] %s — %s (via %s)\n", i+1, s.Title, s.URL, s.Origin)
			} else if s.Title != "" {
				fmt.Fprintf(os.Stderr, "  [%d] %s — %s\n", i+1, s.Title, s.URL)
			} else {
				fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, s.URL)
			}
		}
	}
	return 0
}

func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	httpAddr := fs.String("http", "", "serve MCP over HTTP at this address (e.g. :8080) instead of stdio; overrides GROK_HTTP_ADDR")
	toolsFlag := fs.String("tools", "", "advertised tool set: core (default) or all (adds async task tools); overrides GROK_MCP_TOOLS")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	// Logs go to stderr; stdout is reserved exclusively for the MCP
	// JSON-RPC stream.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.Debug)}))
	client := grok.NewClientWithLimit(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout, cfg.UpstreamConcurrency)
	provider := config.ResolveSearchProvider(cfg.SearchProvider, cfg.APIBaseURL)
	searchSvc := search.NewWithOptions(client, cfg.Model, search.Options{
		Profiles:    searchProfiles(cfg),
		Provider:    provider,
		RefProvider: newRefProvider(cfg),
		Tools:       cfg.SearchTools,
	})
	fetchSvc := fetch.New(client, fetch.Options{
		Model:           cfg.Model,
		Strict:          cfg.FetchFallback == "strict",
		TavilyAPIKey:    cfg.TavilyAPIKey,
		TavilyAPIURL:    cfg.TavilyAPIURL,
		FirecrawlAPIKey: cfg.FirecrawlAPIKey,
		FirecrawlAPIURL: cfg.FirecrawlAPIURL,
		Tools:           cfg.FetchTools(),
	})

	srv := mcpserver.NewWithConfig(os.Stdin, os.Stdout, logger, mcpserver.EngineConfig{
		MaxConcurrentRequests: cfg.Concurrency,
		RequestQueueSize:      cfg.QueueSize,
		QueueWaitTimeout:      engineQueueWait(cfg.QueueWaitTimeout),
	})
	mapSvc := mapper.New(mapper.Options{
		TavilyAPIKey: cfg.TavilyAPIKey,
		TavilyAPIURL: cfg.TavilyAPIURL,
	})

	planSvc := planner.New(client, planner.Options{Model: cfg.Model, Profiles: searchProfiles(cfg)})

	// Resolve the advertised tool set: --tools overrides GROK_MCP_TOOLS.
	toolset := cfg.MCPTools
	if strings.TrimSpace(*toolsFlag) != "" {
		v, terr := config.NormalizeToolset(*toolsFlag)
		if terr != nil {
			fmt.Fprintln(os.Stderr, "config error:", terr)
			return 1
		}
		toolset = v
	}

	// Resolve the transport: the --http flag overrides GROK_HTTP_ADDR; empty
	// means stdio.
	addr := strings.TrimSpace(*httpAddr)
	if addr == "" {
		addr = cfg.HTTPAddr
	}
	transport := "stdio"
	if addr != "" {
		transport = "http"
	}

	// Core tools: the request/response surface every agent drives.
	srv.Register(mcpserver.WebSearchTool(searchSvc))
	srv.Register(mcpserver.WebFetchTool(fetchSvc))
	srv.Register(mcpserver.WebMapTool(mapSvc))
	srv.Register(mcpserver.ResearchPlanTool(planSvc))
	srv.Register(mcpserver.WebSearchBatchTool(searchSvc))

	info := mcpserver.ConfigInfo{
		Name:                mcpserver.ServerName,
		Version:             version,
		Protocol:            mcpserver.MCPProtocolVersion,
		Transport:           transport,
		Model:               cfg.Model,
		BaseURL:             cfg.APIBaseURL,
		Provider:            provider,
		FetchFallback:       cfg.FetchFallback,
		RequestTimeout:      cfg.RequestTimeout.String(),
		Concurrency:         cfg.Concurrency,
		QueueSize:           cfg.QueueSize,
		QueueWaitTimeout:    cfg.QueueWaitTimeout.String(),
		UpstreamConcurrency: cfg.UpstreamConcurrency,
		Tavily:              cfg.TavilyAPIKey != "",
		Firecrawl:           cfg.FirecrawlAPIKey != "",
		Toolset:             toolset,
		SearchTools:         cfg.SearchTools,
	}
	srv.Register(mcpserver.GetConfigInfoTool(info, client.Ping))

	// Async task-lifecycle tools are opt-in via --tools all / GROK_MCP_TOOLS=all.
	// The capability is always compiled in; this only gates the advertised
	// surface so the default agent sees a minimal six-tool selection.
	if toolset == "all" {
		taskStore := tasks.NewStore()
		srv.Register(mcpserver.SubmitSearchTaskTool(taskStore, searchSvc))
		srv.Register(mcpserver.GetSearchTaskResultTool(taskStore))
		srv.Register(mcpserver.CancelSearchTaskTool(taskStore))
		srv.Register(mcpserver.ListSearchTasksTool(taskStore))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Info("openscry.signal", "signal", s.String())
		cancel()
	}()

	tools := "web_search,web_fetch,web_map,research_plan,web_search_batch,get_config_info"
	if toolset == "all" {
		tools += ",submit_search_task,get_search_task_result,cancel_search_task,list_search_tasks"
	}
	logger.Info("openscry.mcp_serving",
		"model", cfg.Model, "base_url", cfg.APIBaseURL,
		"provider", provider, "fetch_fallback", cfg.FetchFallback,
		"transport", transport, "toolset", toolset,
		"tools", tools,
		"search_tools", strings.Join(cfg.SearchTools, ","),
		"tavily", cfg.TavilyAPIKey != "", "firecrawl", cfg.FirecrawlAPIKey != "",
		"concurrency", cfg.Concurrency, "queue_size", cfg.QueueSize,
		"queue_wait_timeout", cfg.QueueWaitTimeout.String(),
		"upstream_concurrency", cfg.UpstreamConcurrency,
		"debug", cfg.Debug)

	var serveErr error
	if transport == "http" {
		if cfg.HTTPAPIKey == "" {
			fmt.Fprintln(os.Stderr, "config error: GROK_HTTP_API_KEY is required when serving over HTTP (refusing an unauthenticated /mcp endpoint)")
			return 1
		}
		serveErr = srv.ServeHTTP(ctx, mcpserver.HTTPOptions{
			Addr:           addr,
			APIKey:         cfg.HTTPAPIKey,
			ReadinessProbe: client.Ping,
			ConfigInfo:     info.Map(),
			// Admission cap mirrors the stdio engine's total in-flight
			// capacity (worker pool + bounded queue) so HTTP bounds local
			// request processing the way stdio does; the upstream-concurrency
			// limiter separately bounds grok2api load.
			MaxInFlight: cfg.Concurrency + cfg.QueueSize,
		})
	} else {
		serveErr = srv.Serve(ctx)
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		logger.Error("openscry.serve_ended", "err", serveErr.Error())
		return 1
	}
	return 0
}

func runFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 0, "total fetch budget (e.g. 60s); 0 uses the configured fetch timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if target == "" {
		fmt.Fprintln(os.Stderr, `error: missing url
usage: openscry fetch [--timeout D] <url>`)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	client := grok.NewClientWithLimit(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout, cfg.UpstreamConcurrency)
	fetchSvc := fetch.New(client, fetch.Options{
		Model:           cfg.Model,
		Strict:          cfg.FetchFallback == "strict",
		TavilyAPIKey:    cfg.TavilyAPIKey,
		TavilyAPIURL:    cfg.TavilyAPIURL,
		FirecrawlAPIKey: cfg.FirecrawlAPIKey,
		FirecrawlAPIURL: cfg.FirecrawlAPIURL,
		Tools:           cfg.FetchTools(),
	})

	// --timeout 作为显式预算参数交给服务层，0 表示使用 OpFetch 档位（30s）。
	res, err := fetchSvc.FetchWithTimeout(context.Background(), target, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch failed:", err)
		return 1
	}
	// Degradation is visible: report the winning tier on stderr so stdout
	// stays clean for piping the fetched content.
	fmt.Fprintf(os.Stderr, "fetched via tier=%s\n", res.Tier)
	fmt.Println(res.Content)
	return 0
}

func runMap(args []string) int {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	depth := fs.Int("depth", 1, "maximum traversal depth from root (default 1)")
	breadth := fs.Int("breadth", 20, "maximum links to follow per page (default 20)")
	limit := fs.Int("limit", 50, "total URL cap (default 50)")
	instructions := fs.String("instructions", "", "natural-language filter for the crawl")
	timeout := fs.Duration("timeout", 0, "total map budget (e.g. 60s); 0 uses the configured OpMap timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if target == "" {
		fmt.Fprintln(os.Stderr, `error: missing url
usage: openscry map [--depth N] [--breadth N] [--limit N] [--instructions S] [--timeout D] <url>`)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	mapSvc := mapper.New(mapper.Options{
		TavilyAPIKey: cfg.TavilyAPIKey,
		TavilyAPIURL: cfg.TavilyAPIURL,
	})

	// --timeout 作为显式预算参数交给服务层，0 表示使用 OpMap 档位（90s）。
	res, err := mapSvc.Map(context.Background(), mapper.Request{
		URL:          target,
		MaxDepth:     *depth,
		MaxBreadth:   *breadth,
		Limit:        *limit,
		Instructions: *instructions,
		Timeout:      *timeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "map failed:", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "mapped via tier=%s, found %d URLs\n", res.Tier, len(res.URLs))
	if res.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", res.Warning)
	}
	for _, u := range res.URLs {
		fmt.Println(u)
	}
	return 0
}

func runPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 0, "total plan budget (e.g. 60s); 0 uses the configured search timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Fprintln(os.Stderr, `error: missing research question
usage: openscry plan [--timeout D] "your research question"`)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return 1
	}

	client := grok.NewClientWithLimit(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout, cfg.UpstreamConcurrency)
	planSvc := planner.New(client, planner.Options{Model: cfg.Model, Profiles: searchProfiles(cfg)})

	// --timeout 作为显式预算参数交给服务层，0 表示使用搜索档位（GROK_REQUEST_TIMEOUT）。
	plan, err := planSvc.PlanWithTimeout(context.Background(), question, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plan failed:", err)
		return 1
	}

	// Output the structured plan as indented JSON for readability.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(plan); err != nil {
		fmt.Fprintln(os.Stderr, "encode error:", err)
		return 1
	}
	return 0
}
