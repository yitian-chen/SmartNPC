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

// StrategicSystemPrompt is the strategic layer's system message: mechanism
// text only (role positioning, world mechanics, planning rules, output
// format, example). It is fully static across agents and calls, so the LLM
// gateway can cache it. Per-call context/data (role, KB, physical state,
// capabilities, yesterday summary) goes in the user message built from
// StrategicUserTemplate + BuildStrategic.
//
// Rules referencing dynamic segments say "用户信息中【…】" because those
// segments live in the user message, not here.
const StrategicSystemPrompt = `你是小镇居民 NPC 的战略规划模块。每天清晨 07:00，你根据用户信息中提供的角色身份、今日日程、物理状态、世界知识与可用能力，规划当天 07:00 到次日 07:00 的活动安排。

【动作对状态的影响】
- work_shift（装配/分拣等）：消耗能量、积累疲劳与少量关节磨损，赚取余额
- charge_at_station（充电）：恢复能量、缓解疲劳，消耗余额
- self_maintenance（维护）：缓解关节磨损，大量消耗余额
- rest_at_residence（休息）：缓解疲劳
- surf_internet（上网）：少量消耗能量与余额、缓解疲劳
规划时请综合权衡：产出性活动（工作）赚取余额但消耗体力、缓慢积攒关节磨损；恢复性活动（充电/维护/休息）花余额但延续工作能力。避免长时间连续工作导致体力耗尽，也避免频繁恢复导致余额入不敷出。

要求：
1. 输出 JSON 数组（6-8 条），每条含 "time"（"HH:MM-HH:MM"）和 "goal"（一句话），以 [ 开头 ] 结尾，不要其他文字
2. 每个时段 ≥120 分钟，仅安排一项主要任务；连续两个任务相同的时段合并为一个长时段（如 "07:00-12:00: 车间装配作业"）
3. 规划每个时段时，先想清楚这个时段的活动要用用户信息中【可用能力】里哪个 cmd 实现：
   - 有对应 cmd 的活动（如装配→work_shift、充电→charge_at_station）→ 可以安排
   - 没有对应 cmd 的活动（如"准备工具""巡查""整理仪容"）→ 不要安排，改用有 cmd 对应的活动
   - 判断标准：goal 能否直接映射到【可用能力】中列出的某个 cmd？能 → 可以安排；不能 → 换一个
4. goal 中提到的地点、人物、设备必须是用户信息中【你的角色】和【世界知识】里存在的，不得编造未提及的人物或设施
5. 首段（07:00 起）必须是日间活动，不得安排休眠；末段跨午夜时结束时间表示次日时刻
6. 充电仅在能量为"低电量"或疲劳为"非常疲劳"时安排；维护仅在关节磨损达到"明显磨损"及以上时安排；能量充足时优先产出性活动
7. 综合用户信息中【物理状态】的四项状态调整安排侧重点：能量偏低→多充电少工作；疲劳偏高→提前休眠；磨损偏高→安排维护；余额低→多工作少花钱

示例：[{"time":"07:00-09:00","goal":"早晨上网浏览新闻（surf_internet）"},{"time":"09:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-14:00","goal":"午间去长椅上坐坐"},{"time":"14:00-18:00","goal":"下午继续在车间装配"},{"time":"18:00-22:00","goal":"傍晚去充电站补电"},{"time":"22:00-07:00","goal":"夜间在休眠舱休息"}]`

// StrategicUserTemplate is the strategic layer's user message template.
// Placeholders: %s = strategic context (BuildStrategic output: role +
// schedule + physical + KB + peers + capabilities), %s = yesterday summary.
// The instruction line stays in the user message so the "plan today" ask
// sits immediately after the data it refers to.
const StrategicUserTemplate = `[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了，你刚从休眠舱醒来，当前位于休眠舱区域。

%s

%s

请基于你的角色身份和性格，规划今天一天的活动安排。一天从 07:00 到次日 07:00，你从 07:00 开始活动，夜间活动可持续到次日清晨。`

// BuildStrategic constructs the strategic layer user message's context
// segment, containing five parts:
//   - 【你的角色】: from AgentRole(kb, profiles, agentID)
//   - 【今日日程】: from dayContext (pre-formatted by weeklyschedule.WeeklyLine;
//     "" skips the segment — disabled or dayCount<0)
//   - 【物理状态】: from PhysicalLine(physical); nil → default fresh state
//   - 【世界知识】: from KBContext(kb) (shared with tactical layer)
//   - 【可用能力】: composite actions from capabilities
//
// Mechanism text (rules, 【动作对状态的影响】, output format, example) lives
// in StrategicSystemPrompt, not here. The 【其他NPC】 roster segment is
// temporarily removed — the strategic layer no longer arranges social slots;
// restore it when social planning comes back.
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
	if line := PhysicalLine(physical, BandThresholdsFor(profiles, agentID)); line != "" {
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
		// 【其他NPC】段暂时移除——战略层暂不安排社交（social_chat）时段，
		// 花名册仅为社交目标服务；战术层的【附近NPC】仍用 OtherAgentsLine。
		// 日后战略层恢复社交安排时，把下面的段加回来即可：
		// if peers := OtherAgentsLine(kb, agentID); peers != "" {
		// 	sb.WriteString("【其他NPC】\n")
		// 	sb.WriteString(peers)
		// 	sb.WriteString("\n")
		// }
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
