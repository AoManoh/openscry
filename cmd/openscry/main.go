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

	"github.com/AoManoh/openscry/internal/config"
	"github.com/AoManoh/openscry/internal/fetch"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/mapper"
	"github.com/AoManoh/openscry/internal/mcpserver"
	"github.com/AoManoh/openscry/internal/planner"
	"github.com/AoManoh/openscry/internal/refsource"
	"github.com/AoManoh/openscry/internal/search"
	"github.com/AoManoh/openscry/internal/tasks"
)

const version = mcpserver.ServerVersion

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
  openscry mcp        run the thin MCP adapter over stdio (tools: web_search, web_fetch, web_map, research_plan)
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

	client := grok.NewClient(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout)
	provider := config.ResolveSearchProvider(cfg.SearchProvider, cfg.APIBaseURL)
	svc := search.NewWithOptions(client, cfg.Model, search.Options{
		Provider:    provider,
		RefProvider: newRefProvider(cfg),
	})

	to := cfg.RequestTimeout
	if *timeout > 0 {
		to = *timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()

	res, err := svc.Search(ctx, search.Request{Query: query, Platform: *platform, Model: *model, ExtraSources: *extraSources})
	if err != nil {
		fmt.Fprintln(os.Stderr, "search failed:", err)
		return 1
	}
	fmt.Println(res.Content)
	if res.Warning != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", res.Warning)
	}
	// Sources go to stderr so stdout stays clean for piping the answer.
	if len(res.Sources) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d sources:\n", len(res.Sources))
		for i, s := range res.Sources {
			if s.Title != "" {
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := grok.NewClient(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout)
	provider := config.ResolveSearchProvider(cfg.SearchProvider, cfg.APIBaseURL)
	searchSvc := search.NewWithOptions(client, cfg.Model, search.Options{
		Provider:    provider,
		RefProvider: newRefProvider(cfg),
	})
	fetchSvc := fetch.New(client, fetch.Options{
		Model:           cfg.Model,
		Strict:          cfg.FetchFallback == "strict",
		TavilyAPIKey:    cfg.TavilyAPIKey,
		TavilyAPIURL:    cfg.TavilyAPIURL,
		FirecrawlAPIKey: cfg.FirecrawlAPIKey,
		FirecrawlAPIURL: cfg.FirecrawlAPIURL,
	})

	srv := mcpserver.NewWithConfig(os.Stdin, os.Stdout, logger, mcpserver.EngineConfig{
		MaxConcurrentRequests: cfg.Concurrency,
		RequestQueueSize:      cfg.QueueSize,
	})
	mapSvc := mapper.New(mapper.Options{
		TavilyAPIKey: cfg.TavilyAPIKey,
		TavilyAPIURL: cfg.TavilyAPIURL,
	})

	planSvc := planner.New(client, planner.Options{Model: cfg.Model})

	// Process-level async task store backing the submit/get/cancel/list tools.
	taskStore := tasks.NewStore()

	srv.Register(mcpserver.WebSearchTool(searchSvc))
	srv.Register(mcpserver.WebFetchTool(fetchSvc))
	srv.Register(mcpserver.WebMapTool(mapSvc))
	srv.Register(mcpserver.ResearchPlanTool(planSvc))
	srv.Register(mcpserver.WebSearchBatchTool(searchSvc))
	srv.Register(mcpserver.SubmitSearchTaskTool(taskStore, searchSvc))
	srv.Register(mcpserver.GetSearchTaskResultTool(taskStore))
	srv.Register(mcpserver.CancelSearchTaskTool(taskStore))
	srv.Register(mcpserver.ListSearchTasksTool(taskStore))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Info("openscry.signal", "signal", s.String())
		cancel()
	}()

	logger.Info("openscry.mcp_serving",
		"model", cfg.Model, "base_url", cfg.APIBaseURL,
		"provider", provider, "fetch_fallback", cfg.FetchFallback,
		"tools", "web_search,web_fetch,web_map,research_plan,web_search_batch,submit_search_task,get_search_task_result,cancel_search_task,list_search_tasks",
		"tavily", cfg.TavilyAPIKey != "", "firecrawl", cfg.FirecrawlAPIKey != "",
		"concurrency", cfg.Concurrency, "queue_size", cfg.QueueSize)
	if err := srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("openscry.serve_ended", "err", err.Error())
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

	client := grok.NewClient(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout)
	fetchSvc := fetch.New(client, fetch.Options{
		Model:           cfg.Model,
		Strict:          cfg.FetchFallback == "strict",
		TavilyAPIKey:    cfg.TavilyAPIKey,
		TavilyAPIURL:    cfg.TavilyAPIURL,
		FirecrawlAPIKey: cfg.FirecrawlAPIKey,
		FirecrawlAPIURL: cfg.FirecrawlAPIURL,
	})

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	res, err := fetchSvc.Fetch(ctx, target)
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

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	res, err := mapSvc.Map(ctx, mapper.Request{
		URL:          target,
		MaxDepth:     *depth,
		MaxBreadth:   *breadth,
		Limit:        *limit,
		Instructions: *instructions,
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

	client := grok.NewClient(cfg.APIBaseURL, cfg.APIKey, cfg.RequestTimeout)
	planSvc := planner.New(client, planner.Options{Model: cfg.Model})

	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	plan, err := planSvc.Plan(ctx, question)
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
