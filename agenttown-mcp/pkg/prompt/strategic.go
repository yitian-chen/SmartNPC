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
10. 首个时段（从 07:00 起）必须是日间活动（如晨间巡视、装配、维护），不得安排休眠——你刚从休眠舱醒来，应立即离开开始当日活动；休眠只能安排在午间和夜间。
11. 充电（charge_at_station）仅在【物理状态】显示能量偏低（<40）或疲劳较高（>80）时安排；能量充足（如刚从休眠醒来、能量接近 100）时不得安排充电时段，应优先安排工作/巡视/维护等产出性活动。
12. 维护（self_maintenance）仅在【物理状态】显示关节磨损较高（>50）时安排；磨损较低（<30）时不得安排维护时段，应优先安排工作/巡视/上网等产出或低成本活动以积累余额。维护是周期性大修（类比人去医院，不是每天都要去），通常需要工作多日积累的磨损才值得一次维护——频繁维护会使余额入不敷出。

示例：[{"time":"07:00-09:00","goal":"早晨去上网休闲放松"},{"time":"09:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-14:00","goal":"午间停工短暂休息"},{"time":"14:00-18:00","goal":"下午继续在车间装配"},{"time":"18:00-22:00","goal":"傍晚去充电站补电"},{"time":"22:00-07:00","goal":"夜间在休眠舱休息"}]`

// BuildStrategic constructs the strategic layer prompt's KB context segment,
// containing six parts:
//   - 【你的角色】: from AgentRole(kb, profiles, agentID)
//   - 【今日日程】: from dayContext (pre-formatted by weeklyschedule.WeeklyLine;
//     "" skips the segment — disabled or dayCount<0)
//   - 【物理状态】: from PhysicalLine(physical); nil → default fresh state
//   - 【世界知识】: from KBContext(kb) (shared with tactical layer)
//   - 【区域设施映射】: zone→object mapping (currently disabled — see comment)
//   - 【可用能力】: composite actions from capabilities
//
// kb == nil → skips 【世界知识】 segment but still injects persona + capabilities.
// actions == nil → falls back to builtin 6 composite tools (same as tactical).
// profiles == nil → AgentRole falls back to hardcoded fallback (KB persona ignored).
// physical == nil → PhysicalLine falls back to default fresh state (100/0/0/100).
// dayContext == "" → skips 【今日日程】 segment (weekly schedule disabled or
// dayCount < 0 before first perception).
func BuildStrategic(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string, actions []protocol.CapabilityAction, physical *protocol.PhysicalState, dayContext string) string {
	var sb strings.Builder
	// 【你的角色】段仅依赖 profile + fallback，与 KB 可用性解耦。
	if role := AgentRole(kb, profiles, agentID); role != "" {
		sb.WriteString("【你的角色】\n")
		sb.WriteString(role)
	}
	// 【今日日程】段：每周日程上下文（星期几 + 工作日/休息日 + 当日提示）。
	// dayContext 由调用方通过 weeklyschedule.WeeklyLine(dayCount, sched) 预格式化，
	// pkg/prompt 不依赖 weeklyschedule 包（解耦）。空串=禁用或 dayCount<0，跳过。
	if dayContext != "" {
		sb.WriteString("【今日日程】\n")
		sb.WriteString(dayContext)
		sb.WriteString("\n")
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
	sb.WriteString("【动作对状态的影响】\n")
	sb.WriteString("- work_shift（装配/分拣等）：消耗能量、积累疲劳与少量关节磨损，赚取余额\n")
	sb.WriteString("- charge_at_station（充电）：恢复能量、缓解疲劳，消耗余额\n")
	sb.WriteString("- self_maintenance（维护）：缓解关节磨损，大量消耗余额\n")
	sb.WriteString("- rest_at_residence（休息）：缓解疲劳\n")
	sb.WriteString("- surf_internet（上网）：少量消耗能量与余额、缓解疲劳\n")
	sb.WriteString("规划时请综合权衡：产出性活动（工作）赚取余额但消耗体力、缓慢积攒关节磨损；恢复性活动（充电/维护/休息）花余额但延续工作能力。避免长时间连续工作导致体力耗尽，也避免频繁恢复导致余额入不敷出。\n")
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
