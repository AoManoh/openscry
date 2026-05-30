# openscry

Grok-backed web search, built as a **CLI core + thin MCP adapter** in Go (single binary).

openscry is the Go rewrite of the Python `GrokSearch` MCP server. It consumes
`grok2api` (OpenAI-compatible) and exposes search both as reproducible CLI
subcommands and as MCP tools for clients like Windsurf/Cascade.

> Status: **stage S1 (skeleton + spike)**. Only `web_search` is implemented as a
> minimal end-to-end closed loop. Resilience (circuit breaker / backpressure /
> timeout layering), `web_fetch`, `web_map`, and the deep planner land in later
> stages. See the staged plan in
> `../docs/refactor/2026-05-30-openscry-go-rewrite.md`.

## Design principles

- **Model is configurable, never hard-coded.** Default `grok-4.3-console`,
  overridable via `GROK_MODEL` or `--model`.
- **Failures are explicit.** A model/provider failure returns a structured
  error and never silently switches to a different model. An empty result is
  reported as an explicit error, not a hollow success.
- **One search code path.** The CLI and the MCP adapter both call
  `internal/search`, so behaviour cannot drift between them.

## Build

```sh
go build -o openscry ./cmd/openscry
```

## Configure

Copy `.env.example` to `.env` and fill in `GROK_API_URL` and `GROK_API_KEY`,
or export them in your shell. See `.env.example` for all variables.

## Usage

CLI search:

```sh
GROK_API_URL=https://host/v1 GROK_API_KEY=... \
  ./openscry search --platform GitHub "best Go MCP SDK"
```

Run the thin MCP adapter over stdio (exposes the `web_search` tool):

```sh
GROK_API_URL=https://host/v1 GROK_API_KEY=... ./openscry mcp
```

Version:

```sh
./openscry version
```

## Layout

```
cmd/openscry/        entry point + subcommand dispatch
internal/config/     env-based configuration (fail-loud)
internal/grok/       grok2api OpenAI-compatible client + typed errors
internal/prompt/     search system prompt + time context
internal/search/     transport-agnostic search core
internal/mcpserver/  thin MCP adapter (stdio JSON-RPC) + web_search tool
```

## Test

```sh
go test ./...
```
