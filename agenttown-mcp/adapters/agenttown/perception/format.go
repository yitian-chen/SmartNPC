// Package perception converts a protocol perception_update payload into the
// natural-language text that MCP pushes to Hermes Gateway.
//
// It also folds in the latest physical state (from state_report) and any
// pending action-completion lines, so the LLM sees a complete first-person
// picture without those arriving on separate channels.
package perception

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// Format converts a perception_update payload (JSON) into the NL text
// Hermes receives. `physical` is the latest known physical state (from
// state_report, may be nil). `extras` are additional lines to append
// (e.g. action-completion notices). Returns "" on parse failure.
func Format(payload json.RawMessage, physical *protocol.PhysicalState, extras []string) string {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return FormatPayload(&p, physical, extras)
}

// FormatPayload is the typed entry point. Exposed for tests.
func FormatPayload(p *protocol.PerceptionPayload, physical *protocol.PhysicalState, extras []string) string {
	if p == nil {
		return ""
	}

	timeOfDay := p.Environment.TimeOfDay
	hour := parseHour(timeOfDay)
	period := periodOf(hour)

	lines := make([]string, 0, 12)

	// Location line.
	zone := "未知区域"
	if p.Location.CurrentZone != nil && *p.Location.CurrentZone != "" {
		zone = *p.Location.CurrentZone
	}
	lines = append(lines, fmt.Sprintf("[感知] %s，时间%s。", period, timeOfDay))
	lines = append(lines, fmt.Sprintf("你在%s。", zone))

	// Physical state (from state_report authoritative channel).
	if physical != nil {
		lines = append(lines, fmt.Sprintf("电池%.0f%%，疲劳%.0f，关节磨损%.0f，健康%.0f。",
			physical.Energy, physical.Fatigue, physical.JointWear, physical.Health))
	} else if len(p.PhysicalStateDelta) > 0 {
		// Fall back to any delta carried in this perception.
		parts := make([]string, 0, len(p.PhysicalStateDelta))
		for k, v := range p.PhysicalStateDelta {
			parts = append(parts, fmt.Sprintf("%s=%.0f", k, v))
		}
		lines = append(lines, "状态变化: "+strings.Join(parts, ", ")+"。")
	}

	// Nearby objects.
	if len(p.NearbyObjects) > 0 {
		names := make([]string, 0, len(p.NearbyObjects))
		for _, o := range p.NearbyObjects {
			label := o.Name
			if label == "" {
				label = o.ID
			}
			if len(o.AvailableActions) > 0 {
				label += "(" + strings.Join(o.AvailableActions, "/") + ")"
			}
			names = append(names, label)
		}
		lines = append(lines, "附近可用: "+strings.Join(names, ", ")+"。")
	}

	// Visible agents.
	if len(p.VisibleAgents) > 0 {
		names := make([]string, 0, len(p.VisibleAgents))
		for _, a := range p.VisibleAgents {
			names = append(names, fmt.Sprintf("%s(%s)", a.Name, a.CurrentAction))
		}
		lines = append(lines, "你看到: "+strings.Join(names, ", ")+"。")
	}

	// Audible events.
	for _, e := range p.AudibleEvents {
		lines = append(lines, fmt.Sprintf("你听到 %s：\"%s\"", e.Source, e.Content))
	}

	// Time-based hints.
	if hour == 12 {
		lines = append(lines, "午间维护时间到了，可以去充电。")
	} else if hour == 17 {
		lines = append(lines, "工作快结束了，准备收尾。")
	}

	// Action completions / extras.
	for _, ex := range extras {
		if ex != "" {
			lines = append(lines, "["+ex+"]")
		}
	}

	lines = append(lines, "你现在想做什么？")

	// Tool-use directive.
	lines = append(lines, "")
	lines = append(lines, "【重要】执行任何动作时必须调用对应的 MCP 工具（如 mcp__agenttown__move_to、mcp__agenttown__work_assemble、mcp__agenttown__speak、mcp__agenttown__charge_at 等），第一个参数为你的 agent_id。不要只用文字叙述。回复要简短，不要重复描述已知环境。")

	return strings.Join(lines, "\n")
}

// parseHour extracts the hour (0-23) from a "HH:MM" time-of-day string.
// Returns -1 if unparseable.
func parseHour(timeOfDay string) int {
	parts := strings.SplitN(timeOfDay, ":", 2)
	if len(parts) == 0 {
		return -1
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return -1
	}
	return h
}

// periodOf maps a 0-23 hour to a Chinese time-of-day label.
func periodOf(hour int) string {
	switch {
	case hour < 0:
		return "某时"
	case hour < 6:
		return "深夜"
	case hour == 6:
		return "清晨"
	case hour < 12:
		return "上午"
	case hour < 13:
		return "中午"
	case hour < 18:
		return "下午"
	default:
		return "傍晚"
	}
}
