package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
)

// dailyPlanItem 是战略层输出的单条计划。
type dailyPlanItem struct {
	Time string `json:"time"`
	Goal string `json:"goal"`
}

// strategicCaller 是 hermes.Client 的窄接口，便于单测 mock。
type strategicCaller interface {
	SendWithSummary(ctx context.Context, input, summary string) (*hermes.Response, error)
	ResetSession()
}

const strategicPromptTemplate = `[战略层/每日规划] 现在是仿真时间 06:00，新的一天开始了。

%s

请基于你的角色身份和性格，规划今天一天的活动安排。

要求：
1. 输出一个 JSON 数组，6-10 条
2. 每条包含 "time"（时段，如 "07:00-12:00"）和 "goal"（这个时段你要做什么，一句话）
3. 安排要符合你的角色身份和性格特点
4. 每个时段时长不少于 60 分钟（起止时间差 ≥ 60 分钟）。短活动（如午休 30 分钟、短暂维修）合并到相邻时段，不要单独成段——调度器按整点采样，短于 60 分钟的时段会被跳过
5. 只输出 JSON 数组，不要任何其他文字
6. 必须以字符 [ 开头，以字符 ] 结尾，不要输出设计思路、不要解释、不要 markdown 围栏
7. goal 中提到的地点、人物、设备必须是你的角色设定和当前世界知识中存在的，不得编造未提及的人物或设施

示例：[{"time":"06:00-07:00","goal":"起床晨检，慢速活动关节"},{"time":"07:00-12:00","goal":"上午车间装配作业，盯紧关键工序"},{"time":"12:00-13:00","goal":"午间停工，检查公差记录并短暂补电休息"}]`

// defaultDailyPlan 是战略层解析失败时的兜底计划。
// 不返回空字符串是为了避免整天 Wait(60s) 瘫痪——兜底计划虽然无个性，
// 但能驱动战术层正常工作，让仿真继续运行而非停滞。
// 时段覆盖 06-22，每段 ≥60min，符合调度器采样约束。
const defaultDailyPlan = "06:00-12:00: 上午车间装配作业\n" +
	"12:00-13:00: 午间停工检修与短暂补电\n" +
	"13:00-18:00: 下午继续装配作业\n" +
	"18:00-22:00: 充电保养与写工作日志"

// yesterdaySummaryForFirstDay 是首日启动时注入的"昨日总结"。
//
// 早期版本写死了"小柯/充电站"等具体人物和设施，但当 KB 不包含这些
// 元素时（如最小化测试 KB 或换地图运行），LLM 会被诱导在战略计划里
// 编造这些 KB 外概念。改为中性表述：只描述抽象活动模式（装配/休息/
// 充电），不点名任何人物或具体设施，由 LLM 根据 KB 自行具象化。
const yesterdaySummaryForFirstDay = "昨天按计划完成了车间装配和设备巡检，下午体力下降明显，晚上进入低功耗休息状态，关节略有磨损"

// generateDailyPlan 调 LLM 生成当日计划，返回格式化字符串（每行 "时段: 目标"）。
// 任一步失败均回退到 defaultDailyPlan，保证战术层有目标可分解、
// 仿真不瘫痪。返回 "" 仅表示连兜底计划都没用上（理论上不会发生）。
func generateDailyPlan(ctx context.Context, sc strategicCaller, agentID string, logger *slog.Logger) string {
	prompt := fmt.Sprintf(strategicPromptTemplate, "昨日总结："+yesterdaySummaryForFirstDay)
	logger.Info("[MCP→Hermes/STRATEGIC-PROMPT]", "agent_id", agentID, "text", prompt)

	resp, err := sc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		logger.Warn("[战略层] 计划生成失败，使用默认计划兜底",
			"agent_id", agentID, "err", err, "fallback", defaultDailyPlan)
		return defaultDailyPlan
	}
	sc.ResetSession() // 战略调用一次性使用，立即清链

	raw := resp.ExtractText()
	logger.Info("[Hermes→MCP/STRATEGIC-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	items, err := parseDailyPlan(raw)
	if err != nil {
		logger.Warn("[战略层] 计划解析失败，使用默认计划兜底",
			"agent_id", agentID, "raw", truncateText(raw, 200), "err", err, "fallback", defaultDailyPlan)
		return defaultDailyPlan
	}
	if len(items) == 0 {
		logger.Warn("[战略层] 计划解析为空数组，使用默认计划兜底", "agent_id", agentID)
		return defaultDailyPlan
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
//（"HH:MM-HH:MM"）。无法解析时回退到全量注入。
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
		if _, _, ok := splitPlanRange(timePart); !ok {
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
	cur := parsePlanMinute(timeOfDay)
	if cur < 0 {
		return ""
	}
	for _, item := range items {
		start, end, ok := splitPlanRange(item.Time)
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

// splitPlanRange 把 "HH:MM-HH:MM" 拆成起止分钟数（从午夜起）。
func splitPlanRange(s string) (start, end int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start = parsePlanMinute(parts[0])
	end = parsePlanMinute(parts[1])
	if start < 0 || end < 0 {
		return 0, 0, false
	}
	return start, end, true
}

// parsePlanMinute 把 "HH:MM" 转成从午夜起的分钟数，失败返回 -1。
func parsePlanMinute(s string) int {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return -1
	}
	h := atoi(parts[0])
	m := atoi(parts[1])
	if h < 0 || m < 0 {
		return -1
	}
	return h*60 + m
}

// atoi 解析非负整数，失败返回 -1。
func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
