// Package prompt — strategic layer prompt builder.
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// ProductionWorkflowText summarizes the town's production workflow (from
// docs/AgentTown_Workflow.md) as a short natural-language paragraph. Injected
// into both the strategic and tactical system prompts so both layers share
// the same mental model of the production loop and job trade-offs.
const ProductionWorkflowText = "小镇生产是一条概念流水线：物流转运站加工原料、分拣货物，主生产车间装配半成品、调试设备、质检成品，废料回收与再制造场拆解报废设备回收零件（当前阶段无物料/库存依赖，各工序彼此独立）。六类工种强度与收入成正比：质检轻松低薪；装配、加工、分拣强度与收入中等；调试、拆解高薪但更耗电量、更积磨损。机器人靠电量、疲劳、关节磨损、余额四项状态维持“打工循环”：工作挣余额，同时耗电量、涨疲劳、积磨损，又需花钱充电、休息、维修才能延续工作。规划时应按自身状态取舍：累了或电量低选轻松工种，缺钱时选高强度工种。"

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

// BuildStrategicSystemPrompt constructs the strategic layer's system message,
// three KB-derived modules (stable within a session):
//  1. 【世界背景】 — world overview registered from the world KB: narrative
//     setting/theme, zone roster, smart-object group roster, NPC roster.
//  2. 【人物背景】 — the current agent's profile (AgentRole).
//  3. 【世界详细信息】 — per-zone descriptions; smart objects grouped by
//     semantic_group with per-interaction description, per-hour attribute
//     effects and usage gates (from the KB's declared rates).
//
// 复合动作清单不注入战略层——goal 只需映射到设施详情中的
// (semantic_group, interaction) 组合，cmd 的选择属于战术层职责。
//
// The seven planning rules (StrategicRules) live in the user message
// (StrategicUserTemplate's third placeholder) so they sit adjacent to the
// planning ask. Per-call dynamic data (physical state, weekly-schedule
// context, yesterday summary) also lives in the user message
// (BuildStrategicUserContext). The system prompt text is identical across
// calls within a session (per agent), keeping it cacheable.
//
// kb == nil → modules 1/3 degrade to empty; profiles == nil → persona falls
// back to the hardcoded fallback fields.
func BuildStrategicSystemPrompt(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	var sb strings.Builder

	if m1 := WorldOverview(kb); m1 != "" {
		sb.WriteString("【世界背景】\n")
		sb.WriteString(m1)
	}
	if role := AgentRole(kb, profiles, agentID); role != "" {
		sb.WriteString("\n【人物背景】\n")
		sb.WriteString(role)
	}
	if m3 := worldDetailCore(kb); m3 != "" {
		sb.WriteString("\n【世界详细信息】\n")
		sb.WriteString(m3)
	}
	sb.WriteString("\n【生产工作流】\n")
	sb.WriteString(ProductionWorkflowText)
	return sb.String()
}

// WorldOverview renders the shared module 1: the world's basic situation —
// narrative setting/theme, zone roster, smart-object group roster, and NPC
// roster (compact inventories only; details live in module 3).
func WorldOverview(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	var lines []string
	if kb.Narrative.Setting != "" {
		lines = append(lines, "设定："+kb.Narrative.Setting)
	}
	if kb.Narrative.Theme != "" {
		lines = append(lines, "主题："+kb.Narrative.Theme)
	}
	if zs := kb.ListZones(); len(zs) > 0 {
		parts := make([]string, 0, len(zs))
		for _, z := range zs {
			if z.DisplayName != "" && z.DisplayName != z.ID {
				parts = append(parts, fmt.Sprintf("%s（%s）", z.DisplayName, z.ID))
			} else {
				parts = append(parts, z.ID)
			}
		}
		lines = append(lines, fmt.Sprintf("区域（%d 个）：%s。", len(zs), strings.Join(parts, "、")))
	}
	if os := kb.ListObjects(); len(os) > 0 {
		parts := make([]string, 0)
		for _, g := range groupObjectsBySemantic(os) {
			label := g.SemanticGroup
			if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
				label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
			}
			if g.InstanceCount > 1 {
				label += fmt.Sprintf("，%d 个实例", g.InstanceCount)
			}
			parts = append(parts, label)
		}
		lines = append(lines, fmt.Sprintf("可交互设施类别（%d 类）：%s。", len(parts), strings.Join(parts, "、")))
	}
	if ags := kb.Agents; len(ags) > 0 {
		parts := make([]string, 0, len(ags))
		for _, a := range ags {
			if a.DisplayName != "" && a.DisplayName != a.ID {
				parts = append(parts, fmt.Sprintf("%s（%s）", a.DisplayName, a.ID))
			} else {
				parts = append(parts, a.ID)
			}
		}
		lines = append(lines, fmt.Sprintf("居民（%d 位）：%s。", len(ags), strings.Join(parts, "、")))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// worldDetailCore renders the KB-derived world detail shared by the strategic
// and tactical system prompts: per-zone descriptions + a per-zone
// semantic_group map + smart objects grouped by semantic_group with
// per-interaction description, per-hour attribute effects and usage gates
// (from the KB's declared rates).
func worldDetailCore(kb *worldkb.KB) string {
	var sb strings.Builder
	wroteZone := false
	if kb != nil {
		if zs := kb.ListZones(); len(zs) > 0 {
			sb.WriteString("各区域详情：\n")
			wroteZone = true
			for _, z := range zs {
				label := z.ID
				if z.DisplayName != "" && z.DisplayName != z.ID {
					label = fmt.Sprintf("%s（%s）", z.DisplayName, z.ID)
				}
				if d := strings.TrimSpace(z.Description); d != "" {
					sb.WriteString("- " + label + "：" + d + "\n")
				} else {
					sb.WriteString("- " + label + "\n")
				}
			}
		}
	}
	// 各区域可交互设施：按实例真实分布列出，跨 zone 的 semantic_group
	//（如 bench 分布在中央广场/主生产车间/物流转运站）会在每个实际分布
	// zone 下列出，避免"长椅只在中央广场"这类误导，供规划时直接按地点选设施。
	if kb != nil {
		if zs := kb.ListZones(); len(zs) > 0 {
			if os := kb.ListObjects(); len(os) > 0 {
				if wroteZone {
					sb.WriteString("\n")
				}
				// zone → semantic_group 集合：按每个实例的 zone_id 聚合真实分布。
				zoneGroups := make(map[string]map[string]bool, len(zs))
				for _, o := range os {
					if o.ZoneID == "" || o.SemanticGroup == "" {
						continue
					}
					if zoneGroups[o.ZoneID] == nil {
						zoneGroups[o.ZoneID] = make(map[string]bool)
					}
					zoneGroups[o.ZoneID][o.SemanticGroup] = true
				}
				// semantic_group → 显示名（取 group 的 DisplayName）。
				display := make(map[string]string, len(os))
				for _, g := range groupObjectsBySemantic(os) {
					label := g.SemanticGroup
					if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
						label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
					}
					display[g.SemanticGroup] = label
				}
				sb.WriteString("各区域可交互设施：\n")
				for _, z := range zs {
					gs := zoneGroups[z.ID]
					if len(gs) == 0 {
						continue
					}
					keys := make([]string, 0, len(gs))
					for k := range gs {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					names := make([]string, 0, len(keys))
					for _, k := range keys {
						if n, ok := display[k]; ok {
							names = append(names, n)
						} else {
							names = append(names, k)
						}
					}
					zlabel := z.ID
					if z.DisplayName != "" && z.DisplayName != z.ID {
						zlabel = fmt.Sprintf("%s（%s）", z.DisplayName, z.ID)
					}
					sb.WriteString("- " + zlabel + "：" + strings.Join(names, "、") + "\n")
				}
			}
		}
	}
	// 设施详情：按 semantic_group 分组，交互行内联 KB 声明的描述与属性变动。
	if kb != nil {
		if os := kb.ListObjects(); len(os) > 0 {
			if wroteZone {
				sb.WriteString("\n")
			}
			sb.WriteString("设施详情（这些设施均为SmartObject，均附带有可交互的一个或多个interaction列在后面，可进行调用）：\n")
			effects := effectLookup(kb)
			for _, g := range groupObjectsBySemantic(os) {
				label := g.SemanticGroup
				if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
					label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
				} else {
					label = fmt.Sprintf("semantic_group=%s", g.SemanticGroup)
				}
				meta := ""
				if g.InstanceCount > 1 {
					meta += fmt.Sprintf("，%d 个实例", g.InstanceCount)
				}
				sb.WriteString("- " + label + meta + "：\n")
				// 无速率声明的交互只列动词；有声明的带描述+属性变动+门槛。
				if len(g.AvailableInteractions) == 0 {
					if d := strings.TrimRight(strings.TrimSpace(g.Description), "。"); d != "" {
						sb.WriteString("  " + d + "。\n")
					}
					continue
				}
				for _, itx := range g.AvailableInteractions {
					if e, ok := effects[g.SemanticGroup+"/"+itx]; ok {
						sb.WriteString("  - " + itx + "：" + describeEffect(e) + "\n")
					} else {
						sb.WriteString("  - " + itx + "\n")
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// effectLookup builds a (semantic_group, interaction) → InteractionEffect map
// from the merged KB's declared interaction rates.
func effectLookup(kb *worldkb.KB) map[string]InteractionEffect {
	effects := InteractionEffectsFromKB(kb)
	if len(effects) == 0 {
		return nil
	}
	m := make(map[string]InteractionEffect, len(effects))
	for _, e := range effects {
		m[e.SemanticGroup+"/"+e.Interaction] = e
	}
	return m
}

// StrategicRules is the seven planning rules, injected into the user message
// (StrategicUserTemplate's third placeholder) so they sit adjacent to the
// planning ask (recency effect: instructions closer to the ask are followed
// more reliably). References to 【世界背景】/【人物背景】/【世界详细信息】
// point at the system message's modules; references to 【物理状态】 point at
// the user message's dynamic segments.
const StrategicRules = `1. 输出 JSON 数组（6-8 条），每条只含 "time"（"HH:MM-HH:MM"）和 "goal"（一句话，必须是纯文本字符串），以 [ 开头 ] 结尾，不要其他文字
   - goal 用干练简洁的客观描述，只写"在哪 + 做什么"（如"主生产车间工作台装配""中央广场充电桩充电"），不带语气词、口头禅、内心独白或人设腔调，人设只影响选择什么活动、如何安排时段，不影响 goal 的文字风格
2. 【硬性要求】每个时段的结束时间减去开始时间必须 ≥30 分钟（不足 30 分钟的活动要么并入相邻时段，要么不安排）；每段安排 1 - 2 项任务，连续两个时段不得任务完全相同
3. 规划每个时段时，先想清楚这个时段的活动用什么实现：goal 应能映射到【世界详细信息】设施详情中列出的某个 (semantic_group, interaction) 组合——不限于工种设备，睡眠舱的 sleep/meditate/tidy_up、长椅的 rest 都是合法活动，战术层会据此分解为对应的移动与长时段互动；映射不上的抽象活动（如"准备工具""巡查"）→ 换一个。锻炼类活动（晨练拉伸等原地动作）不需要设施，属例外
4. goal 中提到的地点、人物、设备必须是系统信息中【人物背景】和【世界详细信息】里存在的，不得编造未提及的人物或设施
5. 第一个时段必须从 07:00 开始，且任何时段的开始时间不得早于 07:00——禁止输出 0:00-7:00 这类凌晨睡觉时段（凌晨睡眠已由前一晚的跨午夜末段覆盖，不要重复安排）。首段原则上不安排工作——早间也可以安排晨练拉伸（原地锻炼，不需要特定设施）、上网、长椅放松、冥想醒神、整理舱位等非工作活动，按性格与状态选择。午间可以选择锻炼、就近长椅小憩、休眠舱午睡等非产出性活动。夜间睡眠必须是一个连续的跨午夜时段：约 22:00 前后开始、次日 06:00-07:00 结束；不得拆成多个睡眠时段（禁止 20:30-22:58 睡觉 + 22:58-07:16 睡觉这样的连续两段），也不得在凌晨提前结束（禁止 23:00-01:00 这样的短睡眠段）。末段跨午夜时结束时间表示次日时刻
6. 充电仅在规划时电量为"低"或"较低"时安排；维护仅在关节磨损达到"明显磨损"及以上时安排；电量高时优先产出性活动；睡眠只能在午间和晚上
7. 综合用户信息中【物理状态】的四项状态调整安排侧重点：电量偏低→多充电少工作；疲劳偏高→提前休眠；磨损偏高→安排维护；余额低→多工作少花钱
8. 休息类时段（午休、长椅小憩等）就近安排：优先在附近区域的休息设施休息——长椅在主生产车间、物流转运站等各区域均有分布，不必专程前往中央广场；中央广场长椅仅适合社交性休息或本就在广场时
9. 整理内务、冥想等动作安排的时间不得超过一小时

格式示例：[{"time":"07:00-9:00","goal":"xxx"},{"time":"9:00-12:00","goal":"xxx"}]`

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
只输出 JSON 数组，每条形如 {"time":"HH:MM-HH:MM","goal":"纯文本一句话"}，"goal" 必须是字符串；第一个时段从 07:00 开始，任何时段的开始时间不得早于 07:00；每个时段必须 ≥120 分钟；不要输出任何其他文字。`

// BuildStrategicUserContext constructs the strategic layer user message's
// dynamic context segment: 【今日日程】 (weekly schedule context, skipped
// when empty) + 【物理状态】 (nil physical → default fresh state).
func BuildStrategicUserContext(agentID string, profiles map[string]*profile.Profile, physical *protocol.PhysicalState, dayContext string) string {
	var sb strings.Builder
	// 【今日日程】段：每周日程上下文（星期几 + 工作日/休息日 + 当日提示）。
	// dayContext 由调用方通过 weeklyschedule.WeeklyLine(dayCount, sched) 预格式化，
	// pkg/prompt 不依赖 weeklyschedule 包（解耦）。空串=禁用或 dayCount<0，跳过。
	sb.WriteString(`你是小镇居民 NPC 的战略规划模块。每天清晨 07:00，你根据系统信息中的【世界背景】【人物背景】【世界详细信息】，以及用户信息中的今日日程、物理状态、昨日总结与规划要求，规划当天 07:00 到次日 07:00 的活动安排。

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
	return sb.String()
}
