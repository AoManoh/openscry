# 更新日志

本文件面向 openscry 的使用者，记录每个发布版本可感知的变化。版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)，日期为 tag 推送日。

## [Unreleased]

自 v0.2.1 以来尚未发版的变化。所有条目已逐项与当前源码核对；不改任何接口形态，已有配置无需重建，新增的配置项都有默认值。

### 修复

- **引用整理不再误删正文**：来源提取里"只含链接的行"改为整行判定，一行只有在去掉列表前缀后完全由 Markdown 链接或裸 URL 组成（之间只允许空白与中英文逗号、分号、顿号，行尾只允许句末标点）时才算链接行。此前以普通 Markdown 链接结尾的正文句子（例如"……尚未获官方确认。[官方公告](url)"）会被当成尾部来源列表整段删除。新增兜底策略：前面所有提取策略都未命中时，正文里的普通 `[文本](url)` 链接按出现顺序收集为来源（按 URL 去重，链接文本作为标题），正文原样保留；此前这类答案会因为没有来源而触发"检索已执行但答案没有可解析引用"的告警。
- **`GROK_REQUEST_TIMEOUT` 对所有搜索类路径生效**：该值现在是搜索类操作（`web_search` / `web_search_batch` / 异步搜索任务 / `research_plan`）的默认预算，对 CLI、MCP stdio、MCP HTTP 一致生效。此前 `openscry mcp` 没有把它传给服务层，MCP 下的搜索实际用的是固定的 120s 档位，与 `get_config_info` 报告的 `request_timeout` 不一致；CLI 下大于 120s 的配置值也会被该档位截短。显式超时参数（CLI 各子命令的 `--timeout`，MCP `web_fetch` / `web_map` / `research_plan` 的 `timeout`）按传入值作为该次操作的预算，不再被默认档位截短（此前 `openscry search --timeout 300s` 实际仍受 120s 限制）。HTTP 传输的 10 分钟请求上限只在预算之外兜底，不再替代 `web_fetch`（30s）、`web_map`（90s）与 `research_plan` 的默认预算；此前这三者在父上下文已有截止时间时跳过自己的档位，同一次调用在 stdio 与 HTTP 下的超时行为不一致。
- **`research_plan` 校验依赖关系**：拒绝子查询 ID 重复、`depends_on` 指向不存在的子查询、依赖自身以及依赖成环的计划，错误信息点名出错的 ID（成环时给出环路径）。此前重复 ID 会让同一子查询进入分组两次，未知依赖与自依赖被静默忽略，成环触发深度保护后留下无法执行的分层，这些计划看起来成功却无法按计划运行。
- **`GROK_DEBUG=true` 生效**：`openscry mcp` 的 stderr 日志级别降为 Debug（可见 `mcp.req_completed`、`mcp.queue_full_waiting`、`mcp.queue_wait_admitted` 等记录）。此前配置被读取但从未使用，日志级别固定为 Info。级别只改变过滤阈值，不改变任何日志字段，密钥不会因此进入日志。
- **`extra_sources` 不再掩盖无引用告警**：无引用判定只统计模型自身的来源（正文解析出的引用与上游 `url_citation` 注解），`extra_sources` 补充的条目不计入。此前按合并后的总数判定，没有任何引用的答案只要传了 `extra_sources` 就不再出现告警。当来源列表非空却全部是补充项时，告警后紧跟一句说明：这些条目是参考检索给出的阅读候选，模型作答时并未使用，不构成该答案的证据。

### 改进

- **流式响应完整性可见**：每次上游流式响应都判定完整性：`complete`（收到 `[DONE]` 且 `finish_reason` 为空、`stop` 或 `end_turn`，或 `finish_reason` 明确为 `stop` / `end_turn`）、`truncated`（`finish_reason=length`，模型达到输出上限）、`filtered`（`finish_reason=content_filter`）、`unconfirmed`（流在 `[DONE]` 之前结束且没有 `finish_reason`，或出现未知的 `finish_reason`）。`web_search` 非 `complete` 时 warning 首句说明响应未确认完整、答案未提及的内容应视为未知，并在 `> model:` 之后单独一行 `> completion: <state> (<detail>)`；正文仍原样返回，不重试（`length` 重试仍会在同一处截断，连接中断后重试会让上游成本加倍）。批量 / 异步 JSON 里有正文的结果总是带 `completion_state`，非 `complete` 时再带 `completion_detail`；异步任务的 `state` 仍是 `completed`，完整性只通过结果字段披露。CLI `openscry search` 非 `complete` 时在 stderr 输出一行 `completion: <state> (<detail>)`，stdout 不变。`web_fetch` 的 Grok 层把未确认完整的响应按该层失败处理（`grok tier: response incomplete (...)`），继续降级到 basic HTTP，或在 `GROK_FETCH_FALLBACK=strict` 下显式失败。`research_plan` 对未确认完整的响应直接以 `planner: model response incomplete (...)` 失败，不再尝试解析残缺的 JSON。
- **stdio 队列饱和时有限等待并显式拒绝，新增 `GROK_QUEUE_WAIT_TIMEOUT`**：stdio 传输的请求队列满时，`tools/call` 不再在接收循环上同步执行，而是最多等待 `GROK_QUEUE_WAIT_TIMEOUT`（默认 `10s`；`0` 立即拒绝；超过 `10m` 裁剪为 `10m`；负值或无法解析的值启动即报错）争取空位。等不到、等待名单已满（同时等待的调用最多 `GROK_QUEUE_SIZE` 个）或服务正在关机时，以 `isError=true` 的工具结果拒绝，文本以 `openscry MCP server overloaded` 开头，写明拒绝原因、其它在飞请求数、worker 数、队列与等待名单容量、实际等待时长以及稍后重试或降低并发的建议。`ping`、`tools/list`、`initialize` 等协议请求因此在任何负载下都能即时响应。HTTP 传输不受影响，满载仍返回 `503` 与 `Retry-After`。
- `get_config_info` 与 HTTP 的 `/.well-known/mcp-config` 新增 `queue_wait_timeout`（`0s` 表示立即拒绝）；引擎启动日志 `mcp.engine_started` 增加 `queue_wait_timeout` 与 `max_queue_waiters`，关机日志 `mcp.engine_stopped` 的 `fallback` 计数改为 `rejected` 与 `waited`。

### 说明

- **异步任务存储勘误**：`[0.2.0]` 条目写的"异步任务持久化"不成立。`submit_search_task` 提交的任务存放在进程内存中，容量 256：超容时先驱逐最旧的终态任务；若 256 个都未结束，则取消并删除最旧的运行中任务，该任务随后对 `get_search_task_result` 不可见。任务不跨服务重启保留，重启后对旧 `task_id` 查询返回任务不存在。本轮不改变该行为，只修正文档并补充 `submit_search_task` 的工具描述；`internal/tasks` 新增记录现状的测试与基准，为后续容量决策提供依据。
- **测试文件纳入版本控制**：`.gitignore` 不再忽略 `*_test.go`，各包的测试文件随源码提交。对 `go install` / `go run` 的使用者没有影响。

### 升级方法

待发版。发版后本条目改为 `[X.Y.Z] - 日期`，升级命令与 MCP 配置中的 tag 以发版时的版本号为准。

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
- 弹性层：按操作分档的超时、重试与熔断，全局上游并发限流与 HTTP 准入控制，异步任务持久化与取消。勘误（2026-09-07）：异步任务存储是进程内存、有界 256（超容时先驱逐最旧的终态任务，若全部未结束则取消并删除最旧的运行中任务），不跨服务重启保留，此处"持久化"的表述不成立，以本勘误为准。

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

[Unreleased]: https://github.com/AoManoh/openscry/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/AoManoh/openscry/releases/tag/v0.2.1
[0.2.0]: https://github.com/AoManoh/openscry/releases/tag/v0.2.0
