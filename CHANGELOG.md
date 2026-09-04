# 更新日志

本文件面向 openscry 的使用者，记录每个发布版本可感知的变化。版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)，日期为 tag 推送日。

## [0.2.1] - 2026-09-04

依据一次真实调研任务的使用反馈（8 项）做的修复与改进；升级不改任何接口，不需要重建配置。

### 修复

- **`web_fetch` 对 GitHub blob/raw 页面改为取文件原文**：`github.com/{owner}/{repo}/blob|raw/{ref}/{path}` 自动改写为 `raw.githubusercontent.com/...`（去掉 `#L` 行锚点），此前各抓取层拿到的是带行号的页面骨架；首行注释以 `fetched=` 标出实际抓取地址，`url=` 仍是调用方给的地址。
- **相邻重复引用折叠**：模型把同一引用连写两遍（`[[3]](url)[[3]](url)`）时折叠为一个；指向不同 URL 的连续引用不受影响。
- **`research_plan` 执行顺序按依赖重排**：`parallel_groups` 现在是按 `depends_on` 推导的分层（同层并行、层间顺序、覆盖全部子查询），`sequential` 只列有前置的子查询；此前模型常把只依赖同一前置的多个子查询全部串行。缺少搜索词的 `web_search` 子查询用其 goal 兜底补一条 round 1 搜索词。
- 异步任务的 `submitted_at` / `started_at` / `finished_at` 统一为 UTC。

### 改进

- **`extra_sources` 结算可见**：传了 `extra_sources` 时，`web_search` 尾部追加 `> extra_sources: requested N, added M, K duplicated model sources[, failed: 提供方]`；批量 / 异步 JSON 带 `extra_sources` 对象（含 `by_origin`）。回答"要了 3 条为什么只多了 1 条"。
- `research_plan` 返回 `elapsed_s`。
- 搜索提示词新增三条规则：许可证只在读过 LICENSE 文件（或官方仓库许可证字段）后陈述，并引用决定性条款，否则写"license not verified"；列举 / 比较软件项目时报告 stars 与最近发布 / 提交日期并确认仓库路径存在；发现类问题（"有哪些项目 / 工具"）多次换词搜索，包含领域内的知名候选，并明说哪些知名候选未能核实。

### 升级方法

```bash
go install github.com/AoManoh/openscry/cmd/openscry@v0.2.1
```

MCP 配置把 `@v0.2.0` 改为 `@v0.2.1` 后重启会话；`openscry version` 应输出 `openscry v0.2.1`。

## [0.2.0] - 2026-09-03

openscry 的首个正式发布版本。openscry 是把 grok2api（OpenAI 兼容上游）当作联网检索引擎使用的 Go 单二进制：CLI 核心 + 薄 MCP 适配层，为 AI IDE / agent 提供带引用的联网搜索、多级网页抓取、站点地图与调研规划。此前的 `v0.2.0-s5` 至 `v0.2.0-s11` 均为预发布构建，本版取代它们；`go install …@latest` 自此解析到 `v0.2.0`。

### 核心能力（首次发布的完整功能面）

- MCP 工具面：core 五个（`web_search`、`web_search_batch`、`web_fetch`、`web_map`、`research_plan`），`--tools all` 再加异步任务族与配置查询（`submit_search_task`、`get_search_task_result`、`list_search_tasks`、`cancel_search_task`、`get_config_info`）；stdio JSON-RPC 默认，HTTP JSON-RPC 可选。CLI 同一套核心代码：`search` / `fetch` / `map` / `plan` / `mcp` / `version`。
- 联网搜索走 grok2api 的托管检索工具：请求显式声明 `GROK_SEARCH_TOOLS`（默认 `web_search`，Console 模型可加 `x_search`），答案逐句内联引用 `[[n]](url)`，尾部 `Sources` 列表与编号一致；上游 `url_citation` 注解补齐正文未标注的来源。
- `web_fetch` 四级链路：Tavily -> Firecrawl -> Grok -> basic HTTP，输出首行标注命中层级；`GROK_FETCH_FALLBACK=strict` 禁用低保真兜底，Grok 层对"页面不可用 / 只取到部分正文"显式失败而不是当成功返回。
- fail-loud：模型由用户指定绝不默认；不可用模型、非法请求、上游错误原样显式失败（`isError=true` / exit 1），绝不静默换模型；答案无来源且上游未报告检索时附带 warning。
- 弹性层：按操作分档的超时、重试与熔断，全局上游并发限流与 HTTP 准入控制，异步任务持久化与取消。

### 相对预发布构建（`v0.2.0-s5` 至 `s11`）的变化，按老用户可感知排序

- **检索真的发生**：搜索请求向上游声明 `web_search` / `x_search` 托管工具；此前对 grok2api v3 的请求不带工具声明，模型只凭内置知识作答。
- **检索可观测**：`web_search` 结果尾部新增 `> tools: N server-side calls`（上游报告的托管工具调用次数）与 `> elapsed:`；批量 / 异步结果带 `server_tool_calls`、`elapsed_s`。
- **一手来源规则**：版本号、发布日期、发布状态与官方公告必须逐字取自一手来源（官方站点、发布清单、官方仓库 tag / release 页、官方博客），追踪站与聚合站只能佐证，并区分"已发布"与"计划中 / 进行中"。
- **引用结构**：`[[n]]` 编号按最终 Sources 列表重排；上游注解里只是序号的标题不再被渲染成 `[2](url)`；重复 URL 的后到项回填缺失标题。
- **来源出处**：`extra_sources` 补充的来源在 Sources 行尾标注 ` — via tavily|firecrawl`，JSON 带 `origin`；Firecrawl 参考检索此前因只按 v2 响应形态解析而从未成功，现已修复。
- **抓取**：`web_fetch` 的 Grok 层在 `GROK_SEARCH_TOOLS` 含 `web_search` 时声明该工具，模型可经 browse_page / open_page 真正打开目标 URL；模型只取到部分正文时输出 `OPENSCRY_FETCH_PARTIAL`，抓取层按该层失败处理。
- **错误分类**：HTTP 400 按上游 `error.code` 区分 `model_unavailable` 与新增的 `invalid_request`（如不支持的 `x_search`）；错误文本只保留第一个 JSON 对象，不再拼接 SSE 帧。
- **版本号来自 Go build 信息**：`openscry version`、MCP `serverInfo.version` 与 `get_config_info.version` 输出发布 tag（`v0.2.0`）、本地构建的伪版本或 `devel`，不再依赖硬编码常量。
- 新增配置项 `GROK_SEARCH_TOOLS`；`.env.example`、README（中英）同步。

### 升级方法

```bash
go install github.com/AoManoh/openscry/cmd/openscry@v0.2.0
```

MCP 配置以 `go run` 在线启动的，把 `args` 中的 `github.com/AoManoh/openscry/cmd/openscry@main`（或 `@v0.2.0-sN`）改为 `@v0.2.0` 后重启会话。升级后用 `openscry version` 确认输出 `openscry v0.2.0`。

[0.2.1]: https://github.com/AoManoh/openscry/releases/tag/v0.2.1
[0.2.0]: https://github.com/AoManoh/openscry/releases/tag/v0.2.0
