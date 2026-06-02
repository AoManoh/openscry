English | [简体中文](./README.md)

# openscry

Grok-backed web search, fetch and site mapping, built as a **CLI core + thin MCP adapter** in Go (single binary, zero external dependencies).

openscry is the Go rewrite of the Python `GrokSearch` MCP server. It consumes `grok2api` (an OpenAI-compatible reverse proxy) and exposes its capabilities both as reproducible CLI subcommands and as MCP tools for clients like Windsurf/Cascade.

## Capabilities

| CLI | MCP Tool | Description |
|-----|----------|-------------|
| `openscry search` | `web_search` | Deep web search via grok2api (optional extra_sources enrichment) |
| `openscry fetch` | `web_fetch` | URL content extraction (Tavily/Firecrawl/Grok/HTTP multi-tier fallback) |
| `openscry map` | `web_map` | Site structure discovery (Tavily /map or HTTP BFS crawl) |
| `openscry plan` | `research_plan` | Offline research plan generation (model-driven structured JSON) |
| — | `web_search_batch` | Concurrent batch search (multi-query, per-item failure isolation, capped at 32) |
| — | `get_config_info` | Runtime config + upstream connectivity self-check (never leaks secrets) |

The MCP tool surface is gated by `--tools`:
- **core (default, 6):** web_search, web_fetch, web_map, research_plan, web_search_batch, get_config_info
- **all (10):** core + async task family submit_search_task, get_search_task_result, cancel_search_task, list_search_tasks

## Design principles

- **Model is user-supplied, never defaulted.** `GROK_MODEL` is required. An unavailable model fails explicitly (exit 1 / MCP `isError=true`), never silently switches.
- **Degradation is visible.** web_fetch reports which tier produced the result; `GROK_FETCH_FALLBACK=strict` disables the low-fidelity basic-HTTP fallback.
- **One code path.** CLI and MCP adapter share the same core packages.
- **Resilience built-in.** Circuit breaker, bounded retry with a shared budget, per-operation timeout profiles (search 120s / fetch 30s / map 90s), upstream Retry-After honored.

## Build

```sh
go build -o openscry ./cmd/openscry
```

## Configure

```sh
# Required
export GROK_API_URL=https://your-grok2api-host/v1
export GROK_API_KEY=your-api-key
export GROK_MODEL=grok-4.20-fast      # user-supplied, never defaulted

# Optional
export GROK_REQUEST_TIMEOUT=120s
export GROK_SEARCH_PROVIDER=auto      # auto|chat|responses
export GROK_FETCH_FALLBACK=full       # full|strict
export GROK_CONCURRENCY=8             # MCP worker pool size
export GROK_QUEUE_SIZE=64             # MCP bounded queue capacity
export GROK_MCP_TOOLS=core            # core|all, advertised MCP tool surface
export GROK_HTTP_ADDR=                # when set, mcp serves over HTTP (e.g. 127.0.0.1:8080)
export GROK_HTTP_API_KEY=             # required auth key for the HTTP transport
export TAVILY_API_KEY=                # enables Tavily tiers in fetch/map
export FIRECRAWL_API_KEY=             # enables Firecrawl tier in fetch
```

See `.env.example` for the complete reference.

## Usage

```sh
./openscry search "How does Go's GC work?"
./openscry search --model grok-4.3-console --platform GitHub "best Go MCP SDK"
./openscry fetch https://go.dev/doc/effective_go
./openscry map --depth 2 --limit 20 https://pkg.go.dev/std
./openscry plan "Compare Go and Rust memory management"

# MCP adapter (stdio JSON-RPC by default; core tool surface)
./openscry mcp
./openscry mcp --tools all              # expose all 10 tools (incl. async task family)
./openscry mcp --http 127.0.0.1:8080    # serve over HTTP JSON-RPC (requires GROK_HTTP_API_KEY)

./openscry version
```

## Windsurf MCP configuration

```json
{
  "mcpServers": {
    "openscry-mcp": {
      "command": "/path/to/openscry",
      "args": ["mcp", "--tools", "all"],
      "env": {
        "GROK_API_URL": "https://your-host/v1",
        "GROK_API_KEY": "your-key",
        "GROK_MODEL": "grok-4.20-fast"
      }
    }
  }
}
```

## Layout

```
cmd/openscry/          entry point + subcommand dispatch (flag-based, no frameworks)
internal/config/       env-based configuration (fail-loud on missing GROK_MODEL)
internal/grok/         grok2api OpenAI-compatible streaming client + typed errors
internal/search/       search core + batch (resilience layer: breaker + retry + timeout)
internal/sources/      source extraction + normalization (multi-strategy, dedup merge)
internal/refsource/    extra_sources reference enrichment (Tavily/Firecrawl /search)
internal/fetch/        web_fetch multi-tier fallback chain (Tavily/Firecrawl/Grok/HTTP)
internal/mapper/       web_map site structure discovery (Tavily /map + HTTP BFS crawl)
internal/planner/      research_plan generator (model-driven structured JSON output)
internal/prompt/       system prompts
internal/resilience/   circuit breaker, retry budget, timeout profiles
internal/tasks/        async task store (goroutine + cancel + long-poll + LRU)
internal/mcpserver/    MCP adapter (stdio/HTTP JSON-RPC, concurrent engine, tool gating)
```
