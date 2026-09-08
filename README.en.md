English | [简体中文](./README.md)

# openscry

Grok-backed web search, fetch and site mapping, built as a **CLI core + thin MCP adapter** in Go (single binary, zero external dependencies).

openscry is the Go rewrite of the Python `GrokSearch` MCP server. It consumes `grok2api` (an OpenAI-compatible reverse proxy) and exposes its capabilities both as reproducible CLI subcommands and as MCP tools for clients like Windsurf/Cascade.

## Capabilities

| CLI                 | MCP Tool             | Description                                                                     |
| ------------------- | -------------------- | ------------------------------------------------------------------------------- |
| `openscry search` | `web_search`       | Deep web search via grok2api (optional extra_sources enrichment)                |
| `openscry fetch`  | `web_fetch`        | URL content extraction (Tavily/Firecrawl/Grok/HTTP multi-tier fallback)         |
| `openscry map`    | `web_map`          | Site structure discovery (Tavily /map or HTTP BFS crawl)                        |
| `openscry plan`   | `research_plan`    | Offline research plan generation (model-driven structured JSON)                 |
| —                  | `web_search_batch` | Concurrent batch search (multi-query, per-item failure isolation, capped at 32) |
| —                  | `get_config_info`  | Runtime config + upstream connectivity self-check (never leaks secrets)         |

The MCP tool surface is gated by `--tools`:

- **core (default, 6):** web_search, web_fetch, web_map, research_plan, web_search_batch, get_config_info
- **all (10):** core + async task family submit_search_task, get_search_task_result, cancel_search_task, list_search_tasks

Async task storage semantics: tasks live in process memory only, bounded to 256; when the bound is exceeded the oldest finished tasks are evicted first, and if all 256 are still running the oldest running task is cancelled and dropped. Tasks do not survive a server restart; after a restart, querying an old `task_id` reports that the task does not exist.

## Design principles

- **Model is user-supplied, never defaulted.** `GROK_MODEL` is required. An unavailable model fails explicitly (exit 1 / MCP `isError=true`), never silently switches.
- **Degradation is visible.** web_fetch reports which tier produced the result; `GROK_FETCH_FALLBACK=strict` disables the low-fidelity basic-HTTP fallback; a search answer with no parsable sources carries a warning. The no-citation check counts only the model's own sources (citations parsed from the body plus upstream `url_citation` annotations); entries added by `extra_sources` do not count, and when the Sources list consists solely of such entries a second sentence states that they are not evidence for the answer. When the upstream stream is not confirmed complete (truncated / filtered / no `[DONE]` received), `web_search` still returns the generated text but the warning opens by stating that the response is unconfirmed and that anything it does not state must be treated as unknown; the Grok tier of `web_fetch` treats an unconfirmed response as a tier failure (falls through, or fails loud under strict); `research_plan` fails instead of parsing a possibly truncated JSON.
- **Retrieval is observable.** `web_search` results end with `> model:`, `> tools: N server-side calls` (hosted tool calls reported by the upstream; absent when the upstream does not report it) and `> elapsed:`; when the stream is not confirmed complete, a `> completion: <state> (<detail>)` line follows `> model:` (state is `truncated` / `filtered` / `unconfirmed`; omitted when `complete`); with `extra_sources` set, also `> extra_sources: requested N, added M, K duplicated model sources[, failed: …]`; batch/async results carry `server_tool_calls`, `elapsed_s` and `extra_sources`, every result with content carries `completion_state` and, when not `complete`, `completion_detail`; inline `[[n]](url)` numbering matches the Sources list. `web_fetch` rewrites GitHub blob/raw pages to raw.githubusercontent.com to get the file itself and reports the actual address as `fetched=` in the header comment.
- **Search tools are declared on the request.** Every search declares hosted search tools in the request `tools` array (default `web_search`, optionally `x_search`) instead of relying on upstream "implicit search"; grok2api v3's Console route only runs tools the client declares. `GROK_SEARCH_TOOLS=none` disables it.
- **One code path.** CLI and MCP adapter share the same core packages.
- **Resilience built-in.** Circuit breaker, bounded retry with a shared budget, per-operation timeout profiles (search takes `GROK_REQUEST_TIMEOUT`, default 120s / fetch 30s / map 90s), upstream Retry-After honored.

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
export GROK_REQUEST_TIMEOUT=120s      # default budget for search-class operations (web_search / web_search_batch / async search / research_plan); fetch 30s and map 90s are fixed profiles; an explicit timeout argument wins
export GROK_SEARCH_PROVIDER=auto      # auto|chat|responses
export GROK_SEARCH_TOOLS=web_search   # hosted tools declared on search requests, comma-separated; add x_search; none disables
export GROK_FETCH_FALLBACK=full       # full|strict
export GROK_CONCURRENCY=8             # MCP worker pool size (stdio request processing)
export GROK_QUEUE_SIZE=64             # MCP bounded queue capacity
export GROK_QUEUE_WAIT_TIMEOUT=10s    # max wait for a queue slot when the queue is full; 0 rejects at once; max 10m; stdio only
export GROK_UPSTREAM_CONCURRENCY=32   # global cap on simultaneous grok2api calls (both transports); 0 disables
export GROK_MCP_TOOLS=core            # core|all, advertised MCP tool surface
export GROK_HTTP_ADDR=                # when set, mcp serves over HTTP (e.g. 127.0.0.1:8080)
export GROK_HTTP_API_KEY=             # required auth key for the HTTP transport
export TAVILY_API_KEY=                # enables Tavily tiers in fetch/map
export FIRECRAWL_API_KEY=             # enables Firecrawl tier in fetch
export GROK_DEBUG=false               # true enables Debug-level stderr logging in openscry mcp
```

See `.env.example` for the complete reference.

About `GROK_QUEUE_WAIT_TIMEOUT`: it only governs the stdio transport's request-processing queue (`GROK_CONCURRENCY` workers plus a bounded queue of `GROK_QUEUE_SIZE`). When the queue is full, a `tools/call` is no longer executed synchronously on the receive loop; it waits up to this long for a slot and, if none frees up, is rejected with an `isError=true` tool result that spells out the overload, the current in-flight count, the queue capacity, how long the call waited, and the advice to retry later or reduce concurrency. `ping`, `tools/list` and the other protocol requests therefore stay responsive under any load. Default `10s`; `0` rejects as soon as the queue is full; values above `10m` are clamped to `10m`; negative or unparsable values fail at startup. At most `GROK_QUEUE_SIZE` calls wait at a time; further calls are rejected immediately. The HTTP transport is unaffected: its admission control already answers `503` with `Retry-After` when full.

About `GROK_SEARCH_TOOLS`: the default `web_search` works on both the Console and Grok Web routes of grok2api v3; `web_search,x_search` additionally searches X (Twitter) content, but the Grok Web route rejects `x_search` (upstream 400, surfaced as-is by openscry), so only add it when `GROK_MODEL` points at a Console model (e.g. `grok-4.3`); `none` restores the legacy request shape (`model/messages/stream` only). The tools apply to `web_search` / `web_search_batch` / async search tasks; the Grok tier of `web_fetch` declares `web_search` only when the list contains it (xAI's web_search tool group includes browse_page / open_page, which is what lets the model actually open the target URL), and `research_plan` stays tool-free by design.

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

./openscry version   # prints the version from build info: release tag (v0.2.1), local pseudo-version or devel
```

## IDE MCP configuration

Add openscry under `mcpServers` in your `mcp_config.json`. **Online mode is recommended** — no local build needed, `go run` pulls the source from GitHub and compiles it automatically. Pin a release tag (such as `@v0.2.1`) rather than `@main`; to upgrade, change the tag and restart the session (see [CHANGELOG.md](CHANGELOG.md)):

```json
{
  "mcpServers": {
    "openscry-mcp": {
      "command": "go",
      "args": ["run", "github.com/AoManoh/openscry/cmd/openscry@v0.2.1", "mcp", "--tools", "all"],
      "env": {
        "GROK_API_URL": "https://your-host/v1",
        "GROK_API_KEY": "your-key",
        "GROK_MODEL": "grok-4.20-fast"
      }
    }
  }
}
```

The first launch fetches and compiles (slightly slow); subsequent launches start in seconds from cache.

**For users in mainland China with poor connectivity**, add these two entries to `env` to pull through domestic mirrors (omit them if your network is fine or you are outside China):

```json
"GOPROXY": "https://goproxy.cn,direct",
"GOSUMDB": "sum.golang.google.cn"
```

If the first launch fails on checksum verification, set `GOSUMDB` to `off`.

You can also **build locally** and point to the binary (good for offline or pinned versions):

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
