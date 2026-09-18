// Package prompt — tactical layer prompt builder.
package prompt

import (
	"fmt"
	"strings"
)

// TacticalRules is the tactical decomposition rules, injected into the user
// message (recency effect: instructions closer to the ask are followed more
// reliably). References to 【世界背景】/【人物背景】/【世界详细信息】 point
// at the system message's modules; references to 【物理状态】/
// 【附近NPC】/【物体实时占用】 point at the user message's dynamic segments.
// The available tools are NOT listed here — they arrive via the
// function-calling `tools` request field.
const TacticalRules = `1. 第一个工具调用必须是 speak（用一段话表达此刻内心想法或独白），随后必须返回至少一个带 duration 的长动作（InteractSmartObject 设施互动或 exercise 原地锻炼），最多 6 个动作段，按执行顺序排列。禁止只返回 speak——只返回 speak 会被系统判定为失败并重试。可以在动作之间穿插speak表达现在的情况。
2. 你可以根据当前NPC的实际属性、实际游戏时间等信息灵活安排，如果当前此条日程并不合理，例如半夜不睡觉而是跑步/工作、电量不为低时就去充电等情况，请下发更合理的动作，不必遵守原有日程规定。
3. InteractSmartObject 通过 zone + semantic_group 定位目标设施实例并完成互动，直接调用并填好 semantic_group / interaction / zone / duration 即可，无需在其前面额外调用 move_to。
4. 禁止把同一动作连续重复多次填充时段（工作段之间应穿插休息段）。
5. InteractSmartObject 的 semantic_group 必须严格使用设施详情中给出的 semantic_group 值，禁止编造、禁止用实例 id（如 Charge-1）。
6. InteractSmartObject 的 semantic_group 与 interaction 必须严格对应，禁止跨类别组合——所有工种设备与生活设施都可用 InteractSmartObject 直接交互，semantic_group 填设施、interaction 填对应动词即可（如 workbench/assemble、process_machine/process、charger/charge、sleep_pod/sleep、bench/rest 等，完整映射见设施详情）。
7. 所有非瞬时动作（InteractSmartObject 设施互动、exercise 原地锻炼等需要持续一段时间的）都必须填写 duration 参数（秒，schema 必填）；move_to 的移动时长由 UE 自动决定、无需填 duration；瞬时动作（speak 等立即完成的）也不填 duration。duration 要合理：冥想、整理床铺等单段设 1800 秒左右，不宜超过 1 小时；工作段可设 3600-7200 秒。到点后系统会打断该段并继续执行后续动作段；只有全部动作执行完，系统才会再次询问。推荐模式：工作段（如 1.5 小时）→ 长椅小憩/原地拉伸段（不超过 30 分钟）→ 返回工作段（duration 设为时段剩余时长）。
8. 每次生成的最后一个动作必须是长动作（InteractSmartObject 长动作），其 duration 设为当前时段的剩余时长（见上文"剩余约 X 分钟"提示）——到点后系统自动切入下一时段，NPC 不会呆站。所有动作的 duration 总和应接近当前时段的剩余时长，避免过短导致队列提前耗尽触发重分解、或过长拖到下一时段。
9. 如果是调用 InteractSmartObject 工具，若当前日程目标明确指定了区域（如"去中央广场长椅休息"），**必须**在该工具的 zone 参数中填写对应区域 id（如 central_plaza、logistics_hub）。`

// tacticalCoreRules 是精简模式（Compact=true）下替代完整 TacticalRules 的
// 核心约束速览，覆盖实测高频失败模式（speak-only 队列、duration 漏填、
// 末段非长动作、semantic_group 编造）。完整规则经 tacticalCompactRefLine
// 指向本日第一条战术 user 消息，不再逐轮重复。
const tacticalCoreRules = `- 首个工具调用必须是 speak；随后必须返回至少一个带 duration 的长动作，禁止只返回 speak。
- 除了speak和移动，其他必须填 duration（秒）：中间动作约 1800 秒、工作段 3600-7200 秒。
- 最后一个动作必须是长动作，duration 设为当前时段剩余时长。`

// tacticalCompactRefLine 是精简模式的引用行：指向本日第一条战术消息的
// 全量头。"以最新一份为准"覆盖跨日交错等边界下历史出现多份全量头的情况。
// 引用行带"战术规划模块"开头标记——历史中夹杂 [战略层/日程规划] 开头的
// 战略消息、[系统注入] 工具结果与对话层消息，该前缀是战术消息唯一无歧义
// 的识别特征。
const tacticalCompactRefLine = `完整分解规则与全天日程见本日第一条战术分解指令（以"你是小镇居民 NPC 的战术规划模块"开头、含【分解规则】与【全天日程】段的消息），全部要求继续适用；若历史中有多份，以最新一份为准。`

// BuildTactical constructs the tactical layer's user message, four parts:
//  1. 全天任务与当前时段任务 — full-day schedule + current slot goal +
//     slot duration hint.
//  2. NPC与环境实时状态 — realtime state from the latest perception_update:
//     zone, game time, physical state, recent memories, relationships,
//     nearby NPCs, object occupancy, replan hint (incl. physical-alert
//     constraints). Tools are NOT injected into the prompt text — they are
//     passed via the function-calling `tools` request field instead.
//  3. 分解规则 — TacticalRules (injected adjacent to the ask).
//  4. 任务 — the decomposition ask + goal-specific example.
//
// KB/world/persona live in the system message (BuildSharedSystemPrompt,
// shared verbatim with the strategic and dialogue layers).
func BuildTactical(in TacticalInput) string {
	th := BandThresholdsFor(in.Profiles, in.AgentID)

	var sb strings.Builder
	sb.WriteString("你是小镇居民 NPC 的战术规划模块。你根据系统信息中的【世界背景】【人物背景】【世界详细信息】，以及用户信息中的全天任务与当前时段任务、NPC与环境实时状态、分解规则，把当前时段目标分解为一个或多个 action，按顺序执行。\n")

	// ── 一、全天任务与当前时段任务 ──
	sb.WriteString("一、全天任务与当前时段任务\n")
	// 精简模式（Compact=true）省略【全天日程】：日内不变块只在每天第一条
	// 战术 user 消息出现，后续轮次经 tacticalCompactRefLine 引用。
	if in.DailyPlan != "" && !in.Compact {
		sb.WriteString("【全天日程】\n")
		sb.WriteString(in.DailyPlan)
		if !strings.HasSuffix(in.DailyPlan, "\n") {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("【当前时段目标】" + in.Goal + "\n")
	sb.WriteString(SlotDurationHint(in.Slot, in.TimeOfDay))

	// ── 二、NPC与环境实时状态 ──
	sb.WriteString("\n二、NPC与环境实时状态\n")
	sb.WriteString(fmt.Sprintf("你目前在：%s，游戏时间 %s。\n", in.Zone, in.TimeOfDay))
	if line := PhysicalLine(in.Physical, th); line != "" {
		// PhysicalLine 自带"物理状态："前缀，与段头【物理状态】去重。
		sb.WriteString("【物理状态】\n")
		sb.WriteString(strings.TrimPrefix(line, "物理状态：") + "\n")
	}
	// P3-7 安全点 drain（§4.4/§5.1 三输入之一）：攒下的非 force 事件
	// 一次性全取注入。逐次变化的数据，compact 模式同样注入（非日内不变块）。
	if in.Events != "" {
		sb.WriteString("【发生的事件】（自上次规划以来世界上发生的事，请纳入本轮安排考虑）\n")
		sb.WriteString(in.Events)
		sb.WriteString("\n")
	}
	// P3-9 修复 A：持续情境（未被解除的威胁）。事件是边沿触发的，但威胁
	// 在解除信号到达前持续存在——每轮 refill 都要可见，否则 LLM 会脑补
	// "威胁解除了"。
	if in.Situations != "" {
		sb.WriteString("【当前处境】以下情境仍在持续、尚未收到解除信号，安排行动时必须考虑：\n")
		sb.WriteString(in.Situations)
		sb.WriteString("\n")
	}
	if in.Memories != "" {
		sb.WriteString("【过往经验】\n" + in.Memories)
		if !strings.HasSuffix(in.Memories, "\n") {
			sb.WriteString("\n")
		}
	}
	if in.Relationships != "" {
		sb.WriteString("【人际关系】\n" + in.Relationships)
		if !strings.HasSuffix(in.Relationships, "\n") {
			sb.WriteString("\n")
		}
	}
	nearbyLine := NearbyAgentsLine(in.VisibleAgents, in.KB)
	if nearbyLine == "" && in.KB != nil {
		// 附近无可见 NPC 时 fallback 到 KB 静态花名册，让 LLM 始终能看到
		// NPC id 列表（social_chat 的 target_agent_id 需要 id 而非显示名）。
		nearbyLine = OtherAgentsLine(in.KB, in.AgentID)
	}
	if nearbyLine != "" {
		sb.WriteString(nearbyLine)
		if !strings.HasSuffix(nearbyLine, "\n") {
			sb.WriteString("\n")
		}
	}
	// 物体实时占用模块暂时移除：ObjectStatusContext 会注入【物体实时占用】段，
	// 列出各类设施的空闲/占用数量。当前仿真中该段信息量大、且与战术层
	// duration 约束叠加后可能干扰 LLM 的时长规划，先注释停用，需要时恢复。
	// if os := ObjectStatusContext(in.ObjectStatus, in.NearbyObjects, in.KB); os != "" {
	// 	sb.WriteString(os)
	// 	if !strings.HasSuffix(os, "\n") {
	// 		sb.WriteString("\n")
	// 	}
	// }
	hintLine := tacticalHintLine(in, th)
	if hintLine != "" {
		sb.WriteString(hintLine)
		if !strings.HasSuffix(hintLine, "\n") {
			sb.WriteString("\n")
		}
	}

	// ── 三、分解规则 ──
	sb.WriteString("\n三、分解规则\n")
	if in.Compact {
		sb.WriteString(tacticalCoreRules)
		sb.WriteString("\n")
		sb.WriteString(tacticalCompactRefLine)
		sb.WriteString("\n")
	} else {
		sb.WriteString(TacticalRules)
		sb.WriteString("\n")
	}

	// ── 四、任务 ──
	sb.WriteString("\n四、任务\n")
	sb.WriteString("请通过工具调用（function calling）把【当前时段目标】分解为动作序列，按顺序执行（首个工具调用是 speak）。\n")
	return sb.String()
}

// tacticalHintLine renders the replan hint. Two hint flavors:
//   - "【强制打断】" 前缀（force 事件，P1-3）：升级为【紧急事件】最高优先级
//     指令——显式授权战术层暂停时段目标（设计文档 §4.2/§5.5 的否决权），
//     并豁免"时长填满/末段长动作"规则。不这样写的话，事件只是规则段前
//     的一行说明性注释，主指令（分解时段目标 + 填满剩余时长）全部指向
//     恢复原工作——LLM 的理性选择就是一句 speak 后继续原时段动作
//     （2026-09-17 仿真实测：被玩家瞄准 → 说"先躲躲" → 继续拆解原设备）。
//   - "物理状态告警" marker (set by upgradeIfPhysicalAlert): type-specific
//     recovery constraints based on which physical values are actually in
//     alert. Pairs with physicalAlertOverrideGoal (code-layer goal override)
//     as double insurance. Different alert types drive different recovery
//     actions:
//   - 低电量 → charge_at_station 充电
//   - 高疲劳 → charge_at_station 充电 / rest_at_residence 休息
//   - 高关节磨损 → self_maintenance 维修保养
func tacticalHintLine(in TacticalInput, th BandThresholds) string {
	if in.Hint == "" {
		return ""
	}
	if strings.HasPrefix(in.Hint, "【强制打断】") {
		event := strings.TrimPrefix(in.Hint, "【强制打断】")
		return "【紧急事件】" + event + "\n" +
			"本轮规划的最高优先级是应对上述紧急事件，它优先于【当前时段目标】和下方分解规则：" +
			"你有权暂停原计划，先妥善处置事件（如撤离威胁范围、移动到安全位置、寻找同伴支援、保持警戒观察等，" +
			"具体做法由你结合角色性格与事件性质决定），处理完且时间允许时再回到时段目标。" +
			"本轮动作时长按应对事件的实际需要安排即可，无需用长动作填满时段剩余时长——" +
			"事件应对只需短时间时，队列耗尽后系统会自然重新规划回到日程。" +
			"若该事件描述的是威胁类情境（如被攻击/被瞄准），在收到明确的解除信号（脱离战斗）之前应视为持续存在，" +
			"不得自行认定威胁已解除。"
	}
	hintLine := "【上次中断原因】" + in.Hint + "（请据此调整本轮规划）"
	if !strings.Contains(in.Hint, "物理状态告警") || in.Physical == nil || in.Physical.IsZero() {
		return hintLine
	}
	var reqs, forbids []string
	if in.Physical.Energy < th.EnergyAlert() {
		reqs = append(reqs, "- 电量过低：必须优先 InteractSmartObject 充电（charger/charge）补能")
	}
	if in.Physical.Fatigue > th.FatigueAlert() {
		reqs = append(reqs, "- 疲劳过高：优先 InteractSmartObject 充电（charger/charge）或到睡眠舱休息（sleep_pod/sleep），充电后若仍疲劳追加休息")
	}
	if in.Physical.JointWear > th.JointWearAlert() {
		reqs = append(reqs, "- 关节磨损过高：必须优先 InteractSmartObject 维护保养（repair_table/repair），否则持续工作会加剧损耗")
	}
	// 禁止项：仅禁止与所有活跃告警冲突的消耗性动作
	// 关节磨损告警时不禁维护保养（那是需要的恢复动作）
	fatigueAlert := in.Physical.Fatigue > th.FatigueAlert()
	jointWearAlert := in.Physical.JointWear > th.JointWearAlert()
	if fatigueAlert {
		forbids = append(forbids, "工作类 InteractSmartObject（如 workbench/assemble，消耗体力）")
	}
	if jointWearAlert {
		forbids = append(forbids, "InteractSmartObject 上网（computer/surf_internet，无助于恢复）")
	}
	if fatigueAlert {
		forbids = append(forbids, "move_to 到非恢复设施区域")
	}
	if len(reqs) > 0 {
		hintLine += "\n【物理告警强制约束】当前物理状态已突破警戒阈值，必须立即规划恢复类动作：\n" +
			strings.Join(reqs, "\n")
		if len(forbids) > 0 {
			hintLine += "\n禁止规划以下动作：" + strings.Join(forbids, "、")
		}
	}
	return hintLine
}

// SlotDurationHint constructs a hint line based on slot "HH:MM-HH:MM" and
// current game_time. Guides the LLM to plan by remaining duration
// (slot_end - timeOfDay), avoiding long actions that overshoot into next slot.
// timeOfDay empty or parse failure → degrades to full slot duration.
// Supports cross-midnight slots.
func SlotDurationHint(slot, timeOfDay string) string {
	start, end := SlotRangeMinute(slot)
	if start < 0 {
		return ""
	}
	total := end - start
	curMin := ParsePlanMinute(timeOfDay)
	if curMin < 0 {
		return fmt.Sprintf("当前时段 %s，约 %d 分钟；请让步骤总时长接近此时长，避免过短导致队列提前耗尽触发重分解。\n", slot, total)
	}
	curMin = NormalizeTodToSlot(curMin, start, end)
	remaining := end - curMin
	if remaining <= 0 {
		return fmt.Sprintf("当前时段 %s 已过期（game_time=%s 已超出时段末尾），请仅规划 1-2 个短动作（≤10 分钟），避免 overshoot。\n", slot, timeOfDay)
	}
	if remaining < total {
		elapsed := curMin - start
		return fmt.Sprintf("当前时段 %s，剩余约 %d 分钟（已过去 %d 分钟）；请让步骤总时长接近剩余时长，避免过短导致队列提前耗尽触发重分解。\n", slot, remaining, elapsed)
	}
	return fmt.Sprintf("当前时段 %s，约 %d 分钟；请让步骤总时长接近此时长，避免过短导致队列提前耗尽触发重分解。\n", slot, total)
}
