// Package prompt — strategic layer prompt builder.
package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// defaultDailyPlan is the fallback plan when kb == nil.
// Kept neutral (no KB-specific terms) so it adapts when KB changes.
const defaultDailyPlan = "07:00-12:00: 上午主要工作\n" +
	"12:00-14:00: 午间停工与短暂休息\n" +
	"14:00-18:00: 下午继续工作\n" +
	"18:00-22:00: 前往中央广场休息\n" +
	"22:00-07:00: 夜间休眠"

// DefaultDailyPlan derives a fallback daily plan from KB.
// kb == nil → returns defaultDailyPlan (neutral, no KB-specific terms).
// With KB: uses first zone display name as work location, first object display
// name as work content for morning/afternoon slots. Avoids hardcoding
// "车间"/"装配" so the fallback adapts to any KB.
func DefaultDailyPlan(kb *worldkb.KB) string {
	if kb == nil {
		return defaultDailyPlan
	}
	zoneName := "主要区域"
	if zs := kb.ListZones(); len(zs) > 0 {
		if zs[0].DisplayName != "" {
			zoneName = zs[0].DisplayName
		} else {
			zoneName = zs[0].ID
		}
	}
	workName := "工作"
	if os := kb.ListObjects(); len(os) > 0 {
		if os[0].DisplayName != "" {
			workName = os[0].DisplayName
		} else {
			workName = os[0].ID
		}
	}
	return fmt.Sprintf("07:00-12:00: 上午在%s进行%s作业\n", zoneName, workName) +
		"12:00-13:00: 午间停工与短暂休息\n" +
		fmt.Sprintf("13:00-18:00: 下午继续%s作业\n", workName) +
		"18:00-22:00: 保养休息\n" +
		"22:00-06:00: 夜间休眠"
}

// StrategicPromptTemplate is the strategic layer prompt template.
// Placeholders: %s = strategic context (role+KB+capabilities), %s = yesterday summary.
const StrategicPromptTemplate = `[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了，你刚从休眠舱醒来，当前位于休眠舱区域。

%s

%s

请基于你的角色身份和性格，规划今天一天的活动安排。一天从 07:00 到次日 07:00，你从 07:00 开始活动，夜间活动可持续到次日清晨。

要求：
1. 输出一个 JSON 数组，6-8 条
2. 每条包含 "time"（时段，如 "07:00-12:00"）和 "goal"（这个时段你要做什么，一句话）
3. 安排要符合你的角色身份和性格特点
4. 每个时段时长不少于 120 分钟（起止时间差 ≥ 120 分钟），每个时段原则上仅安排一项主要任务
5. 只输出 JSON 数组，不要任何其他文字
6. 必须以字符 [ 开头，以字符 ] 结尾，不要输出设计思路、不要解释、不要 markdown 围栏
7. goal 中提到的地点、人物、设备必须是【你的角色】和【世界知识】中存在的，不得编造未提及的人物或设施
8. 末段若跨午夜（如 "22:00-07:00"），结束时间表示次日该时刻，调度器会自动识别跨午夜时段
9. goal 的主要活动应能用【可用能力】中列出的复合动作实现（如装配→work_shift、充电→charge_at_station），不得规划【可用能力】未列出且无法用基础动作（移动/说话/表达情绪/交互物体/等待）组合实现的活动——如"整理仪容""冥想"等无对应能力的活动会被战术层拒绝。
10. 首个时段（从 07:00 起）必须是日间活动（如晨间补电、装配、维护），不得安排休眠——你刚从休眠舱醒来，应立即离开开始当日活动；休眠只能安排在午间和夜间。

示例：[{"time":"07:00-09:00","goal":"去中央广场晨间补电"},{"time":"09:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-14:00","goal":"午间停工，前往充电区域短暂补电休息"},{"time":"14:00-18:00","goal":"下午继续在车间装配"},{"time":"18:00-22:00","goal":"傍晚去维修台维护修理"},{"time":"22:00-07:00","goal":"夜间在休眠舱休息"}]`

// BuildStrategic constructs the strategic layer prompt's KB context segment,
// containing five parts:
//   - 【你的角色】: from AgentRole(kb, profiles, agentID)
//   - 【物理状态】: from PhysicalLine(physical); nil → default fresh state
//   - 【世界知识】: from KBContext(kb) (shared with tactical layer)
//   - 【区域设施映射】: zone→object mapping (currently disabled — see comment)
//   - 【可用能力】: composite actions from capabilities
//
// kb == nil → skips 【世界知识】 segment but still injects persona + capabilities.
// actions == nil → falls back to builtin 6 composite tools (same as tactical).
// profiles == nil → AgentRole falls back to hardcoded fallback (KB persona ignored).
// physical == nil → PhysicalLine falls back to default fresh state (100/0/0/100).
func BuildStrategic(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string, actions []protocol.CapabilityAction, physical *protocol.PhysicalState) string {
	var sb strings.Builder
	// 【你的角色】段仅依赖 profile + fallback，与 KB 可用性解耦。
	if role := AgentRole(kb, profiles, agentID); role != "" {
		sb.WriteString("【你的角色】\n")
		sb.WriteString(role)
	}
	if line := PhysicalLine(physical); line != "" {
		sb.WriteString("【物理状态】\n")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if kb != nil {
		if kbCtx := KBContext(kb); kbCtx != "" {
			sb.WriteString("【世界知识】\n")
			sb.WriteString(kbCtx)
		}
		// 【区域设施映射】段暂时移除——【世界知识】已逐 object 列出所在 zone
		// 与可用 interaction，信息冗余；移除后 prompt 从 ~2000 字降到 ~1400 字，
		// 降低战略层 LLM 输入 token 数以缩短延迟。日后若 LLM 又出现 zone-object
		// 错配可重新启用。
	}
	if cap := StrategicCapabilitySummary(actions); cap != "" {
		sb.WriteString("【可用能力】\n")
		sb.WriteString("长时段活动用以下复合动作（自动移动到对应位置，覆盖整段工作时间）：\n")
		sb.WriteString(cap)
		sb.WriteString("此外始终可用基础动作：移动、说话、表达情绪、与物体交互、等待（用于短耗时或衔接）。\n")
	}
	return sb.String()
}

// StrategicCapabilitySummary constructs the 【可用能力】 segment's composite
// action bullet list. Reuses toolEntries derivation (same source, ensuring
// strategic/tactical capability views match), keeping only Kind=="composite"
// entries formatted as "- 描述（tool_name）".
// actions == nil → falls back to builtin tools.
func StrategicCapabilitySummary(actions []protocol.CapabilityAction) string {
	entries := ToolEntries(actions)
	var sb strings.Builder
	for _, e := range entries {
		if e.Kind != "composite" {
			continue
		}
		fmt.Fprintf(&sb, "- %s（%s）\n", e.Desc, e.Name)
	}
	return sb.String()
}
