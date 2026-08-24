// Package prompt — tactical layer prompt builder.
package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// BuildTacticalSystemPrompt constructs the tactical layer's system message.
// It shares the strategic layer's KB/cmd-derived modules ("绝大部分相同"),
// stable within a session (per agent) and thus cacheable:
//  1. 【世界背景】 — world overview (WorldOverview, shared with strategic).
//  2. 【人物背景】 — the current agent's profile (AgentRole).
//  3. 【世界详细信息】 — shared world detail core (zone descriptions +
//     facility groups with inline per-interaction effects).
//
// The decomposition rules (TacticalRules) live in the user message so they
// sit adjacent to the decomposition ask. Per-call data (full-day plan,
// current slot goal, realtime state, example) also lives in the user
// message (BuildTactical). The tool list itself is NOT rendered in the
// prompt — it is passed via the function-calling `tools` request field.
func BuildTacticalSystemPrompt(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	var sb strings.Builder

	if m1 := WorldOverview(kb); m1 != "" {
		sb.WriteString("【世界背景】\n")
		sb.WriteString(m1)
	}
	if role := AgentRole(kb, profiles, agentID); role != "" {
		sb.WriteString("\n【人物背景】\n")
		sb.WriteString(role)
	}
	sb.WriteString("\n【世界详细信息】\n")
	sb.WriteString(worldDetailCore(kb))
	return sb.String()
}

// TacticalRules is the tactical decomposition rules, injected into the user
// message (recency effect: instructions closer to the ask are followed more
// reliably). References to 【世界背景】/【人物背景】/【世界详细信息】 point
// at the system message's modules; references to 【物理状态】/
// 【附近NPC】/【物体实时占用】 point at the user message's dynamic segments.
// The available tools are NOT listed here — they arrive via the
// function-calling `tools` request field.
const TacticalRules = `1. 第一个工具调用必须是 speak（用一段话表达此刻内心想法或独白），随后可返回 1-4 个动作段，按执行顺序排列。每段是长复合动作或 InteractSmartObject 长动作，段间用 time_to_stop 控制时长；最后一段以长动作收尾（可不设 time_to_stop，自然持续到时段切换）。
2. 你可以根据当前NPC的实际属性、实际游戏时间等信息灵活安排，如果当前此条日程并不合理（例如半夜不睡觉而是跑步；电量高时去充电；所有对应的smartObject都已经占用等），鼓励安排其他更合理的action去做。
3. 复合动作已包含自动移动到对应位置的逻辑，禁止在复合动作前调用 move_to——直接调用单个长复合动作即可。
4. 仅当目标确实没有匹配的长复合动作时，才用原子动作组合实现目标。禁止把同一动作连续重复多次填充时段（工作段之间应穿插休息段）。
5. InteractSmartObject 和复合动作的 semantic_group 必须严格使用设施详情中给出的 semantic_group 值，禁止编造、禁止用实例 id（如 Charge-1）、禁止拼接 zone/interaction 信息。
6. 复合动作与 semantic_group 必须严格对应，禁止跨类别组合。
   - 补充：所有工种设备都可用 InteractSmartObject 原子动作直接工作——semantic_group 填工作设备、interaction 填对应动词即可（如 加工机 process、调试台 debug、拆解台 dismantle，以及 workbench/assemble、sorting_conveyor/sort_cargo、inspection_table/inspect）；work_shift 只是其中三类工种设备的快捷复合动作，没有复合动作的工种一律用 InteractSmartObject。
7. 长动作可加 time_to_stop 参数（秒）设置该段动作的时长：冥想、整理床铺等单段设 1800 秒左右，不宜超过 1 小时。到点后系统会打断该段并继续执行你返回的后续动作段；只有你返回的动作全部执行完，系统才会再次询问你。推荐模式：工作段（设 time_to_stop，如 1 小时）→ 长椅小憩段（设 time_to_stop，不超过 30 分钟）→ 返回工作段（不设，持续到时段结束）。睡眠等可自然持续到时段切换的动作可不设 time_to_stop。
8. 在work_shift工作时，也可以设置执行时长（例如先工作1小时），允许NPC在工作途中短暂在长椅小憩（不超过30分钟），然后回去继续工作`

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
// KB/world/persona live in the system message (BuildTacticalSystemPrompt).
func BuildTactical(in TacticalInput) string {
	th := BandThresholdsFor(in.Profiles, in.AgentID)

	var sb strings.Builder
	sb.WriteString(`你是小镇居民 NPC 的战术规划模块。你根据系统信息中的【世界背景】【人物背景】【世界详细信息】，以及用户信息中的全天任务与当前时段任务、NPC与环境实时状态、分解规则，把当前时段目标分解为一个或多个 action，按顺序执行。\n`)

	// ── 一、全天任务与当前时段任务 ──
	sb.WriteString("一、全天任务与当前时段任务\n")
	if in.DailyPlan != "" {
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
	nearbyLine := NearbyAgentsLine(in.VisibleAgents)
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
	if os := ObjectStatusContext(in.ObjectStatus, in.NearbyObjects, in.KB); os != "" {
		sb.WriteString(os)
		if !strings.HasSuffix(os, "\n") {
			sb.WriteString("\n")
		}
	}
	hintLine := tacticalHintLine(in, th)
	if hintLine != "" {
		sb.WriteString(hintLine)
		if !strings.HasSuffix(hintLine, "\n") {
			sb.WriteString("\n")
		}
	}

	// ── 三、分解规则 ──
	sb.WriteString("\n三、分解规则\n")
	sb.WriteString(TacticalRules)
	sb.WriteString("\n")

	// ── 四、任务 ──
	sb.WriteString("\n四、任务\n")
	sb.WriteString("请通过工具调用（function calling）把【当前时段目标】分解为动作序列，按顺序执行（首个工具调用是 speak）。\n")
	if ex := TacticalExample(in.KB, in.Goal, in.AgentID); ex != "" {
		sb.WriteString("工具与参数参考（下面每行展示一个工具调用应使用的工具名与参数值，实际请直接调用工具，不要输出文本 JSON）：\n")
		sb.WriteString(ex)
		sb.WriteString("\n")
	}
	return sb.String()
}

// tacticalHintLine renders the replan hint plus, when the hint carries the
// "物理状态告警" marker (set by upgradeIfPhysicalAlert), type-specific
// recovery constraints based on which physical values are actually in alert.
// Pairs with physicalAlertOverrideGoal (code-layer goal override) as double
// insurance. Different alert types drive different recovery actions:
//   - 低电量 → charge_at_station 充电
//   - 高疲劳 → charge_at_station 充电 / rest_at_residence 休息
//   - 高关节磨损 → self_maintenance 维修保养
func tacticalHintLine(in TacticalInput, th BandThresholds) string {
	if in.Hint == "" {
		return ""
	}
	hintLine := "【上次中断原因】" + in.Hint + "（请据此调整本轮规划）"
	if !strings.Contains(in.Hint, "物理状态告警") || in.Physical == nil || in.Physical.IsZero() {
		return hintLine
	}
	var reqs, forbids []string
	if in.Physical.Energy < th.EnergyAlert() {
		reqs = append(reqs, "- 电量过低：必须优先 charge_at_station（充电）补能")
	}
	if in.Physical.Fatigue > th.FatigueAlert() {
		reqs = append(reqs, "- 疲劳过高：优先 charge_at_station（充电）或 rest_at_residence（休息），充电后若仍疲劳追加 rest_at_residence")
	}
	if in.Physical.JointWear > th.JointWearAlert() {
		reqs = append(reqs, "- 关节磨损过高：必须优先 self_maintenance（维护保养），否则持续工作会加剧损耗")
	}
	// 禁止项：仅禁止与所有活跃告警冲突的消耗性动作
	// 关节磨损告警时不禁 self_maintenance（那是需要的恢复动作）
	fatigueAlert := in.Physical.Fatigue > th.FatigueAlert()
	jointWearAlert := in.Physical.JointWear > th.JointWearAlert()
	if fatigueAlert {
		forbids = append(forbids, "work_shift（消耗体力）")
	}
	if jointWearAlert {
		forbids = append(forbids, "surf_internet（无助于恢复）")
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

// TacticalExample constructs the example block dynamically from KB and goal,
// ensuring zone/object IDs in the example are legal in KB and the example
// tool matches the goal semantics.
//
// goal keyword matching (by priority):
//   - 巡视/巡检/巡逻      → move_to + generic_act example
//   - 充电/补能/休息/恢复  → charge_at_station example (find charging object)
//   - 装配/工作/作业/打磨  → work_shift example (find workbench object)
//   - 聊天/社交/对话       → move_to agent + speak example (needs ≥2 agents)
//   - 检查/自检/inspect    → InteractSmartObject inspect example
//
// No keyword match or required resource missing → degrade to default:
// pick first object by category. KB empty → generic example with no concrete IDs.
//
// Key constraint: example's move_to target_id must match the example
// object's ZoneID — otherwise the example itself violates the prompt's
// "InteractSmartObject must be called in object's zone" constraint #5.
func TacticalExample(kb *worldkb.KB, goal, agentID string) string {
	const genericExample = `{"action":"speak","params":{"content":"先去目标区域再开始作业"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"<上方可前往区域的 id>"}}
{"action":"InteractSmartObject","params":{"semantic_group":"<上方可交互物体的 semantic_group>","interaction":"<可用 interaction>"}}`
	if kb == nil {
		return genericExample
	}
	objs := kb.ListObjects()
	zones := kb.ListZones()

	// Goal-specific example first — some branches (e.g. social_chat) only
	// need kb.Agents and don't depend on zones/objs, so must run before the
	// empty-map guard below.
	if ex := exampleForGoal(kb, goal, agentID, zones, objs); ex != "" {
		return ex
	}

	if len(zones) == 0 && len(objs) == 0 {
		return genericExample
	}

	if len(objs) == 0 {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"action":"speak","params":{"content":"先去目标区域再开始作业"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"InteractSmartObject","params":{"semantic_group":"<上方可交互物体的 semantic_group>","interaction":"<可用 interaction>"}}`, exZone)
	}
	obj := objs[0]
	exObj := semanticGroupOf(obj)
	exZone := obj.ZoneID
	if exZone == "" {
		exZone = "<上方可前往区域的 id>"
	}
	switch obj.Category {
	case "workbench", "work":
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		// 多段示例：工作段（设 time_to_stop）→ 长椅小憩段 → 返回工作段（不设，
		// 持续到时段结束）。示范"长时间工作途中休息再回来"的推荐模式。
		return fmt.Sprintf(`{"action":"speak","params":{"content":"先去工作设施干一阵，中途歇口气再继续"}}
{"action":"work_shift","params":{"semantic_group":"%s","interaction":"%s","time_to_stop":3600}}
{"action":"InteractSmartObject","params":{"semantic_group":"bench","interaction":"rest","time_to_stop":1800}}
{"action":"work_shift","params":{"semantic_group":"%s","interaction":"%s"}}`, exObj, verb, exObj, verb)
	case "charging_station", "charging":
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		return fmt.Sprintf(`{"action":"speak","params":{"content":"去充电设施补充能量"}}
{"action":"charge_at_station","params":{"semantic_group":"%s","interaction":"%s"}}`, exObj, verb)
	default:
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		return fmt.Sprintf(`{"action":"speak","params":{"content":"先去目标区域再开始作业"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"InteractSmartObject","params":{"semantic_group":"%s","interaction":"%s"}}`, exZone, exObj, verb)
	}
}

// exampleForGoal returns a goal-specific example; no match or required resource
// missing returns empty string (caller degrades to default).
func exampleForGoal(kb *worldkb.KB, goal, agentID string, zones []worldkb.ZoneInfo, objs []worldkb.ObjectInfo) string {
	if kb == nil || goal == "" {
		return ""
	}
	gl := strings.ToLower(goal)

	// 1. 巡视/巡检/巡逻 → speak + move_to zone + generic_act（新体系无 patrol_zone）
	if containsAny(gl, "巡视", "巡检", "巡逻", "patrol") {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"action":"speak","params":{"content":"去目标区域巡视一圈"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"generic_act","params":{"thought":"巡视设备状态","behavior":"look_around"}}`, exZone)
	}

	// 2. 充电/补能 → speak + charge_at_station
	//    仅匹配明确的"充电"语义，避免把"休息/恢复/疲劳"也路由到充电示例
	//    （P0-2 修复：原分支含"休息/恢复/疲劳"会把"长椅休息"goal 错配到 charge_at_station）。
	if containsAny(gl, "充电", "补能", "charge") {
		if obj := findObjectByCategory(objs, "charging_station", "charging"); obj != nil {
			verb := "<可用 interaction>"
			if len(obj.AvailableInteractions) > 0 {
				verb = obj.AvailableInteractions[0]
			}
			return fmt.Sprintf(`{"action":"speak","params":{"content":"去充电设施补充能量"}}
{"action":"charge_at_station","params":{"semantic_group":"%s","interaction":"%s"}}`, semanticGroupOf(*obj), verb)
		}
	}

	// 2a-2. 冥想 → speak + InteractSmartObject(sleep_pod/meditate)
	//     必须先于 2b（回住所/休眠舱/睡觉）判断：冥想 goal 常含"睡眠舱"
	//     字样，若 2b 先命中会被错误引向 rest_at_residence/sleep。
	//     InteractSmartObject 自带"前往物体+执行互动"，无需 move_to。
	if containsAny(gl, "冥想", "meditate") {
		if obj := findObjectBySemanticGroup(objs, "sleep_pod"); obj != nil {
			return `{"action":"speak","params":{"content":"去舱里静坐一会儿"}}
{"action":"InteractSmartObject","params":{"semantic_group":"sleep_pod","interaction":"meditate"}}`
		}
	}

	// 2a-3. 整理床铺/舱位 → speak + InteractSmartObject(sleep_pod/tidy_up)
	if containsAny(gl, "整理床铺", "整理舱", "整理内务", "tidy") {
		if obj := findObjectBySemanticGroup(objs, "sleep_pod"); obj != nil {
			return `{"action":"speak","params":{"content":"把舱位收拾整齐"}}
{"action":"InteractSmartObject","params":{"semantic_group":"sleep_pod","interaction":"tidy_up"}}`
		}
	}

	// 2a-4. 锻炼/晨练/拉伸 → speak + exercise（原地复合动作，无需 move_to）
	if containsAny(gl, "锻炼", "晨练", "拉伸", "散步", "做操", "运动", "exercise") {
		return `{"action":"speak","params":{"content":"先活动活动筋骨"}}
{"action":"exercise","params":{"exercise_type":"stretch"}}`
	}

	// 2a-5. 上网 → speak + surf_internet（复合动作自带前往电脑的移动）。
	//     关键：禁止在 surf_internet 前加 move_to——实测 LLM 自由发挥输出
	//     speak+move_to+surf_internet 三步结构时，UE 对已到达目标的复合
	//     动作会瞬间返回 success（0.1-1.7s），触发战术层重分解循环+呆站；
	//     两步结构（复合动作自己走路）则正常持续到时段切换。
	if containsAny(gl, "上网", "网上", "浏览", "查资料", "surf") {
		if obj := findObjectBySemanticGroup(objs, "computer"); obj != nil {
			return `{"action":"speak","params":{"content":"去档案馆电脑上查点资料"}}
{"action":"surf_internet","params":{"semantic_group":"computer","interaction":"surf_internet"}}`
		}
	}

	// 2b. 回住所/休眠/睡觉 → speak + rest_at_residence(sleep_pod/sleep)
	//     仅匹配"回舱/休眠/睡觉"语义，rest_at_residence 只能搭配 sleep_pod。
	if containsAny(gl, "回住所", "回休眠", "休眠舱", "睡觉", "睡个", "睡眠", "回舱", "rest_at_residence", "sleep") {
		if obj := findObjectBySemanticGroup(objs, "sleep_pod"); obj != nil {
			return fmt.Sprintf(`{"action":"speak","params":{"content":"回休眠舱好好睡一觉"}}
{"action":"rest_at_residence","params":{"semantic_group":"sleep_pod","interaction":"sleep"}}`)
		}
	}

	// 2c. 长椅/广场休息 → speak + InteractSmartObject(bench/rest)
	//     长椅休息不是"在住所休息"，必须用 InteractSmartObject 原子动作，禁止套用 rest_at_residence。
	if containsAny(gl, "长椅", "广场休息", "短暂休息", "歇会儿", "歇会", "休息", "rest") {
		if obj := findObjectBySemanticGroup(objs, "bench"); obj != nil {
			return fmt.Sprintf(`{"action":"speak","params":{"content":"去中央广场长椅坐会儿"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"central_plaza"}}
{"action":"InteractSmartObject","params":{"semantic_group":"bench","interaction":"rest"}}`)
		}
	}

	// 3. 装配/工作/作业/打磨/加工 → speak + work_shift
	if containsAny(gl, "装配", "工作", "作业", "打磨", "加工", "assemble", "craft") {
		if obj := findObjectByCategory(objs, "workbench", "work"); obj != nil {
			verb := "<可用 interaction>"
			if len(obj.AvailableInteractions) > 0 {
				verb = obj.AvailableInteractions[0]
			}
			return fmt.Sprintf(`{"action":"speak","params":{"content":"去工作设施开始作业"}}
{"action":"work_shift","params":{"semantic_group":"%s","interaction":"%s"}}`, semanticGroupOf(*obj), verb)
		}
	}

	// 4. 聊天/社交/对话 → speak + social_chat（主动找人聊天，走向对方+对话挂起）
	//    peer 优先从 KB 关系中选熟悉度最高的；无关系则回退首个非自身 agent。
	if containsAny(gl, "聊天", "社交", "对话", "chat", "social") && len(kb.Agents) >= 2 {
		peer := pickChatPeer(kb, agentID)
		return fmt.Sprintf(`{"action":"speak","params":{"content":"去找同事聊两句"}}
{"action":"social_chat","params":{"target_agent_id":"%s","content":"最近怎么样？"}}`, peer)
	}

	// 5. 检查/自检/inspect → speak + move_to + InteractSmartObject inspect
	if containsAny(gl, "检查", "自检", "inspect", "examine") {
		for i := range objs {
			for _, v := range objs[i].AvailableInteractions {
				if v == "inspect" {
					exZone := objs[i].ZoneID
					if exZone == "" {
						exZone = "<上方可前往区域的 id>"
					}
					return fmt.Sprintf(`{"action":"speak","params":{"content":"先去目标区域检查设备"}}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"InteractSmartObject","params":{"semantic_group":"%s","interaction":"inspect"}}`, exZone, semanticGroupOf(objs[i]))
				}
			}
		}
	}

	return ""
}

// pickChatPeer chooses a social_chat target for the example prompt. Prefers
// the KB-declared relationship peer with the highest familiarity (so the
// example reflects an existing bond); falls back to the first non-self agent
// in declaration order. Returns "" if fewer than 2 agents exist.
func pickChatPeer(kb *worldkb.KB, selfID string) string {
	if kb == nil || len(kb.Agents) < 2 {
		return ""
	}
	bestFam := -1
	bestPeer := ""
	for _, rel := range kb.Relationships {
		var other string
		switch {
		case rel.From == selfID:
			other = rel.To
		case rel.To == selfID:
			other = rel.From
		default:
			continue
		}
		if rel.Familiarity > bestFam {
			bestFam = rel.Familiarity
			bestPeer = other
		}
	}
	if bestPeer != "" {
		return bestPeer
	}
	// Fallback: first non-self agent in declaration order.
	for _, ag := range kb.Agents {
		if ag.ID != selfID {
			return ag.ID
		}
	}
	return kb.Agents[0].ID
}

// containsAny checks if s contains any of subs (case-insensitivity by caller).
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// findObjectByCategory finds the first object matching any of the category
// aliases. Supports new/old KB schema: UE5 new schema uses "charging"/"work"/
// "rest", old schema uses "charging_station"/"workbench"/"rest_bench".
// Also matches by semantic_group as a fallback (UE5 categories like "Net" or
// "maintainance" don't always align with the prompt's category aliases).
func findObjectByCategory(objs []worldkb.ObjectInfo, categories ...string) *worldkb.ObjectInfo {
	if len(categories) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(categories))
	for _, c := range categories {
		wanted[c] = struct{}{}
	}
	for i := range objs {
		if _, ok := wanted[objs[i].Category]; ok {
			o := objs[i]
			return &o
		}
		if _, ok := wanted[objs[i].SemanticGroup]; ok {
			o := objs[i]
			return &o
		}
	}
	return nil
}

// findObjectBySemanticGroup finds the first object whose semantic_group
// matches any of the given names. Used by exampleForGoal to locate a
// specific semantic_group (e.g. "sleep_pod", "bench") regardless of category.
func findObjectBySemanticGroup(objs []worldkb.ObjectInfo, semanticGroups ...string) *worldkb.ObjectInfo {
	if len(semanticGroups) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(semanticGroups))
	for _, sg := range semanticGroups {
		wanted[sg] = struct{}{}
	}
	for i := range objs {
		if _, ok := wanted[objs[i].SemanticGroup]; ok {
			o := objs[i]
			return &o
		}
	}
	return nil
}

// semanticGroupOf returns the UE5-facing semantic_group value for an object,
// falling back to the instance ID when the field is absent (legacy KB).
// The tactical prompt examples use this so they emit UE5-recognized values
// like "charger" instead of instance IDs like "Charge-1".
func semanticGroupOf(o worldkb.ObjectInfo) string {
	if o.SemanticGroup != "" {
		return o.SemanticGroup
	}
	return o.ID
}
