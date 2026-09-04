[English](./README.en.md) | 简体中文

# openscry

基于 Grok 的网页搜索 / 抓取 / 站点测绘，以 **CLI 核心 + 薄 MCP 适配层** 的形态用 Go 实现（单文件二进制，零外部依赖）。

openscry 是 Python 版 `GrokSearch` MCP 服务的 Go 重写。它消费 `grok2api`（OpenAI 兼容反代），把能力同时暴露为可复现的 CLI 子命令和供 Windsurf/Cascade 等客户端使用的 MCP 工具。

## 能力

| CLI                 | MCP 工具             | 说明                                                          |
| ------------------- | -------------------- | ------------------------------------------------------------- |
| `openscry search` | `web_search`       | 经 grok2api 的深度网页搜索（可选 extra_sources 参考信源补强） |
| `openscry fetch`  | `web_fetch`        | URL 正文提取（Tavily/Firecrawl/Grok/HTTP 多级回退）           |
| `openscry map`    | `web_map`          | 站点结构发现（Tavily /map 或 HTTP BFS 爬取）                  |
| `openscry plan`   | `research_plan`    | 离线研究计划生成（模型驱动的结构化 JSON）                     |
| —                  | `web_search_batch` | 并发批量搜索（多 query，故障逐项隔离，上限 32）               |
| —                  | `get_config_info`  | 运行时配置与上游连通性自检（绝不泄露密钥）                    |

MCP 工具面通过 `--tools` 分级：

- **core（默认，6 个）**：web_search / web_fetch / web_map / research_plan / web_search_batch / get_config_info
- **all（10 个）**：core + 异步任务族 submit_search_task / get_search_task_result / cancel_search_task / list_search_tasks

## 设计原则

- **模型由用户指定，绝不默认。** `GROK_MODEL` 必填；不可用的模型显式失败（exit 1 / MCP `isError=true`），绝不静默切换到其他模型。
- **降级可见。** web_fetch 报告结果由哪一级产出；`GROK_FETCH_FALLBACK=strict` 完全禁用低保真的 basic-HTTP 回退；搜索答案解析不出任何来源时附带 warning。
- **检索可观测。** `web_search` 结果尾部附 `> model:`、`> tools: N server-side calls`（上游报告的托管工具调用次数，缺失表示上游未提供）与 `> elapsed:`；传了 `extra_sources` 时再附 `> extra_sources: requested N, added M, K duplicated model sources[, failed: …]`；批量/异步结果含 `server_tool_calls`、`elapsed_s`、`extra_sources` 字段；正文内联引用 `[[n]](url)` 的 n 与 Sources 列表序号一致。`web_fetch` 会把 GitHub blob/raw 页面改写为 raw.githubusercontent.com 取文件原文，首行注释以 `fetched=` 标出实际地址。
- **搜索工具由请求显式声明。** 每次搜索都在请求的 `tools` 数组声明托管搜索工具（默认 `web_search`，可加 `x_search`），不依赖上游"自动搜索"；grok2api v3 的 Console 路由只执行客户端声明的工具。`GROK_SEARCH_TOOLS=none` 可关闭。
- **单一代码路径。** CLI 与 MCP 适配层共用同一套核心包（`internal/search` / `fetch` / `mapper` / `planner`）。
- **内建弹性。** 熔断器、共享预算的有界重试、分操作超时档（search 120s / fetch 30s / map 90s）、尊重上游 Retry-After。

## 构建

```sh
go build -o openscry ./cmd/openscry
```

## 配置

```sh
# 必填
export GROK_API_URL=https://你的-grok2api-host/v1
export GROK_API_KEY=你的-api-key
export GROK_MODEL=grok-4.20-fast      # 用户指定，绝不默认

# 可选
export GROK_REQUEST_TIMEOUT=120s
export GROK_SEARCH_PROVIDER=auto      # auto|chat|responses
export GROK_SEARCH_TOOLS=web_search   # 搜索请求声明的托管工具，逗号分隔；可加 x_search；none 关闭
export GROK_FETCH_FALLBACK=full       # full|strict
export GROK_CONCURRENCY=8             # MCP worker 池大小（stdio 请求处理）
export GROK_QUEUE_SIZE=64             # MCP 有界队列容量
export GROK_UPSTREAM_CONCURRENCY=32   # 全局并发上游 grok2api 调用上限（两种传输都生效）；0 禁用
export GROK_MCP_TOOLS=core            # core|all，MCP 暴露的工具面
export GROK_HTTP_ADDR=                # 设置后 mcp 走 HTTP（如 127.0.0.1:8080）
export GROK_HTTP_API_KEY=             # HTTP 传输强制鉴权密钥
export TAVILY_API_KEY=                # 启用 fetch/map 的 Tavily 级
export FIRECRAWL_API_KEY=             # 启用 fetch 的 Firecrawl 级
```

完整参考见 `.env.example`。

`GROK_SEARCH_TOOLS` 说明：默认 `web_search`，对 grok2api v3 的 Console 与 Grok Web 路由都可用；`web_search,x_search` 额外检索 X（Twitter）内容，但 Grok Web 路由不支持 `x_search`（上游返回 400，openscry 原样透出），只在 `GROK_MODEL` 指向 Console 模型（如 `grok-4.3`）时追加；`none` 恢复只发 `model/messages/stream` 的旧请求形态。工具作用于 `web_search` / `web_search_batch` / 异步搜索任务；`web_fetch` 的 Grok 层只在清单含 `web_search` 时声明 `web_search`（xAI 的 web_search 工具组含 browse_page / open_page，模型借此才能真正打开目标 URL），`research_plan` 按离线设计不带工具。

## 使用

```sh
./openscry search "Go 的 GC 如何工作？"
./openscry search --model grok-4.3-console --platform GitHub "最好的 Go MCP SDK"
./openscry fetch https://go.dev/doc/effective_go
./openscry map --depth 2 --limit 20 https://pkg.go.dev/std
./openscry plan "对比 Go 与 Rust 的内存管理"

# MCP 适配层（默认 stdio JSON-RPC；core 工具面）
./openscry mcp
./openscry mcp --tools all              # 暴露全部 10 个工具（含异步任务族）
./openscry mcp --http 127.0.0.1:8080    # 走 HTTP JSON-RPC（需 GROK_HTTP_API_KEY）

./openscry version   # 输出 build 信息中的版本：发布 tag（v0.2.1）、本地构建伪版本或 devel
```

## 在 IDE中配置 MCP

在 `mcp_config.json` 的 `mcpServers` 中加入 openscry。**推荐在线方式**——无需本地构建，`go run` 会自动从 GitHub 拉取源码并编译；固定到发布 tag（如 `@v0.2.1`）而不是 `@main`，升级时改 tag 后重启会话即可（版本记录见 [CHANGELOG.md](CHANGELOG.md)）：

```json
{
  "mcpServers": {
    "openscry-mcp": {
      "command": "go",
      "args": ["run", "github.com/AoManoh/openscry/cmd/openscry@v0.2.1", "mcp", "--tools", "all"],
      "env": {
        "GROK_API_URL": "https://你的-host/v1",
        "GROK_API_KEY": "你的-key",
        "GROK_MODEL": "grok-4.20-fast"
      }
    }
  }
}
```

首次启动会拉取并编译（稍慢），之后走缓存秒级启动。

**国内用户网络不好时**，在 `env` 中追加下面两项，走国内镜像加速拉取（海外或网络通畅的用户可省略）：

```json
"GOPROXY": "https://goproxy.cn,direct",
"GOSUMDB": "sum.golang.google.cn"
```

若首次因校验失败，把 `GOSUMDB` 改为 `off` 即可。

也可以**先本地构建**，再用二进制路径（适合离线或锁定版本）：

```json
{
  "mcpServers": {
    "openscry-mcp": {
      "command": "/path/to/openscry",
      "args": ["mcp", "--tools", "all"],
      "env": {
        "GROK_API_URL": "https://你的-host/v1",
        "GROK_API_KEY": "你的-key",
        "GROK_MODEL": "grok-4.20-fast"
      }
    }
  }
}
```

## 目录结构

```
cmd/openscry/          入口 + 子命令分发（基于 flag，无框架）
internal/config/       基于环境变量的配置（缺 GROK_MODEL 时 fail-loud）
internal/grok/         grok2api OpenAI 兼容流式客户端 + 类型化错误
internal/search/       搜索核心 + 批量（弹性层：熔断 + 重试 + 超时）
internal/sources/      信源提取与归一化（多策略，去重合并）
internal/refsource/    extra_sources 参考信源补强（Tavily/Firecrawl /search）
internal/fetch/        web_fetch 多级回退链（Tavily/Firecrawl/Grok/HTTP）
internal/mapper/       web_map 站点结构发现（Tavily /map + HTTP BFS 爬取）
internal/planner/      research_plan 生成器（模型驱动结构化 JSON）
internal/prompt/       系统提示词
internal/resilience/   熔断器、重试预算、超时档
internal/tasks/        异步任务 store（goroutine + 取消 + 长轮询 + LRU）
internal/mcpserver/    MCP 适配层（stdio/HTTP JSON-RPC，并发引擎，工具面门控）
```
