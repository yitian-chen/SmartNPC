// Package prompt — tactical layer prompt builder.
package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// toolOverride is the hand-tuned Chinese prompt text for the 12 builtin tools.
// Keys are tool_name (snake_case). New cmds pushed via capability_registry
// derive Desc/Params from CapabilityAction.Description and param schema
// (see toolEntries).
//
// 新 12 cmd 体系（2026-08-11）：MoveTo 统一替换 MoveToLocation+MoveToAgent，
// 用 target_type+target_id/target_position；InteractSmartObject 参数名从
// target_object_id 改为 smart_object；5 个复合动作共享 smart_object+interaction。
var toolOverride = map[string]struct {
	Desc   string
	Params string
}{
	"generic_act": {"兜底通用动作（带内心独白，无匹配复合动作时用）", `{"thought":"...","behavior":"idle|wave_hand|look_around"}`},
	"move_to":     {"移动到目标", `{"target_type":"agent|smart_object|zone|position","target_id":"...","target_position":[x,y,z]}`},
	"turn_to":     {"转向目标", `{"target_type":"agent|smart_object|zone|position","target_id":"...","target_position":[x,y,z]}`},
	"speak":       {"说话", `{"content":"..."}`},
	"emote":       {"表达情绪", `{"emotion":"happy|sad|..."}`},
	"interact":    {"与智能物体交互", `{"smart_object":"...","interaction":"动词"}`},
	// wait intentionally not in override and not shown in prompt tool list.
	"work_shift":        {"工作班次（装配/作业）", `{"smart_object":"工作台id","interaction":"动词"}`},
	"charge_at_station": {"在充电站充电", `{"smart_object":"充电站id","interaction":"动词"}`},
	"self_maintenance":  {"自我维护保养", `{"smart_object":"维护台id","interaction":"动词"}`},
	"rest_at_residence": {"在住所休息", `{"smart_object":"休眠舱id","interaction":"动词"}`},
	"surf_internet":     {"上网浏览", `{"smart_object":"终端id","interaction":"动词"}`},
}

// tacticalPromptBody is the prompt's fixed skeleton. %s placeholders are:
// goal / zone / timeOfDay /
// physicalLine (physical state line, empty when all-0) / roleLine / memoriesLine /
// relationshipsLine /
// hintLine / slotDurationHint / kbContext / toolCount / toolList / exampleBlock.
const tacticalPromptBody = `[战术层/任务分解] 当前时段目标：%s
你目前在：%s，游戏时间 %s。
%s
%s
%s
%s
请把这个目标分解为一个或多个 action，按顺序执行。
%s
%s
%s
可用工具（仅限以下 %d 个）。工具分两类：
- 复合动作（标记 [复合]）：长耗时、单步即可完成一段工作（如装配、充电、巡逻、聊天），会自动移动到对应位置，无需自己调用 move_to。若目标语义与某复合动作匹配，应优先使用复合动作。
- 原子动作（标记 [原子]）：短耗时、作为基本 building block（如移动、说话、等待、交互）。仅当复合动作无法覆盖 schedule 要求时，才用 2-5 个原子动作组合实现。
%s

要求：
1. 第一行输出 {"inner_thought":"一句话内心独白"}
2. 后续每行输出一个 {"action":"工具名","params":{...}}，按执行顺序排列
3. 队列必须以长复合动作（标记 [复合]）结尾——长复合动作会持续执行直到时段切换，让 NPC 一直工作到下一 schedule 节点被 worker 主动打断
4. 禁止输出 wait 动作；若无需移动/转身等前置步骤，可直接输出单个长复合动作，长复合动作包含移动到对应位置的逻辑
5. 仅当目标确实没有匹配的长复合动作时（极少见），才用原子动作组合、结合调用兜底的 generic_act 通用动作实现目标
6. move_to/turn_to 的 target_id、interact 和复合动作的 smart_object 必须严格使用上面"可前往区域"和"可交互物体"中给出的 id，禁止编造、禁止拼接 zone/interaction 信息
7. 每行一个 JSON 对象，不要输出 JSON 数组，不要输出 markdown 围栏，不要输出任何其他文字
8. 必须以字符 {"inner_thought 开头，不要输出步骤说明、不要解释、不要编号列表、不要 markdown 加粗

示例（id 来自上方可用列表，不可照抄示例中的 id）：
%s`

// BuildTactical fills the tactical layer prompt template.
// kb injects available zone/object lists + NPC role (AgentRole), preventing
// the LLM from fabricating IDs and letting decomposition reflect personality.
// slot ("HH:MM-HH:MM") is used to hint slot duration, guiding the LLM to
// produce steps whose total duration approaches the slot length.
// actions (from registry.EffectiveActions) drives the tool list; nil → builtin fallback.
func BuildTactical(in TacticalInput) string {
	physicalLine := ""
	if in.Physical != nil && !in.Physical.IsZero() {
		physicalLine = fmt.Sprintf("物理状态：能量 %.0f、疲劳 %.0f、关节磨损 %.0f、健康 %.0f。", in.Physical.Energy, in.Physical.Fatigue, in.Physical.JointWear, in.Physical.Health)
	}
	roleLine := ""
	if role := AgentRole(in.KB, in.AgentID); role != "" {
		roleLine = "【你的角色】\n" + role
	}
	memoriesLine := ""
	if in.Memories != "" {
		memoriesLine = "【过往经验】\n" + in.Memories
	}
	relationshipsLine := ""
	if in.Relationships != "" {
		relationshipsLine = "【人际关系】\n" + in.Relationships
	}
	hintLine := ""
	if in.Hint != "" {
		hintLine = "【上次中断原因】" + in.Hint + "（请据此调整本轮规划）"
	}
	// 物理告警强约束段: when hint contains "物理状态告警" marker (set by
	// upgradeIfPhysicalAlert), insert explicit prohibition — forbid work
	// actions, require recovery actions. Pairs with physicalAlertOverrideGoal
	// (code-layer goal override) as double insurance.
	if strings.Contains(in.Hint, "物理状态告警") {
		hintLine += "\n【物理告警强制约束】当前物理状态已突破警戒阈值，必须立即规划恢复类动作：\n" +
			"- 优先 charge_at_station（充电）补能\n" +
			"- 充电后若仍疲劳高，追加 wait 或 rest_at_residence 类动作\n" +
			"- 禁止规划 work_shift / self_maintenance / surf_internet 等消耗体能的动作\n" +
			"- 禁止规划 move_to 到非充电站区域（除非当前已在充电站）"
	}
	toolList, toolCount := BuildTacticalToolList(in.Actions)
	return fmt.Sprintf(tacticalPromptBody, in.Goal, in.Zone, in.TimeOfDay,
		physicalLine,
		roleLine,
		memoriesLine,
		relationshipsLine,
		hintLine, SlotDurationHint(in.Slot, in.TimeOfDay), KBContext(in.KB), toolCount, toolList,
		TacticalExample(in.KB, in.Goal))
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

// BuildTacticalToolList builds the tool bullet list for the prompt from
// capability actions. Returns (joined bullet text, tool count).
// Each line carries a [复合]/[原子] label for composite/atomic distinction.
// actions == nil → falls back to builtin tools (backward compat).
func BuildTacticalToolList(actions []protocol.CapabilityAction) (string, int) {
	entries := ToolEntries(actions)
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("- %s [%s]: %s。params: %s", e.Name, kindLabel(e.Kind), e.Desc, e.Params))
	}
	return strings.Join(lines, "\n"), len(entries)
}

// kindLabel maps Kind to the Chinese label; empty defaults to "原子"
// (when new cmds are pushed without Kind, conservatively treat as atomic).
func kindLabel(kind string) string {
	if kind == "composite" {
		return "复合"
	}
	return "原子"
}

// builtinToolKind returns the Kind ("atomic" | "composite") for builtin tools.
// Used when actions == nil (backward-compat scenario) to infer Kind from name.
func builtinToolKind(name string) string {
	switch name {
	case "work_shift", "charge_at_station", "self_maintenance",
		"rest_at_residence", "surf_internet":
		return "composite"
	default:
		return "atomic"
	}
}

// ToolEntries constructs the intermediate tool list representation.
//
// actions != nil: derives from EffectiveActions. Builtin tools' Desc/Params
// use toolOverride; new cmds derive from CapabilityAction.Description and
// param schema.
//
// actions == nil: falls back to BuiltinToolSpecs (minus scan_area/stop/wait),
// all using toolOverride text.
func ToolEntries(actions []protocol.CapabilityAction) []ToolEntry {
	if actions == nil {
		specs := tools.BuiltinToolSpecs()
		out := make([]ToolEntry, 0, len(specs))
		for _, spec := range specs {
			if spec.Name == "scan_area" || spec.Name == "stop" || spec.Name == "wait" {
				continue
			}
			entry := ToolEntry{Name: spec.Name, RequiredCmd: spec.RequiredCmd, Kind: builtinToolKind(spec.Name)}
			if ov, ok := toolOverride[spec.Name]; ok {
				entry.Desc, entry.Params = ov.Desc, ov.Params
			} else {
				entry.Desc, entry.Params = spec.Name, "{}"
			}
			out = append(out, entry)
		}
		return out
	}
	out := make([]ToolEntry, 0, len(actions))
	for _, act := range actions {
		name := tools.CmdToToolName(act.Cmd)
		if name == "scan_area" || name == "stop" || name == "wait" {
			continue
		}
		kind := act.Kind
		if kind == "" {
			kind = builtinToolKind(name)
		}
		entry := ToolEntry{Name: name, RequiredCmd: act.Cmd, Kind: kind}
		if ov, ok := toolOverride[name]; ok {
			entry.Desc, entry.Params = ov.Desc, ov.Params
		} else {
			entry.Desc = act.Description
			if entry.Desc == "" {
				entry.Desc = name
			}
			entry.Params = toolParamHint(act.Params)
		}
		out = append(out, entry)
	}
	return out
}

// toolParamHint derives the prompt params example text from CapabilityParam list.
// e.g. [{Name:"target_object_id",Type:"string",Required:true},{Name:"duration_sec",Type:"number"}]
// → `{"target_object_id":"...","duration_sec":秒数}`
func toolParamHint(params []protocol.CapabilityParam) string {
	if len(params) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, fmt.Sprintf("%s:%s", p.Name, paramPlaceholder(p)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// paramPlaceholder returns the prompt placeholder text by CapabilityParam.Type.
func paramPlaceholder(p protocol.CapabilityParam) string {
	switch p.Type {
	case "number":
		return "秒数"
	case "bool":
		return "true|false"
	case "vector":
		return "[x,y,z]"
	case "enum":
		if len(p.EnumValues) > 0 {
			return strings.Join(p.EnumValues, "|")
		}
		return "..."
	default:
		return "..."
	}
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
//   - 检查/自检/inspect    → interact inspect example
//
// No keyword match or required resource missing → degrade to default:
// pick first object by category. KB empty → generic example with no concrete IDs.
//
// Key constraint: example's move_to target_id must match the example
// object's ZoneID — otherwise the example itself violates the prompt's
// "interact must be called in object's zone" constraint #5.
func TacticalExample(kb *worldkb.KB, goal string) string {
	const genericExample = `{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to","params":{"target_type":"zone","target_id":"<上方可前往区域的 id>"}}
{"action":"interact","params":{"smart_object":"<上方可交互物体的 id>","interaction":"<可用 interaction>"}}`
	if kb == nil {
		return genericExample
	}
	objs := kb.ListObjects()
	zones := kb.ListZones()
	if len(zones) == 0 && len(objs) == 0 {
		return genericExample
	}

	if ex := exampleForGoal(kb, goal, zones, objs); ex != "" {
		return ex
	}

	if len(objs) == 0 {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"interact","params":{"smart_object":"<上方可交互物体的 id>","interaction":"<可用 interaction>"}}`, exZone)
	}
	obj := objs[0]
	exObj := obj.ID
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
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"work_shift","params":{"smart_object":"%s","interaction":"%s"}}`, exZone, exObj, verb)
	case "charging_station", "charging":
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域补充能量"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"charge_at_station","params":{"smart_object":"%s","interaction":"%s"}}`, exZone, exObj, verb)
	default:
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"interact","params":{"smart_object":"%s","interaction":"%s"}}`, exZone, exObj, verb)
	}
}

// exampleForGoal returns a goal-specific example; no match or required resource
// missing returns empty string (caller degrades to default).
func exampleForGoal(kb *worldkb.KB, goal string, zones []worldkb.ZoneInfo, objs []worldkb.ObjectInfo) string {
	if kb == nil || goal == "" {
		return ""
	}
	gl := strings.ToLower(goal)

	// 1. 巡视/巡检/巡逻 → move_to zone + generic_act（新体系无 patrol_zone）
	if containsAny(gl, "巡视", "巡检", "巡逻", "patrol") {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"inner_thought":"去目标区域巡视一圈"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"generic_act","params":{"thought":"巡视设备状态","behavior":"look_around"}}`, exZone)
	}

	// 2. 充电/补能/休息/恢复/疲劳 → charge_at_station
	if containsAny(gl, "充电", "补能", "休息", "恢复", "疲劳", "charge", "rest") {
		if obj := findObjectByCategory(objs, "charging_station", "charging"); obj != nil {
			exZone := obj.ZoneID
			if exZone == "" {
				exZone = "<上方可前往区域的 id>"
			}
			verb := "<可用 interaction>"
			if len(obj.AvailableInteractions) > 0 {
				verb = obj.AvailableInteractions[0]
			}
			return fmt.Sprintf(`{"inner_thought":"先去目标区域补充能量"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"charge_at_station","params":{"smart_object":"%s","interaction":"%s"}}`, exZone, obj.ID, verb)
		}
	}

	// 3. 装配/工作/作业/打磨/加工 → work_shift
	if containsAny(gl, "装配", "工作", "作业", "打磨", "加工", "assemble", "craft") {
		if obj := findObjectByCategory(objs, "workbench", "work"); obj != nil {
			exZone := obj.ZoneID
			if exZone == "" {
				exZone = "<上方可前往区域的 id>"
			}
			verb := "<可用 interaction>"
			if len(obj.AvailableInteractions) > 0 {
				verb = obj.AvailableInteractions[0]
			}
			return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"work_shift","params":{"smart_object":"%s","interaction":"%s"}}`, exZone, obj.ID, verb)
		}
	}

	// 4. 聊天/社交/对话 → move_to agent + speak（新体系无 chat_with）
	if containsAny(gl, "聊天", "社交", "对话", "chat", "social") && len(kb.Agents) >= 2 {
		other := kb.Agents[1].ID
		return fmt.Sprintf(`{"inner_thought":"去找同事聊两句"}
{"action":"move_to","params":{"target_type":"agent","target_id":"%s"}}
{"action":"speak","params":{"content":"最近工作怎么样？"}}`, other)
	}

	// 5. 检查/自检/inspect → interact inspect
	if containsAny(gl, "检查", "自检", "inspect", "examine") {
		for i := range objs {
			for _, v := range objs[i].AvailableInteractions {
				if v == "inspect" {
					exZone := objs[i].ZoneID
					if exZone == "" {
						exZone = "<上方可前往区域的 id>"
					}
					return fmt.Sprintf(`{"inner_thought":"先去目标区域检查设备"}
{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}
{"action":"interact","params":{"smart_object":"%s","interaction":"inspect"}}`, exZone, objs[i].ID)
				}
			}
		}
	}

	return ""
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
	}
	return nil
}
