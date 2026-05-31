# openscry

Grok-backed web search, built as a **CLI core + thin MCP adapter** in Go (single binary, zero external dependencies).

openscry is the Go rewrite of the Python `GrokSearch` MCP server. It consumes
`grok2api` (OpenAI-compatible reverse proxy) and exposes capabilities both as
reproducible CLI subcommands and as MCP tools for clients like Windsurf/Cascade.

## Tools

| CLI | MCP Tool | Description |
|-----|----------|-------------|
| `openscry search` | `web_search` | Deep web search via grok2api |
| `openscry fetch` | `web_fetch` | URL content extraction (Tavily/Firecrawl/Grok/HTTP multi-tier) |
| `openscry map` | `web_map` | Site structure discovery (Tavily /map or HTTP BFS crawl) |
| `openscry plan` | `research_plan` | Offline research plan generation (model-driven structured JSON) |

## Design principles

- **Model is user-supplied, never defaulted.** `GROK_MODEL` is required.
  An unavailable model fails explicitly (exit 1 / MCP `isError=true`),
  never silently switches to another model.
- **Degradation is visible.** web_fetch reports which tier produced the
  result; `GROK_FETCH_FALLBACK=strict` disables the low-fidelity basic-HTTP
  fallback entirely.
- **One code path.** CLI and MCP adapter share the same core packages
  (`internal/search`, `internal/fetch`, `internal/mapper`, `internal/planner`).
- **Resilience built-in.** Circuit breaker, bounded retry with shared budget,
  per-operation timeout profiles (search 120s / fetch 30s / map 90s).

## Build

```sh
go build -o openscry ./cmd/openscry
```

## Configure

```sh
# Required
export GROK_API_URL=https://your-grok2api-host/v1
export GROK_API_KEY=your-api-key
export GROK_MODEL=grok-4.20-fast    # user-supplied, never defaulted

# Optional
export GROK_REQUEST_TIMEOUT=120s
export GROK_SEARCH_PROVIDER=auto    # auto|chat|responses
export GROK_FETCH_FALLBACK=full     # full|strict
export GROK_CONCURRENCY=8           # MCP worker pool size
export TAVILY_API_KEY=              # enables Tavily tiers in fetch/map
export FIRECRAWL_API_KEY=           # enables Firecrawl tier in fetch
```

See `.env.example` for the complete reference.

## Usage

```sh
# Search
./openscry search "How does Go's GC work?"
./openscry search --model grok-4.3-console --platform GitHub "best Go MCP SDK"

# Fetch URL content
./openscry fetch https://go.dev/doc/effective_go

# Map site structure
./openscry map --depth 2 --limit 20 https://pkg.go.dev/std

# Generate research plan
./openscry plan "Compare Go and Rust memory management approaches"

# Run MCP adapter (stdio JSON-RPC, 4 tools)
./openscry mcp

# Version
./openscry version
```

## Windsurf MCP configuration

To use openscry as an MCP server in Windsurf, add to your `mcp_config.json`:

```json
{
  "mcpServers": {
    "openscry-mcp": {
      "command": "/path/to/openscry",
      "args": ["mcp"],
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
internal/search/       search core (resilience layer: breaker + retry + timeout)
internal/fetch/        web_fetch multi-tier fallback chain (Tavily/Firecrawl/Grok/HTTP)
internal/mapper/       web_map site structure discovery (Tavily /map + HTTP BFS crawl)
internal/planner/      research_plan generator (model-driven structured JSON output)
internal/prompt/       system prompts (SearchPrompt, FetchPrompt, PlanPrompt)
internal/resilience/   circuit breaker, retry budget, timeout profiles
internal/mcpserver/    MCP adapter (stdio JSON-RPC, concurrent engine, 4 tools)
```

## Test

```sh
go test ./...           # 50+ tests
go test -race ./...     # with race detector
```
