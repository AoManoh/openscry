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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/AoManoh/openscry/internal/config"
	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/mcpserver"
	"github.com/AoManoh/openscry/internal/search"
)

const version = "0.1.0-s1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch cmd := os.Args[1]; cmd {
	case "search":
		os.Exit(runSearch(os.Args[2:]))
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
  openscry mcp        run the thin MCP adapter over stdio (one tool: web_search)
  openscry version    print version

environment:
  GROK_API_URL          required, OpenAI-compatible base URL of grok2api (e.g. https://host/v1)
  GROK_API_KEY          required
  GROK_MODEL            optional, default `+config.DefaultModel+`
  GROK_REQUEST_TIMEOUT  optional, default 120s (accepts "120" seconds or "2m")
`)
}

func runSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	model := fs.String("model", "", "model override (default: $GROK_MODEL or built-in default)")
	platform := fs.String("platform", "", "platform focus (e.g. GitHub, Reddit)")
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
	svc := search.New(client, cfg.Model)

	to := cfg.RequestTimeout
	if *timeout > 0 {
		to = *timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()

	res, err := svc.Search(ctx, search.Request{Query: query, Platform: *platform, Model: *model})
	if err != nil {
		fmt.Fprintln(os.Stderr, "search failed:", err)
		return 1
	}
	fmt.Println(res.Content)
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
	svc := search.New(client, cfg.Model)

	srv := mcpserver.New(os.Stdin, os.Stdout, logger)
	srv.Register(mcpserver.WebSearchTool(svc))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Info("openscry.signal", "signal", s.String())
		cancel()
	}()

	logger.Info("openscry.mcp_serving", "model", cfg.Model, "base_url", cfg.APIBaseURL)
	if err := srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("openscry.serve_ended", "err", err.Error())
		return 1
	}
	return 0
}
