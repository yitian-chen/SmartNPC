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

昨日总结：%s

请基于你的角色身份和性格，规划今天一天的活动安排。

要求：
1. 输出一个 JSON 数组，6-10 条
2. 每条包含 "time"（时段，如 "07:00-12:00"）和 "goal"（这个时段你要做什么，一句话）
3. 安排要符合你的角色身份和性格特点
4. 只输出 JSON 数组，不要任何其他文字

示例：[{"time":"07:00-08:00","goal":"晨检车间设备"},{"time":"08:00-12:00","goal":"装配作业，盯紧小柯"}]`

const hardcodedYesterdaySummary = "昨天在车间装配8小时，指导小柯分类零件，晚上充电站和铁牛聊了会儿，关节有点酸"

// generateDailyPlan 调 LLM 生成当日计划，返回格式化字符串（每行 "时段: 目标"）。
// 任一步失败均返回 ""，不阻塞仿真。
func generateDailyPlan(ctx context.Context, sc strategicCaller, agentID string, logger *slog.Logger) string {
	prompt := fmt.Sprintf(strategicPromptTemplate, hardcodedYesterdaySummary)
	logger.Info("[战略层] 开始生成每日计划", "agent_id", agentID)

	resp, err := sc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		logger.Warn("[战略层] 计划生成失败，使用空计划继续", "agent_id", agentID, "err", err)
		return ""
	}
	sc.ResetSession() // 战略调用一次性使用，立即清链

	raw := resp.ExtractText()
	items, err := parseDailyPlan(raw)
	if err != nil {
		logger.Warn("[战略层] 计划解析失败", "agent_id", agentID, "raw", truncateText(raw, 200), "err", err)
		return ""
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
	// 提取首个 [..]
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array found")
	}
	var items []dailyPlanItem
	if err := json.Unmarshal([]byte(s[start:end+1]), &items); err != nil {
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
