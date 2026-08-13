package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
)

// dailyPlanItem 是战略层输出的单条计划。
type dailyPlanItem struct {
	Time string `json:"time"`
	Goal string `json:"goal"`
}

// strategicCaller 是 LLM 客户端的窄接口，便于单测 mock。
type strategicCaller interface {
	SendWithSummary(ctx context.Context, input, summary string) (*llmtypes.Response, error)
	ResetSession()
}

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
func generateDailyPlan(ctx context.Context, sc strategicCaller, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, registry *CapabilityRegistry, logger *slog.Logger, yesterdaySummary string, physical *protocol.PhysicalState) string {
	var actions []protocol.CapabilityAction
	if registry != nil {
		actions = registry.EffectiveActions(agentID)
	}
	if yesterdaySummary == "" {
		yesterdaySummary = yesterdaySummaryForFirstDay
	}
	promptText := fmt.Sprintf(prompt.StrategicPromptTemplate,
		prompt.BuildStrategic(kb, profiles, agentID, actions, physical),
		"昨日总结："+yesterdaySummary)
	logger.Info("[MCP→LLM/STRATEGIC-PROMPT]", "agent_id", agentID, "text", promptText)

	resp, err := sc.SendWithSummary(ctx, promptText, "")
	if err != nil {
		fallback := prompt.DefaultDailyPlan(kb)
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
		fallback := prompt.DefaultDailyPlan(kb)
		logger.Warn("[战略层] 计划解析失败，使用默认计划兜底",
			"agent_id", agentID, "raw", truncateText(raw, 200), "err", err, "fallback", fallback)
		return fallback
	}
	items = normalizeDailyPlan(items)
	if len(items) == 0 {
		logger.Warn("[战略层] 计划校验后为空，使用默认计划兜底", "agent_id", agentID)
		return prompt.DefaultDailyPlan(kb)
	}
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
//  3. 首段前伸到 07:00（dayStartMinute；06:00-07:00 是规划时间，不覆盖）
//  4. 填补中间空白：前段 end < 后段 start 时延长前段
//  5. 末段后延到次日 06:00（若 LLM 只规划到 18:00，18:00-22:00 会触发 idle wait 瘫痪）
//
// 支持跨午夜 slot（如 "22:00-06:00"）：跨午夜时段时长按 end+1440-start 计算，
// 末段若已跨午夜则不后延。
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
	// 跨午夜末段（end <= start）已覆盖到次日，不后延。
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
	return valid
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
			// 跨日时段（如 "17:30-06:00"）：cur 在 [start,24:00) 或 [0,end) 内都算匹配。
			if cur >= start || cur < end {
				return item.Time
			}
		} else if cur >= start && cur < end {
			return item.Time
		}
	}
	return ""
}

