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

各活动对属性的每小时影响幅度见用户信息中【动作对属性的影响】段（由 world KB 声明生成）。规划时请综合权衡：产出性活动（工作）赚取余额但消耗体力、缓慢积攒关节磨损；恢复性活动（充电/维护/休息）花余额但延续工作能力。避免长时间连续工作导致体力耗尽，也避免频繁恢复导致余额入不敷出。

要求：
1. 输出 JSON 数组（6-8 条），每条只含 "time"（"HH:MM-HH:MM"）和 "goal"（一句话，必须是纯文本字符串），以 [ 开头 ] 结尾，不要其他文字
   - goal 用干练简洁的客观描述，只写"做什么 + 在哪"（如"主生产车间工作台装配""中央广场充电桩充电"），不带语气词、口头禅、内心独白或人设腔调
   - 人设只影响选择什么活动、如何安排时段，不影响 goal 的文字风格；说话语气留给执行时的 speak 动作表达
2. 【硬性要求】每个时段的结束时间减去开始时间必须 ≥120 分钟（不足 120 分钟的活动要么并入相邻时段，要么不安排；午休等短暂休息也至少120分钟）；仅安排一项主要任务；连续两个时段不得任务相同
3. 规划每个时段时，先想清楚这个时段的活动要用用户信息中【可用能力】里哪个 cmd 实现：
   - 有对应 cmd 的活动（如装配→work_shift、充电→charge_at_station、加工/调试/拆解→InteractSmartObject 填工作设备）→ 可以安排
   - 没有对应 cmd 的活动（如"准备工具""巡查""整理仪容"）→ 不要安排，改用有 cmd 对应的活动
   - 判断标准：goal 能否直接映射到【可用能力】中列出的某个 cmd？能 → 可以安排；不能 → 换一个
4. goal 中提到的地点、人物、设备必须是用户信息中【你的角色】和【世界知识】里存在的，不得编造未提及的人物或设施
5. 第一个时段必须从 07:00 开始，且任何时段的开始时间不得早于 07:00——禁止输出 0:00-7:00 这类凌晨睡觉时段（凌晨睡眠已由前一晚的跨午夜末段覆盖，不要重复安排）。首段必须是日间活动，不得安排休眠；首段不一定是工作——早间也可以安排上网、长椅放松、充电等非工作活动，按性格与状态选择。夜间睡眠必须是一个连续的跨午夜时段：约 22:00 前后开始、次日 06:00-07:00 结束；不得拆成多个睡眠时段（禁止 20:30-22:58 睡觉 + 22:58-07:16 睡觉这样的连续两段），也不得在凌晨提前结束（禁止 23:00-01:00 这样的短睡眠段）。末段跨午夜时结束时间表示次日时刻
6. 充电原则上仅在能量为"低电量"或疲劳为"非常疲劳"时安排；维护仅在关节磨损达到"明显磨损"及以上时安排；能量充足时优先产出性活动
7. 综合用户信息中【物理状态】的四项状态调整安排侧重点：能量偏低→多充电少工作；疲劳偏高→提前休眠；磨损偏高→安排维护；余额低→多工作少花钱
8. 疲劳恢复时段（如午间）的方式应多样化，主要依据你的性格特质（用户信息中【你的角色】）从以下方式中选择一种，不要固定去长椅或充电：
   - 回休眠舱小憩（rest_at_residence）
   - 去充电桩充电兼休息（charge_at_station）
   - 在中央广场长椅上放松（InteractSmartObject，bench/rest）
   - 去档案馆上网放松（surf_internet）
   仅两个强制例外：能量为"低电量"时必须选充电；疲劳为"非常疲劳"时必须回舱小憩。其余情况一律按性格倾向自由选择：慵懒型回舱小憩，省电耐久型充电恢复，好动型长椅或上网

格式示例：[{"time":"07:00-9:00","goal":"xxx"},{"time":"9:00-12:00","goal":"xxx"}]`

// StrategicUserTemplate is the strategic layer's user message template.
// Placeholders: %s = strategic context (BuildStrategic output: role +
// schedule + physical + KB + peers + capabilities), %s = yesterday summary.
// The instruction line stays in the user message so the "plan today" ask
// sits immediately after the data it refers to.
const StrategicUserTemplate = `[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了，你刚从休眠舱醒来，当前位于休眠舱区域。

%s

%s

请基于你的角色身份和性格，规划今天一天的活动安排（人设只影响选什么活动、怎么安排时段；goal 文字一律干练简洁，不带人设语气）。一天从 07:00 到次日 07:00，你从 07:00 开始活动。
只输出 JSON 数组，每条形如 {"time":"HH:MM-HH:MM","goal":"纯文本一句话"}，"goal" 必须是字符串；第一个时段从 07:00 开始，任何时段的开始时间不得早于 07:00；每个时段必须 ≥120 分钟；不要输出任何其他文字。`

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
		sb.WriteString("此外始终可用基础动作：移动、说话、表达情绪、与物体交互（InteractSmartObject）、等待（用于短耗时或衔接）。\n")
		sb.WriteString("与物体交互（InteractSmartObject）也可直接用于任何工种：semantic_group 填工作设备即可——如加工机（process）、调试台（debug）、拆解台（dismantle）、工作台（assemble）、分拣传送带（sort_cargo）、质检台（inspect），战术层会据此分解为在工作设备上的长时段作业。\n")
	}
	// 【动作对属性的影响】段：Go 运行时从 world_kb 推送的互动速率声明
	// 生成（InteractionEffectsFromKB + BuildCmdEffectsText，main 的
	// world_kb handler 在合并后经 SetCmdEffects 注入）。
	// 与 KB/能力列表独立——空串（UE 未推送速率）时整段跳过。
	if cmdEffectsText != "" {
		sb.WriteString("【动作对属性的影响】\n")
		sb.WriteString(cmdEffectsText)
		if !strings.HasSuffix(cmdEffectsText, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// cmdEffectsText is the natural-language cmd→attribute-effect summary
// ("各活动对属性的影响（每游戏小时平均变化…）: - work_shift（assemble）：…").
// Derived offline by scripts/cmd_effect_summary.py from sim logs (the world KB
// itself carries no numeric effect table); loaded once at startup by main and
// injected into every strategic prompt as the 【动作对属性的影响】 segment.
// Empty (default) = disabled, segment skipped.
var cmdEffectsText string

// SetCmdEffects installs the cmd attribute-effect summary text. Call once at
// startup (before any BuildStrategic call); empty string disables the segment.
func SetCmdEffects(s string) { cmdEffectsText = s }

// CmdEffects returns the currently installed summary (empty = disabled).
func CmdEffects() string { return cmdEffectsText }

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
