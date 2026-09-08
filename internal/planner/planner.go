package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AoManoh/openscry/internal/grok"
	"github.com/AoManoh/openscry/internal/resilience"
)

// Service generates structured research plans via the configured grok model.
// It is safe for concurrent use.
type Service struct {
	client   *grok.Client
	model    string
	profiles resilience.Profiles
}

// Options configures the planner service.
type Options struct {
	Model    string
	Profiles resilience.Profiles
}

// New builds a planner Service.
func New(client *grok.Client, opt Options) *Service {
	return &Service{
		client:   client,
		model:    strings.TrimSpace(opt.Model),
		profiles: opt.Profiles.Normalize(),
	}
}

// Plan generates a structured research plan for the given question. It calls
// the grok model with PlanPrompt and parses the returned JSON into a Plan.
// Errors are fail-loud: a model failure or unparseable response surfaces
// explicitly rather than returning a partial/empty plan.
func (s *Service) Plan(ctx context.Context, question string) (*Plan, error) {
	return s.PlanWithTimeout(ctx, question, 0)
}

// PlanWithTimeout 与 Plan 相同，但允许调用方给出显式预算（CLI --timeout / MCP timeout
// 参数）；timeout <= 0 表示使用 OpSearch 档位（规划在延迟预期上属于搜索类操作）。预算
// 以参数传递而不是由调用方给上下文设截止时间，是为了让服务层能区分"调用方要求的预算"
// 与"传输层的请求上限"：后者只应收紧预算，不应替代默认预算。
func (s *Service) PlanWithTimeout(ctx context.Context, question string, timeout time.Duration) (*Plan, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("planner: question must not be empty")
	}

	// 总是按预算建立操作上下文，父上下文更早的截止时间由 context 自动保留。不再因父上下文
	// 已有截止时间而跳过档位，否则 MCP HTTP 传输的请求上限会冒充规划预算。
	budget := timeout
	if budget <= 0 {
		budget = s.profiles.For(resilience.OpSearch)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	completion, err := s.client.CompleteDetailed(ctx, s.model, PlanPrompt, question, nil)
	if err != nil {
		return nil, fmt.Errorf("planner: model call failed: %w", err)
	}
	if completion.State != grok.StateComplete {
		// 规划是一个整体 JSON：流未确认完整（截断 / 过滤 / 未收到 [DONE]）时，残缺的 JSON
		// 要么解析失败并给出误导性的"格式错误"，要么恰好可解析却缺了子查询或搜索词。
		// 两种结果都不能交给执行方，因此直接以完整性错误失败，不再尝试解析。
		return nil, fmt.Errorf("planner: model response incomplete (%s: %s)", completion.State, completion.Detail)
	}
	raw := completion.Content

	plan, err := parsePlan(raw)
	if err != nil {
		return nil, fmt.Errorf("planner: failed to parse model response as plan: %w\nraw response: %.500s", err, raw)
	}

	if err := validatePlan(plan); err != nil {
		return nil, fmt.Errorf("planner: plan validation failed: %w", err)
	}
	normalizePlan(plan)

	return plan, nil
}

// normalizePlan 对模型给出的规划做确定性修正，不改变其意图：
//  1. execution 按 depends_on 重新推导：同一层（前置全部满足）的子查询并行，层与层顺序执行；
//     模型常把只依赖同一个前置的多个子查询全部塞进 sequential，白白串行。
//  2. 每个 tool_hint 为空或 web_search 的子查询至少有一条搜索词；缺失时用其 goal 兜底生成，
//     并标记 round 1。
func normalizePlan(p *Plan) {
	ids := make(map[string]int, len(p.SubQueries))
	for i, sq := range p.SubQueries {
		ids[sq.ID] = i
	}
	deps := func(sq SubQuery) []string {
		var out []string
		for _, d := range splitDependsOn(sq.DependsOn) {
			if _, ok := ids[d]; ok && d != sq.ID {
				out = append(out, d)
			}
		}
		return out
	}
	level := make(map[string]int, len(p.SubQueries))
	var visit func(id string, depth int) int
	visit = func(id string, depth int) int {
		if l, ok := level[id]; ok {
			return l
		}
		// 深度保护只防死循环：成环的计划已在 validatePlan 被拒绝，不会走到这里；
		// 保留它是为了让直接调用 normalizePlan 的路径在任何输入下都能终止。
		if depth > len(p.SubQueries) {
			return 1
		}
		l := 1
		for _, d := range deps(p.SubQueries[ids[id]]) {
			if dl := visit(d, depth+1) + 1; dl > l {
				l = dl
			}
		}
		level[id] = l
		return l
	}
	maxLevel := 0
	for _, sq := range p.SubQueries {
		if l := visit(sq.ID, 0); l > maxLevel {
			maxLevel = l
		}
	}
	groups := make([][]string, maxLevel)
	var sequential []string
	for _, sq := range p.SubQueries {
		l := level[sq.ID]
		groups[l-1] = append(groups[l-1], sq.ID)
		if l > 1 {
			sequential = append(sequential, sq.ID)
		}
	}
	p.Execution = Execution{ParallelGroups: groups, Sequential: sequential}

	covered := make(map[string]bool, len(p.SearchTerms))
	for _, t := range p.SearchTerms {
		for _, pid := range strings.Split(t.Purpose, ",") {
			covered[strings.TrimSpace(pid)] = true
		}
	}
	for _, sq := range p.SubQueries {
		if covered[sq.ID] || (sq.ToolHint != "" && sq.ToolHint != "web_search") {
			continue
		}
		p.SearchTerms = append(p.SearchTerms, SearchTerm{Term: sq.Goal, Purpose: sq.ID, Round: 1})
	}
}

// parsePlan extracts a Plan from the model's raw response. It handles common
// model quirks: markdown code fences wrapping the JSON, leading/trailing
// whitespace, and BOM characters.
func parsePlan(raw string) (*Plan, error) {
	cleaned := strings.TrimSpace(raw)

	// Strip markdown JSON code fences if the model wrapped the output.
	if strings.HasPrefix(cleaned, "```") {
		// Remove opening fence (```json or ```)
		if idx := strings.Index(cleaned, "\n"); idx >= 0 {
			cleaned = cleaned[idx+1:]
		}
		// Remove closing fence
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}

	var plan Plan
	if err := json.Unmarshal([]byte(cleaned), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

// splitDependsOn 把 depends_on 的逗号分隔文本拆成去空白后的非空 ID 列表。校验与规范化
// 共用同一个拆分规则，两处对"什么算一个依赖"才不会各自演化。
func splitDependsOn(raw string) []string {
	var out []string
	for _, d := range strings.Split(raw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// validatePlan 校验解析后的规划在结构上完整、在依赖关系上可执行。依赖校验的必要性：
// normalizePlan 按 depends_on 分层时，重复 ID 会让同一个 ID 进入分组两次，未知依赖与
// 自依赖会被静默忽略，环会触发深度保护并留下空层——这些结果看起来像成功，交给执行方
// 却无法按计划运行，因此必须在这里拒绝并点名出错的 ID。
func validatePlan(p *Plan) error {
	if p.Intent.CoreQuestion == "" {
		return fmt.Errorf("intent.core_question is empty")
	}
	if p.Complexity.Level < 1 || p.Complexity.Level > 3 {
		return fmt.Errorf("complexity.level must be 1-3, got %d", p.Complexity.Level)
	}
	if len(p.SubQueries) == 0 {
		return fmt.Errorf("sub_queries must not be empty")
	}
	ids := make(map[string]int, len(p.SubQueries))
	for i, sq := range p.SubQueries {
		if sq.ID == "" {
			return fmt.Errorf("sub_queries[%d].id is empty", i)
		}
		if sq.Goal == "" {
			return fmt.Errorf("sub_queries[%d].goal is empty", i)
		}
		if first, dup := ids[sq.ID]; dup {
			return fmt.Errorf("sub_queries[%d].id %q is a duplicate of sub_queries[%d].id", i, sq.ID, first)
		}
		ids[sq.ID] = i
	}
	if len(p.SearchTerms) == 0 {
		return fmt.Errorf("search_terms must not be empty")
	}
	// 先收齐全部 ID 再检查依赖：前向引用（依赖声明在被依赖项之前）是合法的。
	for i, sq := range p.SubQueries {
		for _, d := range splitDependsOn(sq.DependsOn) {
			if d == sq.ID {
				return fmt.Errorf("sub_queries[%d] (%s) depends_on itself", i, sq.ID)
			}
			if _, ok := ids[d]; !ok {
				return fmt.Errorf("sub_queries[%d] (%s) depends_on unknown sub-query %q", i, sq.ID, d)
			}
		}
	}
	if cycle := findDependencyCycle(p.SubQueries, ids); len(cycle) > 0 {
		return fmt.Errorf("sub_queries depends_on forms a cycle: %s", strings.Join(cycle, " -> "))
	}
	return nil
}

// findDependencyCycle 在 depends_on 图里寻找一个环，返回参与环的 ID 序列（首尾为同一 ID，
// 表示闭合）；无环返回 nil。用深度优先着色而不是沿用 normalizePlan 的深度计数，是因为
// 后者只能察觉"走得太深"，说不出是哪些 ID 互相依赖，而调用方要修计划必须知道这一点。
// 前提：ID 唯一且依赖均存在（validatePlan 已在此前保证）；不存在的依赖在这里直接跳过。
func findDependencyCycle(sqs []SubQuery, ids map[string]int) []string {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(sqs))
	var path []string
	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		path = append(path, id)
		for _, d := range splitDependsOn(sqs[ids[id]].DependsOn) {
			if _, ok := ids[d]; !ok {
				continue
			}
			switch state[d] {
			case visiting:
				for i, pid := range path {
					if pid == d {
						return append(append([]string(nil), path[i:]...), d)
					}
				}
			case unvisited:
				if cycle := visit(d); cycle != nil {
					return cycle
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return nil
	}
	for _, sq := range sqs {
		if state[sq.ID] == unvisited {
			if cycle := visit(sq.ID); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}
