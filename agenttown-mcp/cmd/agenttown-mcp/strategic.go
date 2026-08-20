package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// 日程校验常量。MCP 无 day-range flag，与 defaultDailyPlan 保持一致。
// 跨日仿真：活动时段从 07:00 到次日 06:00，06:00-07:00 是战略层规划时间
// （不安排 schedule），dayEndMinute = 1440 + 360 = 1800（归一化到次日坐标系，
// 供 normalizeDailyPlan 末段后延使用）。
const (
	dayStartMinute = 7 * 60  // 07:00（活动开始；06:00-07:00 为规划时间不覆盖）
	dayEndMinute   = 30 * 60 // 次日 06:00（= 1440 + 360，归一化坐标）
	minSlotMinutes = 60      // 时段最短时长，短于此会被丢弃

	planJitterMinutes = 30 // 时间节点随机波动幅度（±分钟），用于错开各 NPC 的活动开始时间
	planJitterMinGap  = 30 // 扰动后相邻节点最小间隔，保证时段不会被压扁成零/负时长
)

// dailyPlanItem 是战略层输出的单条计划。
type dailyPlanItem struct {
	Time string `json:"time"`
	Goal string `json:"goal"`
}

// strategicCaller 是 LLM 客户端的窄接口，便于单测 mock。
// SendWithSchema 用于战略层（Structured Outputs 硬约束）；
// SendWithSummary 用于记忆层等无 schema 的调用。
type strategicCaller interface {
	SendWithSummary(ctx context.Context, system, user string) (*llmtypes.Response, error)
	SendWithSchema(ctx context.Context, system, user, schemaName string, schema []byte) (*llmtypes.Response, error)
	ResetSession()
}

// dailyPlanSchema 是战略层输出的 JSON Schema（OpenAI Structured Outputs
// strict 模式）。goal 强制为纯字符串——解码级杜绝 LLM 把 goal 写成
// {"goal":"...","cmd":"..."} 之类的嵌套对象导致整包解析失败。
const dailyPlanSchema = `{
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "time": {"type": "string"},
      "goal": {"type": "string"}
    },
    "required": ["time", "goal"],
    "additionalProperties": false
  }
}`

// yesterdaySummaryForFirstDay 是首日启动时注入的"昨日总结"。
//
// 早期版本写死了"小柯/充电站"等具体人物和设施，但当 KB 不包含这些
// 元素时（如最小化测试 KB 或换地图运行），LLM 会被诱导在战略计划里
// 编造这些 KB 外概念。改为中性表述：只描述抽象活动模式（装配/休息/
// 充电），不点名任何人物或具体设施，由 LLM 根据 KB 自行具象化。
const yesterdaySummaryForFirstDay = "昨天按计划完成了车间装配。"

// generateDailyPlan 调 LLM 生成当日计划，返回格式化字符串（每行 "时段: 目标"）。
// 任一步失败均回退到 prompt.DefaultDailyPlan(kb)，保证战术层有目标可分解、
// 仿真不瘫痪。返回 "" 仅表示连兜底计划都没用上（理论上不会发生）。
// kb 用于注入【你的角色】+【世界知识】+【区域设施映射】段，让 LLM 看到 KB 内合法的
// zone/object/agent 名，避免编造 KB 外概念（如换 KB 后仍写"车间"）。
// registry 用于注入【可用能力】段，让 LLM 知道可用复合动作，避免规划无对应
// 动作的 goal（如"整理仪容"）。profiles 是 NPC persona override（profile.md），
// nil 时 AgentRole 仅走 KB → fallback。kb/registry/profiles == nil 时降级为对应段缺失。
// physical 注入【物理状态】段；nil 时 PhysicalLine 用默认满状态兜底。
// dayContext 注入【今日日程】段（每周日程上下文，由 weeklyschedule.WeeklyLine
// 预格式化）；"" 时跳过该段（禁用或 dayCount<0）。
func generateDailyPlan(ctx context.Context, sc strategicCaller, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, registry *CapabilityRegistry, logger *slog.Logger, yesterdaySummary string, physical *protocol.PhysicalState, dayContext string) string {
	var actions []protocol.CapabilityAction
	if registry != nil {
		actions = registry.EffectiveActions(agentID)
	}
	if yesterdaySummary == "" {
		yesterdaySummary = yesterdaySummaryForFirstDay
	}
	// System prompt：三模块结构（世界背景/人物背景/世界详细信息），
	// 由 world KB + capability registry 派生，会话内稳定可缓存。
	// User prompt：动态段（今日日程+物理状态+昨日总结）+ 七条规则 + 规划指令。
	system := prompt.BuildStrategicSystemPrompt(kb, profiles, agentID, actions)
	promptText := fmt.Sprintf(prompt.StrategicUserTemplate,
		prompt.BuildStrategicUserContext(agentID, profiles, physical, dayContext),
		"昨日总结："+yesterdaySummary,
		prompt.StrategicRules)
	logger.Info("[MCP→LLM/STRATEGIC-PROMPT]", "agent_id", agentID, "text", promptText)
	// 实际 prompt 文档：每次仿真留存 H-01 首次战略层 prompt（system+user）。
	dumpPromptDoc(agentID, "strategic", system, promptText, logger)

	resp, err := sc.SendWithSchema(ctx, system, promptText, "daily_plan", []byte(dailyPlanSchema))
	if err != nil {
		fallback := jitterPlanString(prompt.DefaultDailyPlan(kb))
		logger.Warn("[战略层] 计划生成失败，使用默认计划兜底",
			"agent_id", agentID, "err", err, "fallback", fallback)
		return fallback
	}
	sc.ResetSession() // 战略调用一次性使用，立即清链

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/STRATEGIC-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	items, err := parseDailyPlan(raw)
	if err != nil {
		fallback := jitterPlanString(prompt.DefaultDailyPlan(kb))
		logger.Warn("[战略层] 计划解析失败，使用默认计划兜底",
			"agent_id", agentID, "raw", truncateText(raw, 200), "err", err, "fallback", fallback)
		return fallback
	}
	items = normalizeDailyPlan(items)
	if len(items) == 0 {
		logger.Warn("[战略层] 计划校验后为空，使用默认计划兜底", "agent_id", agentID)
		return jitterPlanString(prompt.DefaultDailyPlan(kb))
	}
	// 时间节点 ±planJitterMinutes 随机扰动：错开各 NPC 的活动开始时间，
	// 时段切换（战术层分解触发点）随之落在扰动后的时间点上。
	// 扰动后钳位夜间结束节点：不得早于 06:00（见 clampNightEnd 注释）。
	items = clampNightEnd(jitterPlanNodes(items, planJitterMinutes))
	plan := formatDailyPlan(items)
	logger.Info("[战略层] 每日计划生成成功", "agent_id", agentID, "items", len(items), "plan", plan)
	return plan
}

// parseDailyPlan 从 LLM 原始输出中解析 JSON 数组。
// 容错：先剥 ```json 围栏，再提取首个 [..] 子串，再 unmarshal。
func parseDailyPlan(raw string) ([]dailyPlanItem, error) {
	s := strings.TrimSpace(raw)
	// 剥 markdown 围栏
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// 提取首个 [..]。容错：LLM 输出可能被上游截断（缺少末尾 ]），
	// 此时尝试补 ] 再 unmarshal；仍失败则报错。
	start := strings.Index(s, "[")
	if start < 0 {
		return nil, fmt.Errorf("no JSON array found")
	}
	end := strings.LastIndex(s, "]")
	var arrayStr string
	if end > start {
		arrayStr = s[start : end+1]
	} else {
		arrayStr = s[start:] + "]"
	}
	var items []dailyPlanItem
	if err := json.Unmarshal([]byte(arrayStr), &items); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return items, nil
}

// normalizeDailyPlan 校验并补全解析后的每日计划：
//  1. 丢弃时长 <60min 的时段（调度器按 60min 采样，短时段大概率不被命中）
//  2. 按起始时间排序
//     2.5 合并相邻同时段：LLM 偶发输出连续两个同名时段（实测连续两段睡眠
//     20:30-22:58 + 22:58-07:16），每次边界到期都会打断睡眠重新分解，
//     合并后由后续规则统一填补/后延
//  3. 首段前伸到 07:00（dayStartMinute；06:00-07:00 是规划时间，不覆盖）
//  4. 填补中间空白：前段 end < 后段 start 时延长前段
//  5. 末段后延到次日 06:00（若 LLM 只规划到 18:00，18:00-22:00 会触发 idle wait 瘫痪）；
//     跨午夜末段结束早于 06:00 的（如 23:29-00:54）同样后延——否则凌晨出现
//     空洞，睡眠段半夜到期被打断重睡
//
// 支持跨午夜 slot（如 "22:00-06:00"）：跨午夜时段时长按 end+1440-start 计算，
// 末段若已跨午夜且覆盖到 06:00 及以后则不后延。
// 全部被丢弃时返回 nil，调用方走 prompt.DefaultDailyPlan(kb) 兜底。
func normalizeDailyPlan(items []dailyPlanItem) []dailyPlanItem {
	// 1. 过滤短时段。跨午夜 slot（end <= start）时长按 end+1440-start 计算。
	valid := make([]dailyPlanItem, 0, len(items))
	for _, it := range items {
		start, end, ok := prompt.SplitPlanRange(it.Time)
		if !ok {
			continue
		}
		dur := end - start
		if end <= start {
			dur = end + 1440 - start // 跨午夜
		}
		if dur < minSlotMinutes {
			continue
		}
		valid = append(valid, it)
	}
	if len(valid) == 0 {
		return nil
	}
	// 2. 按起始时间排序。
	sort.Slice(valid, func(i, j int) bool {
		si, _, _ := prompt.SplitPlanRange(valid[i].Time)
		sj, _, _ := prompt.SplitPlanRange(valid[j].Time)
		return si < sj
	})
	// 2.5 合并相邻同 goal 时段（含跨午夜边界两侧的连续睡眠）。
	// 除字面完全相同外，还做"睡眠语义合并"：LLM 常用同义措辞把夜间拆成
	// 两个时段（实测"睡眠舱休息 20:49-23:25" + "睡眠舱睡眠 23:25-06:35"），
	// 两段都映射到 rest_at_residence(sleep)，边界到期会打断睡眠重睡。
	// isSleepSlotGoal 判定双方均为睡眠舱睡眠类活动时同样合并。
	merged := make([]dailyPlanItem, 0, len(valid))
	for _, it := range valid {
		if n := len(merged); n > 0 && (merged[n-1].Goal == it.Goal ||
			isSleepSlotGoal(merged[n-1].Goal) && isSleepSlotGoal(it.Goal)) {
			s, _, _ := prompt.SplitPlanRange(merged[n-1].Time)
			_, e, _ := prompt.SplitPlanRange(it.Time)
			merged[n-1].Time = prompt.FmtMinute(s) + "-" + prompt.FmtMinute(e)
			continue
		}
		merged = append(merged, it)
	}
	valid = merged
	// 3. 首段前伸到 dayStart。
	if s, e, ok := prompt.SplitPlanRange(valid[0].Time); ok && s > dayStartMinute {
		valid[0].Time = prompt.FmtMinute(dayStartMinute) + "-" + prompt.FmtMinute(e)
	}
	// 4. 填补中间空白（仅在非跨午夜段间）。
	for i := 0; i < len(valid)-1; i++ {
		_, ei, _ := prompt.SplitPlanRange(valid[i].Time)
		sj, _, _ := prompt.SplitPlanRange(valid[i+1].Time)
		// 跨午夜段（ei <= si）不参与中间空白填补——它已经是当天最后一段。
		if ei > 0 && ei < sj {
			si, _, _ := prompt.SplitPlanRange(valid[i].Time)
			valid[i].Time = prompt.FmtMinute(si) + "-" + prompt.FmtMinute(sj)
		}
	}
	// 5. 末段后延到 dayEnd（次日 06:00）。
	// 跨午夜末段（end <= start）若已覆盖到 06:00 及以后则不后延。
	// 非跨午夜末段：若 end < 22:00，后延到 22:00（夜间开始，避免日间 goal
	// 被拉到次日清晨）；若 22:00 <= end < 06:00（次日），后延到 06:00。
	// 这样无论 LLM 规划到几点都不会出现尾部空白瘫痪。
	last := valid[len(valid)-1]
	s, e, ok := prompt.SplitPlanRange(last.Time)
	if ok && e > s {
		nightStart := 22 * 60
		if e < nightStart {
			// 日间时段：后延到夜间开始 22:00。
			valid[len(valid)-1].Time = prompt.FmtMinute(s) + "-" + prompt.FmtMinute(nightStart)
		} else if e < dayEndMinute {
			// 夜间时段未覆盖到次日 06:00：后延到 06:00（次日）。
			valid[len(valid)-1].Time = prompt.FmtMinute(s) + "-" + prompt.FmtMinute(dayEndMinute)
		}
	}
	// 跨午夜末段：结束早于 06:00（如 23:29-00:54）→ 后延到 06:00，
	// 避免 00:54 起的凌晨空洞导致睡眠段半夜到期（见 clampNightEnd 注释）。
	valid = clampNightEnd(valid)
	return valid
}

// isSleepSlotGoal 判断 goal 是否为"睡眠舱睡眠类"活动，供
// normalizeDailyPlan 步骤 2.5 的语义合并使用。
//
// LLM 拆分夜间睡眠时常用不同措辞（休息/睡眠/睡觉/休眠），字面比对
// 无法合并。判定规则：
//   - 排除词：冥想/整理（同为 sleep_pod 交互但不是睡眠）、长椅/广场
//     （bench 休息）、上网/充电（居住区其他活动）
//   - 语境词：睡眠舱/休眠舱/居住区/住所/回舱（落在居住区）
//   - 活动词：睡/休息/休眠/歇
func isSleepSlotGoal(goal string) bool {
	for _, ex := range []string{"冥想", "整理", "长椅", "广场", "上网", "充电"} {
		if strings.Contains(goal, ex) {
			return false
		}
	}
	loc := false
	for _, l := range []string{"睡眠舱", "休眠舱", "居住区", "住所", "回舱"} {
		if strings.Contains(goal, l) {
			loc = true
			break
		}
	}
	if !loc {
		return false
	}
	for _, v := range []string{"睡", "休息", "休眠", "歇"} {
		if strings.Contains(goal, v) {
			return true
		}
	}
	return false
}

// clampNightEnd 保证跨午夜末段的结束时间不早于 06:00（dayEndMinute 当日
// 坐标 = dayEndMinute-1440）。两处调用：
//   - normalizeDailyPlan 规则 5：LLM 生成的末段睡眠只到凌晨（如 23:29-00:54）；
//   - jitterPlanNodes 之后：扰动可能把夜间结束节点抖到 06:00 前（实测
//     22:00-06:20 被抖成 21:26-05:50）。
//
// 不满足条件（非跨午夜 / 已覆盖到 06:00 及以后）时原样返回。夜间睡眠段
// 在 06:00 前到期会触发 advanceSlotIfNeeded 打断睡眠——而 05:50-06:00 不在
// 06:00-07:00 规划屏蔽窗口内，战术层立即重新分解睡眠，表现为"睡着睡着
// 爬起来说句话再睡"。
func clampNightEnd(items []dailyPlanItem) []dailyPlanItem {
	if len(items) == 0 {
		return items
	}
	last := items[len(items)-1]
	s, e, ok := prompt.SplitPlanRange(last.Time)
	if !ok || e > s || e >= dayEndMinute-1440 {
		return items
	}
	out := make([]dailyPlanItem, len(items))
	copy(out, items)
	out[len(out)-1].Time = prompt.FmtMinute(s) + "-" + prompt.FmtMinute(dayEndMinute-1440)
	return out
}

// jitterPlanNodes 对计划的每个时间节点施加 ±maxJitter 分钟的随机扰动，
// 让各 NPC 的活动开始时间错开（每次生成计划扰动一次，当天内保持稳定）。
//
// 节点模型：相邻时段共享的边界（前段 end == 后段 start）视为同一节点，
// 扰动一次，保证扰动后时段仍然连续无缝、不重叠。
//
// 保证：
//   - 每个节点偏移量 ∈ [-maxJitter, +maxJitter]
//   - 相邻节点保持升序且间隔 ≥ planJitterMinGap（钳位实现），时段不会被压扁
//   - 跨午夜时段（如 "22:00-06:00"）扰动后仍是合法跨午夜时段
//     （跨午夜 end 用 SlotRangeMinute 归一化 +1440 后参与排序）
//
// 下游无需改动：扰动写进计划字符串本身，SlotExpired / matchPlanSlot /
// prompt 展示都会自然使用新的时间点触发战术层分解。
func jitterPlanNodes(items []dailyPlanItem, maxJitter int) []dailyPlanItem {
	if len(items) == 0 || maxJitter <= 0 {
		return items
	}
	// 1. 收集去重节点。SlotRangeMinute 已把跨午夜 end 归一化到 +1440 坐标。
	nodeSet := map[int]bool{}
	for _, it := range items {
		s, e := prompt.SlotRangeMinute(it.Time)
		if s < 0 {
			continue
		}
		nodeSet[s] = true
		nodeSet[e] = true
	}
	nodes := make([]int, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Ints(nodes)
	// 2. 逐节点随机扰动，向下钳位保证升序 + 最小间隔。
	offset := make(map[int]int, len(nodes))
	prev := -1 << 30
	for _, n := range nodes {
		j := n + rand.IntN(2*maxJitter+1) - maxJitter
		if j < prev+planJitterMinGap {
			j = prev + planJitterMinGap
		}
		offset[n] = j
		prev = j
	}
	// 3. 重建 Time 字符串。FmtMinute 对 ≥1440 的值自动 mod 回当天坐标。
	out := make([]dailyPlanItem, len(items))
	for i, it := range items {
		rawS, rawE, ok := prompt.SplitPlanRange(it.Time)
		if !ok {
			out[i] = it
			continue
		}
		lookupE := rawE
		if rawE <= rawS { // 跨午夜：与步骤 1 的归一化坐标对齐
			lookupE = rawE + 1440
		}
		js, okS := offset[rawS]
		je, okE := offset[lookupE]
		if !okS || !okE {
			out[i] = it
			continue
		}
		out[i] = dailyPlanItem{
			Time: prompt.FmtMinute(js) + "-" + prompt.FmtMinute(je),
			Goal: it.Goal,
		}
	}
	return out
}

// jitterPlanString 对格式化计划字符串（兜底计划）施加同样的节点扰动。
// 解析失败时原样返回。
func jitterPlanString(s string) string {
	items := parseFormattedPlan(s)
	if len(items) == 0 {
		return s
	}
	return formatDailyPlan(clampNightEnd(jitterPlanNodes(items, planJitterMinutes)))
}

// formatDailyPlan 把计划格式化为多行字符串。
func formatDailyPlan(items []dailyPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(item.Time)
		sb.WriteString(": ")
		sb.WriteString(item.Goal)
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// selectPlanInjection 决定本轮决策注入的每日计划文本。
//
// 策略：时段边界跨越时注入完整计划（让 LLM 看到全天结构），同一时段
// 内只注入当前时段的目标（节省每轮 ~150-300 字节）。fullPlan 的每行
// 格式为 "HH:MM-HH:MM: goal"，timeOfDay 为 "HH:MM"。
//
// 返回注入文本（含 [今日计划] 或 [当前时段] 头）和当前时段标识
// （"HH:MM-HH:MM"）。无法解析时回退到全量注入。
func selectPlanInjection(fullPlan, timeOfDay, lastSlot string) (string, string) {
	if fullPlan == "" {
		return "", ""
	}
	items := parseFormattedPlan(fullPlan)
	if len(items) == 0 {
		return "[今日计划]\n" + fullPlan, lastSlot
	}
	cur := matchPlanSlot(items, timeOfDay)
	if cur == "" {
		return "[今日计划]\n" + fullPlan, lastSlot
	}
	// 时段未变：只注入当前时段。
	if cur == lastSlot {
		for _, item := range items {
			if item.Time == cur {
				return "[当前时段] " + item.Time + ": " + item.Goal, cur
			}
		}
	}
	// 时段跨越：注入完整计划。
	return "[今日计划]\n" + fullPlan, cur
}

// parseFormattedPlan 解析 formatDailyPlan 产出的 "HH:MM-HH:MM: goal" 多行字符串。
// 无法解析的行跳过；返回空切片表示整体不可用。
func parseFormattedPlan(s string) []dailyPlanItem {
	var items []dailyPlanItem
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.Index(line, ": ")
		if colon < 0 {
			continue
		}
		timePart := line[:colon]
		goalPart := line[colon+2:]
		// timePart 必须是 "HH:MM-HH:MM" 格式
		if _, _, ok := prompt.SplitPlanRange(timePart); !ok {
			continue
		}
		items = append(items, dailyPlanItem{Time: timePart, Goal: goalPart})
	}
	return items
}

// matchPlanSlot 找到包含 timeOfDay 的计划时段（"HH:MM-HH:MM"）。
// timeOfDay 格式 "HH:MM"，返回匹配的 item.Time，无匹配返回 ""。
func matchPlanSlot(items []dailyPlanItem, timeOfDay string) string {
	if timeOfDay == "" {
		return ""
	}
	cur := prompt.ParsePlanMinute(timeOfDay)
	if cur < 0 {
		return ""
	}
	for _, item := range items {
		start, end, ok := prompt.SplitPlanRange(item.Time)
		if !ok {
			continue
		}
		if end <= start {
			// 跨日时段（如 "17:30-06:00"）：
			// - 晚间半段 [start,24:00)：当天晚上命中，正常匹配；
			// - 凌晨半段 [0,end)：仅当 cur 仍早于 dayStartMinute(07:00) 才匹配——
			//   此时生效的是前一天的旧计划，夜间时段确实在进行中。07:00 之后生效的
			//   必为当天新生成的计划（跨日在 06:00-07:00 规划窗口内重生成），其中的
			//   跨午夜时段属于今晚而非昨夜，凌晨半段不得命中——否则清晨会把今晚的
			//   睡觉时段当作当前时段分解下发（首段被扰动推迟时尤甚：如末段
			//   22:21-07:25 在 07:02 命中，NPC 一早又回去睡觉）。
			if cur >= start || (cur < end && cur < dayStartMinute) {
				return item.Time
			}
		} else if cur >= start && cur < end {
			return item.Time
		}
	}
	return ""
}
