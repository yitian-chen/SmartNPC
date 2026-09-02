// Package prompt — strategic layer prompt builder.
package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
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

// StrategicRules is the seven planning rules, injected into the user message
// (StrategicUserTemplate's third placeholder) so they sit adjacent to the
// planning ask (recency effect: instructions closer to the ask are followed
// more reliably). References to 【世界背景】/【人物背景】/【世界详细信息】
// point at the system message's modules; references to 【物理状态】 point at
// the user message's dynamic segments.
const StrategicRules = `1. 【硬性要求】每个时段的结束时间减去开始时间必须 ≥30 分钟（不足 30 分钟的活动要么并入相邻时段，要么不安排）；每段安排 1 - 2 项任务，连续两个时段不得任务完全相同
2. 规划每个时段时，先想清楚这个时段的活动用什么实现：goal 应能映射到【世界详细信息】设施详情中列出的某个 (semantic_group, interaction) 组合——不限于工种设备，睡眠舱的 sleep/meditate/tidy_up、长椅的 rest 都是合法活动，战术层会据此分解为对应的移动与长时段互动；映射不上的抽象活动（如"准备工具""巡查"）→ 换一个。锻炼类活动（晨练拉伸等原地动作）不需要设施，属例外；聊天/社交/对话类活动用 social_chat 实现（目标是【其他NPC】名单里的某位 NPC，不是设施），也属例外
3. goal 中提到的地点、人物、设备必须是系统信息中【人物背景】和【世界详细信息】、或用户信息中【其他NPC】里存在的，不得编造未提及的人物或设施
4. 第一个时段必须从 07:00 开始，且任何时段的开始时间不得早于 07:00——禁止输出 0:00-7:00 这类凌晨睡觉时段（凌晨睡眠已由前一晚的跨午夜末段覆盖，不要重复安排）。
5. 首段禁止安排工作——早间可以安排晨练拉伸、上网、长椅放松、冥想醒神、整理舱位等非工作活动。午间可以选择锻炼、就近长椅小憩、休眠舱午睡等非产出性活动。夜间睡眠必须是一个连续的跨午夜时段：约 22:00 前后开始、次日 06:00-07:00 结束；不得拆成多个睡眠时段（禁止 20:30-22:58 睡觉 + 22:58-07:16 睡觉这样的连续两段），也不得在凌晨提前结束（禁止 23:00-01:00 这样的短睡眠段）。末段跨午夜时结束时间表示次日时刻
6. 充电仅在规划时电量为"低"或"较低"时安排，规划时电量为"高"或"中"时严禁规划充电；维护仅在关节磨损达到"明显磨损"及以上时安排；睡眠只能在午间和晚上
7. 综合用户信息中【物理状态】的四项状态调整安排侧重点：电量偏低→多充电少工作；疲劳偏高→提前休眠；磨损偏高→安排维护；余额低→多工作少花钱
8. 整理内务、冥想等动作安排的时间不得超过一小时

格式示例：[{"time":"07:00-09:00","goal":"晨练拉伸"},{"time":"09:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-12:40","goal":"找老王聊聊天（social_chat）"},{"time":"12:40-18:00","goal":"下午继续装配作业"},{"time":"18:00-22:00","goal":"去中央广场长椅休息"},{"time":"22:00-07:00","goal":"夜间在睡眠舱休眠"}]`

// StrategicUserTemplate is the strategic layer's user message template.
// Placeholders: %s = dynamic context (BuildStrategicUserContext output:
// today's weekly-schedule context + physical state), %s = yesterday summary,
// %s = planning rules (StrategicRules). The instruction line stays in the
// user message so the "plan today" ask sits immediately after the data and
// rules it refers to.
const StrategicUserTemplate = `[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了，你刚从休眠舱醒来，当前位于休眠舱区域。

%s

%s

规划要求：
%s

请基于你的角色身份和性格，规划今天一天的活动安排（人设只影响选什么活动、怎么安排时段；goal 文字一律干练简洁，不带人设语气）。一天从 07:00 到次日 07:00，你从 07:00 开始活动。
只输出 JSON 数组，每条形如 {"time":"HH:MM-HH:MM","goal":"纯文本一句话"}，"goal" 必须是字符串；第一个时段从 07:00 开始，任何时段的开始时间不得早于 07:00；分出6 - 8个时段，每个时段必须 ≥30 分钟；不要输出任何其他文字。`

// BuildStrategicUserContext constructs the strategic layer user message's
// dynamic context segment: 【今日日程】 (weekly schedule context, skipped
// when empty) + 【物理状态】 (nil physical → default fresh state) +
// 【其他NPC】 (KB peer roster — the social_chat target list, skipped when no
// peers). The strategic preamble (module role) also lives here: the system
// prompt is shared verbatim across layers, so every strategic-specific text
// belongs in the user message.
func BuildStrategicUserContext(agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, physical *protocol.PhysicalState, dayContext string) string {
	var sb strings.Builder
	// 【今日日程】段：每周日程上下文（星期几 + 工作日/休息日 + 当日提示）。
	// dayContext 由调用方通过 weeklyschedule.WeeklyLine(dayCount, sched) 预格式化，
	// pkg/prompt 不依赖 weeklyschedule 包（解耦）。空串=禁用或 dayCount<0，跳过。
	sb.WriteString(`你是小镇居民 NPC 的战略规划模块。每天清晨 07:00，你根据系统信息中的【世界背景】【人物背景】【世界详细信息】，以及用户信息中的今日日程、物理状态、其他NPC、昨日总结与规划要求，规划当天 07:00 到次日 07:00 的活动安排。

各活动对属性的每小时影响幅度见系统信息【世界详细信息】各设施的属性变动说明（由 world KB 声明生成）。规划时请综合权衡：产出性活动（工作）赚取余额但消耗体力、缓慢积攒关节磨损；恢复性活动（充电/维护/休息）花余额但延续工作能力。避免长时间连续工作导致体力耗尽，也避免频繁恢复导致余额入不敷出。

`)

	if dayContext != "" {
		sb.WriteString("【今日日程】\n")
		sb.WriteString(dayContext)
		sb.WriteString("\n")
	}
	if line := PhysicalLine(physical, BandThresholdsFor(profiles, agentID)); line != "" {
		sb.WriteString("【物理状态】\n")
		// PhysicalLine 自带"物理状态："前缀，段头已去重。
		sb.WriteString(strings.TrimPrefix(line, "物理状态："))
		sb.WriteString("\n")
	}
	// 【其他NPC】段：列出 KB 中除自己外的所有 NPC（id + 职业），让战略层 LLM
	// 在 07:00 规划时看到可聊天的同伴——social_chat 的 target_agent_id 需要
	// 具体 id，没有这份花名册 LLM 会因"不得编造未提及的人物"规则而不安排
	// 社交时段。与战术层的【附近NPC】不同：战术层用 UE 运行时感知，战略层用
	// KB 静态花名册（任何 NPC id 都合法目标）。段头点明"聊天是合法活动"，
	// 提供正向引导（规则 9 删除后这是战略层唯一的社交触发点）。
	if peers := OtherAgentsLine(kb, agentID); peers != "" {
		sb.WriteString("【其他NPC】（你可以主动找其中某位聊天 social_chat，维系人际关系）\n")
		sb.WriteString(peers)
		sb.WriteString("\n")
	}
	return sb.String()
}
