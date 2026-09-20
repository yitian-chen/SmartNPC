// Package prompt — strategic layer prompt builder.
package prompt

import (
	"fmt"
	"hash/fnv"
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

// DefaultDailyPlan derives a per-agent fallback daily plan from KB.
// kb == nil → returns defaultDailyPlan (neutral, no KB-specific terms).
//
// 工作时段从 KB 的工作类设施（category=work）中按 agentID 稳定选择，
// 而非机械取"首个 zone + 首个 object"——后者在 objects 按字典序排列的
// KB 里会选中 bench-1（长椅，休息设施），拼出"在档案馆进行长椅作业"
// 这种荒谬组合（2026-09-03 实测：venus 429 限流导致 5 NPC 全部兜底，
// 且兜底内容完全相同）。按 agentID 选择让各 NPC 的兜底计划不同。
// 无工作类设施时退化为中性表述（"主要工作"）。
func DefaultDailyPlan(kb *worldkb.KB, agentID string) string {
	if kb == nil {
		return defaultDailyPlan
	}
	workName := ""
	workZoneName := ""
	works := make([]worldkb.ObjectInfo, 0, len(kb.ListObjects()))
	for _, o := range kb.ListObjects() {
		if o.Category == "work" {
			works = append(works, o)
		}
	}
	if len(works) > 0 {
		pick := works[stableAgentPick(agentID, len(works))]
		workName = pick.DisplayName
		if workName == "" {
			workName = pick.ID
		}
		workZoneName = zoneDisplayName(kb, pick.ZoneID)
	}
	if workName == "" {
		workName = "主要工作"
	}
	if workZoneName == "" {
		workZoneName = "主要区域"
		if zs := kb.ListZones(); len(zs) > 0 {
			if zs[0].DisplayName != "" {
				workZoneName = zs[0].DisplayName
			} else {
				workZoneName = zs[0].ID
			}
		}
	}
	return fmt.Sprintf("07:00-12:00: 上午在%s进行%s作业\n", workZoneName, workName) +
		"12:00-13:00: 午间停工与短暂休息\n" +
		fmt.Sprintf("13:00-18:00: 下午继续%s作业\n", workName) +
		"18:00-22:00: 保养休息\n" +
		"22:00-06:00: 夜间休眠"
}

// stableAgentPick 按字符串稳定散列选择 [0,n) 桶——同一 agentID 每次选
// 同一桶（兜底计划跨重试稳定），不同 agentID 尽量错开。
func stableAgentPick(s string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return int(h.Sum32() % uint32(n))
}

// zoneDisplayName 查 zone 显示名，找不到或为空时回退 zoneID，再回退空串。
func zoneDisplayName(kb *worldkb.KB, zoneID string) string {
	if zoneID == "" {
		return ""
	}
	for _, z := range kb.ListZones() {
		if z.ID == zoneID {
			if z.DisplayName != "" {
				return z.DisplayName
			}
			return z.ID
		}
	}
	return ""
}

// StrategicRules is the planning rules, injected into the user message
// (BuildStrategicUserPrompt) so they sit adjacent to the
// planning ask (recency effect: instructions closer to the ask are followed
// more reliably). References to 【世界背景】/【人物背景】/【世界详细信息】
// point at the system message's modules; references to 【物理状态】 point at
// the user message's dynamic segments.
const StrategicRules = `1. 【硬性要求】每个时段的结束时间减去开始时间必须 ≥30 分钟（不足 30 分钟的活动要么并入相邻时段，要么不安排）；每段安排 1 - 2 项任务，连续两个时段不得任务完全相同
2. 规划每个时段时，先想清楚这个时段的活动用什么实现：goal 应能映射到【世界详细信息】设施详情中列出的某个 (semantic_group, interaction) 组合——不限于工种设备，睡眠舱的 sleep/meditate/tidy_up、长椅的 rest 都是合法活动，战术层会据此分解为对应的移动与长时段互动；映射不上的抽象活动（如"准备工具""巡查"）→ 换一个。锻炼类活动（晨练拉伸等原地动作）不需要设施，属例外
3. goal 中提到的地点、人物、设备必须是系统信息中【人物背景】和【世界详细信息】里存在的，不得编造未提及的人物或设施
4. 第一个时段必须从当前仿真时间开始，且任何时段的开始时间不得早于当前时间（清晨规划时禁止输出 0:00-7:00 这类凌晨睡觉时段——凌晨睡眠已由前一晚的跨午夜末段覆盖，不要重复安排）。
5. 首段禁止安排工作——早间可以安排晨练拉伸、上网、长椅放松、冥想醒神、整理舱位等非工作活动。午间可以选择锻炼、就近长椅小憩、休眠舱午睡等非产出性活动。夜间睡眠必须是一个连续的跨午夜时段：约 22:00 前后开始、次日 06:00-07:00 结束；不得拆成多个睡眠时段（禁止 20:30-22:58 睡觉 + 22:58-07:16 睡觉这样的连续两段），也不得在凌晨提前结束（禁止 23:00-01:00 这样的短睡眠段）。末段跨午夜时结束时间表示次日时刻
6. 充电仅在规划时电量为"低"或"较低"时安排，规划时电量为"高"或"中"时严禁规划充电；维护仅在关节磨损达到"明显磨损"及以上时安排；睡眠只能在午间和晚上
7. 综合用户信息中【物理状态】的四项状态调整安排侧重点：电量偏低→多充电少工作；疲劳偏高→提前休眠；磨损偏高→安排维护；余额低→多工作少花钱
8. 整理内务、冥想等动作安排的时间不得超过一小时

格式示例：[{"time":"07:00-09:00","goal":"xxx"},{"time":"09:00-12:00","goal":"xxx"},{"time":"12:00-14:00","goal":"xxx"},{"time":"14:00-18:00","goal":"xxx"},{"time":"18:00-22:00","goal":"xxx"},{"time":"22:00-07:00","goal":"xxx"}]`

// StrategicPromptInput aggregates the strategic layer user-prompt inputs.
type StrategicPromptInput struct {
	TimeOfDay        string // 当前游戏时间 "HH:MM"（触发时刻）
	Zone             string // 当前所在 zone id；空则省略位置段
	Hint             string // 本次规划触发原因（如"早晨例行制定每日日程安排"）
	Context          string // BuildStrategicUserContext 产出（动态上下文段）
	YesterdaySummary string // 含"昨日总结："前缀
}

// BuildStrategicUserPrompt 组装战略层 user prompt——任意游戏时间可用。
// 布局：情境行（时间+位置）→ 触发原因 → 动态上下文 → 昨日总结 → 规划要求
// → 收尾指令。规划区间为「当前时间 → 次日 07:00」，第一个时段从当前仿真
// 时间开始（早晨例行触发时等价于从 07:00 开始，行为与旧模板一致）。
func BuildStrategicUserPrompt(in StrategicPromptInput) string {
	var sb strings.Builder
	sb.WriteString("[战略层/日程规划] 现在是仿真时间 " + in.TimeOfDay)
	if in.Zone != "" {
		sb.WriteString("，你当前位于" + in.Zone)
	}
	sb.WriteString("。\n\n")
	if in.Hint != "" {
		sb.WriteString(in.Hint + "\n\n")
	}
	sb.WriteString(in.Context + "\n\n")
	sb.WriteString(in.YesterdaySummary + "\n\n")
	sb.WriteString("规划要求：\n")
	sb.WriteString(StrategicRules)
	sb.WriteString("\n\n")
	sb.WriteString(`请基于你的角色身份和性格，规划从当前时间到次日 07:00 的活动安排（人设只影响选什么活动、怎么安排时段；goal 文字一律干练简洁，不带人设语气）。
只输出 JSON 数组，每条形如 {"time":"HH:MM-HH:MM","goal":"纯文本一句话"}，"goal" 必须是字符串；第一个时段从当前仿真时间开始，任何时段的开始时间不得早于当前时间；分出6 - 8个时段，每个时段必须 ≥30 分钟；不要输出任何其他文字。`)
	return sb.String()
}

// BuildStrategicUserContext constructs the strategic layer user message's
// dynamic context segment: 【今日日程】 (weekly schedule context, skipped
// when empty) + 【物理状态】 (nil physical → default fresh state). The
// 【其他NPC】 roster segment was removed (social_chat is masked from the
// strategic layer). The strategic preamble (module role) also lives here:
// the system prompt is shared verbatim across layers, so every
// strategic-specific text belongs in the user message.
func BuildStrategicUserContext(agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, physical *protocol.PhysicalState, dayContext string) string {
	var sb strings.Builder
	// 【今日日程】段：每周日程上下文（星期几 + 工作日/休息日 + 当日提示）。
	// dayContext 由调用方通过 weeklyschedule.WeeklyLine(dayCount, sched) 预格式化，
	// pkg/prompt 不依赖 weeklyschedule 包（解耦）。空串=禁用或 dayCount<0，跳过。
	sb.WriteString(`你是小镇居民 NPC 的战略规划模块。你根据系统信息中的【世界背景】【人物背景】【世界详细信息】，以及用户信息中的触发原因、今日日程、物理状态、其他NPC、昨日总结与规划要求，规划当前时间到次日 07:00 的活动安排。

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
	// 【其他NPC】段已屏蔽（向 LLM 屏蔽 social_chat）：原实现列出 KB 中除自己
	// 外的所有 NPC（id + 职业），让战略层 LLM 在 07:00 规划时看到可聊天的同伴
	// ——social_chat 的 target_agent_id 需要具体 id。现在战略层不再引导主动
	// 社交（规则 2 的 social_chat 映射、格式示例的社交时段已一并移除），该段
	// 整体注释掉。战术层仍通过 OtherAgentsLine 作运行时回退（见 tactical.go）。
	// if peers := OtherAgentsLine(kb, agentID); peers != "" {
	// 	sb.WriteString("【其他NPC】（你可以主动找其中某位聊天 social_chat，维系人际关系）\n")
	// 	sb.WriteString(peers)
	// 	sb.WriteString("\n")
	// }
	return sb.String()
}
