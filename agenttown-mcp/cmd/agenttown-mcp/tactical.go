package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// plannedAction 是战术层分解出的单步 action，对应一个 MCP 工具调用。
type plannedAction struct {
	Action string         `json:"action"` // 工具名：work_at_workbench / move_to_location / ...
	Params map[string]any `json:"params"` // 工具参数（LLM 原样输出，duration_sec 等未换算）
}

// ndjsonLine 是战术层 NDJSON 输出的单行判别联合体：要么是 inner_thought，要么是一个 action。
type ndjsonLine struct {
	InnerThought string         `json:"inner_thought,omitempty"`
	Action       string         `json:"action,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
}

// actionSource 标识一个在途 action 由哪一层下发，决定 completion 后的路由。
type actionSource string

const (
	sourceTool     actionSource = "mcp_tool"
	sourceTactical actionSource = "tactical"
)

// tacticalToolEntry 是战术层 prompt 工具列表段的单条目，由 buildTacticalToolEntries
// 产出。Name 是 LLM 看到的工具名（snake_case），RequiredCmd 是对应 UE cmd
// （PascalCase），Desc/Params 是 prompt 中展示的中文描述与 params 示例。
// Kind 取 "atomic" 或 "composite"，决定 prompt 中的 [原子]/[复合] 标签——
// 战术层 prompt 据此引导 LLM 优先使用复合动作（见 tacticalPromptBody）。
type tacticalToolEntry struct {
	Name        string
	RequiredCmd string
	Kind        string // "atomic" | "composite"
	Desc        string
	Params      string
}

// tacticalToolOverride 是内置 14 工具的 prompt 文案覆盖表（hand-tuned 中文
// 描述与 params 示例）。键为 tool_name（snake_case）。
//
// UE 通过 capability_registry 新推送的 cmd 不在此表内：其 Desc/Params 由
// CapabilityAction.Description 与参数 schema 派生（见 buildTacticalToolEntries）。
// 内置工具保留人工调优的中文文案，确保 prompt 质量稳定；新 cmd 文案质量
// 依赖 UE 推送的 description 字段，但功能上不受影响。
var tacticalToolOverride = map[string]struct {
	Desc   string
	Params string
}{
	"move_to_location":  {"移动到目标位置", `{"target":"区域或位置id"}`},
	"move_to_agent":     {"跟随目标agent", `{"target_agent_id":"...","speed":"walk|run"}`},
	"turn_to":           {"转向目标", `{"target_agent_id":"实体id"} 或 {"direction":[dx,dy,dz]}`},
	"play_montage":      {"播放蒙太奇", `{"montage_id":"...","wait_finish":true}`},
	"speak":             {"说话", `{"content":"...","target_agent_id":"目标agent_id（可空）"}`},
	"emote":             {"表达情绪", `{"emotion":"happy|sad|...","mode":"oneshot|sustained"}`},
	"interact":          {"与智能物体交互", `{"target_object_id":"...","interaction":"动词"}`},
	// wait 故意不在 override 中且不在 prompt 工具列表展示：长复合动作应持续到
	// 时段切换由 advanceSlotIfNeeded 打断，短动作队列空时由 tacticalRefill
	// 重新分解。wait 工具 struct 保留以兼容反应层等其他调用点。
	"work_at_workbench": {"在工作台装配", `{"target_object_id":"工作台id","duration_sec":秒数}`},
	"work_at_workshop":  {"车间例行工作", `{}`},
	"chat_with":         {"与其他agent聊天", `{"target_agent_id":"...","topic":"话题（可选）"}`},
	"repair_target":     {"修理其他agent", `{"target_agent_id":"...","tool_id":"工具id（可选）"}`},
	// charge_at_station 故意不在 override 中：其 schema 在 mock UE（target_object_id）
	// 与真实 UE5（smart_object + interaction）之间分歧，让 buildTacticalToolEntries
	// 走 buildToolParamHint(act.Params) 按 capability_registry 动态派生，避免
	// 硬编码某一侧 schema 导致另一侧参数名不匹配。
	"patrol_zone":       {"巡逻区域", `{"target_zone":"区域id","duration_sec":秒数}`},
}

// tacticalActionAvailable 判断 action 是否为战术层可用工具，且其依赖的
// cmd 在 registry 中对 agentID 有效。registry == nil 时降级为仅检查是否
// 内置战术工具（向后兼容测试与未启用 capability 的场景）。
//
// scan_area / stop 不属于战术层排队工具，无论 registry 是否 nil 都返回 false。
// wait 同样返回 false：长复合动作应持续到时段切换由 advanceSlotIfNeeded
// 打断，队列空时由 tacticalRefill 重新分解，不应输出 wait。
func tacticalActionAvailable(action, agentID string, registry *CapabilityRegistry) bool {
	if action == "wait" {
		return false
	}
	if registry == nil {
		for _, spec := range tools.BuiltinToolSpecs() {
			if spec.Name == action && spec.Name != "scan_area" && spec.Name != "stop" {
				return true
			}
		}
		return false
	}
	for _, act := range registry.EffectiveActions(agentID) {
		if tools.CmdToToolName(act.Cmd) == action {
			return true
		}
	}
	return false
}

// buildTacticalToolList 按 registry 对 agentID 的有效能力集构造 prompt 中的
// 工具 bullet 列表。registry == nil 时降级为全量内置工具（向后兼容）。
// 返回 (拼接好的 bullet 段, 可用工具数)。每行附 [复合]/[原子] 标签，便于
// LLM 区分长耗时复合动作与短耗时原子动作，配合 prompt 的复合优先策略。
func buildTacticalToolList(agentID string, registry *CapabilityRegistry) (string, int) {
	entries := buildTacticalToolEntries(agentID, registry)
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("- %s [%s]: %s。params: %s", e.Name, kindLabel(e.Kind), e.Desc, e.Params))
	}
	return strings.Join(lines, "\n"), len(entries)
}

// kindLabel 把 Kind 字段映射为 prompt 中的中文标签；空值默认按"原子"处理
// （新 cmd 推送时若未填 Kind，保守视为原子动作）。
func kindLabel(kind string) string {
	if kind == "composite" {
		return "复合"
	}
	return "原子"
}

// builtinToolKind 返回内置工具的 Kind（"atomic" | "composite"）。
// 用于 registry == nil 时（向后兼容场景）从工具名推断 Kind。
// 6 个复合工具硬编码列表与 BuiltinToolSpecs 中的 composite 段一致。
func builtinToolKind(name string) string {
	switch name {
	case "work_at_workbench", "work_at_workshop", "chat_with",
		"repair_target", "charge_at_station", "patrol_zone":
		return "composite"
	default:
		return "atomic"
	}
}

// buildTacticalToolEntries 构造战术层工具列表的中间表示。
//
// registry != nil 时从 EffectiveActions(agentID) 派生：内置工具的 Desc/Params
// 走 tacticalToolOverride 覆盖文案，新 cmd 从 CapabilityAction.Description
// 与参数 schema 派生。
//
// registry == nil 时降级为 BuiltinToolSpecs 全量（去掉 scan_area/stop），
// 文案一律走 tacticalToolOverride（覆盖文案对内置工具必存在）。
func buildTacticalToolEntries(agentID string, registry *CapabilityRegistry) []tacticalToolEntry {
	if registry == nil {
		specs := tools.BuiltinToolSpecs()
		out := make([]tacticalToolEntry, 0, len(specs))
		for _, spec := range specs {
			if spec.Name == "scan_area" || spec.Name == "stop" || spec.Name == "wait" {
				continue
			}
			entry := tacticalToolEntry{Name: spec.Name, RequiredCmd: spec.RequiredCmd, Kind: builtinToolKind(spec.Name)}
			if ov, ok := tacticalToolOverride[spec.Name]; ok {
				entry.Desc, entry.Params = ov.Desc, ov.Params
			} else {
				// 防御性：覆盖表遗漏时降级为工具名 + {}（不应到达，覆盖表应齐全）
				entry.Desc, entry.Params = spec.Name, "{}"
			}
			out = append(out, entry)
		}
		return out
	}
	actions := registry.EffectiveActions(agentID)
	out := make([]tacticalToolEntry, 0, len(actions))
	for _, act := range actions {
		name := tools.CmdToToolName(act.Cmd)
		if name == "scan_area" || name == "stop" || name == "wait" {
			continue
		}
		// act.Kind 通常由 capability_registry 显式给出；空值默认 "atomic"，
		// 与 kindLabel 的容错保持一致。
		kind := act.Kind
		if kind == "" {
			kind = builtinToolKind(name)
		}
		entry := tacticalToolEntry{Name: name, RequiredCmd: act.Cmd, Kind: kind}
		if ov, ok := tacticalToolOverride[name]; ok {
			entry.Desc, entry.Params = ov.Desc, ov.Params
		} else {
			entry.Desc = act.Description
			if entry.Desc == "" {
				entry.Desc = name
			}
			entry.Params = buildToolParamHint(act.Params)
		}
		out = append(out, entry)
	}
	return out
}

// buildToolParamHint 从 CapabilityParam 列表派生 prompt 中的 params 示例文本。
// 例：[{Name:"target_object_id",Type:"string",Required:true},{Name:"duration_sec",Type:"number"}]
// → `{"target_object_id":"...","duration_sec":秒数}`
func buildToolParamHint(params []protocol.CapabilityParam) string {
	if len(params) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, fmt.Sprintf("%s:%s", p.Name, paramPlaceholder(p)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// paramPlaceholder 按 CapabilityParam.Type 返回 prompt 占位符文本。
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

// tacticalPromptBody 是 prompt 的固定骨架，%s 占位符依次为：
// goal / zone / timeOfDay /
// physicalLine（物理状态行，全 0 时为空串）/ roleLine / hintLine /
// slotDurationHint / kbContext / toolCount / toolList / exampleBlock。
// roleLine 由 buildAgentRoleContext(kb, agentID) 生成（含【你的角色】标题），
// kb==nil 或 agent 不存在时为空串。exampleBlock 由 buildTacticalExample
// 动态从 KB 取合法 id 生成，避免示例本身编造 KB 外 id（旧版示例写死
// main_workshop / workbench_01 诱导 LLM 跟随编造）。
//
// 复合优先策略（2026-08）：工具分复合/原子两类，prompt 引导 LLM 优先使用
// 复合动作——若目标语义匹配某复合动作（如"装配"→work_at_workbench、"充电"
// →charge_at_station），输出 1-2 步即可（通常 move_to_location + 1 个复合
// 动作；若已在目标 zone 可直接 1 个复合动作）。仅当没有匹配的复合动作时，
// 才用 2-5 个原子动作组合实现目标。这降低了输出 token、减少 schema 漂移，
// 也让队列总时长更接近 slot 时长（复合动作本身即覆盖整段工作时间）。
const tacticalPromptBody = `[战术层/任务分解] 当前时段目标：%s
你目前在：%s，游戏时间 %s。
%s
%s
请把这个目标分解为一个或多个 action，按顺序执行。
%s
%s
%s
可用工具（仅限以下 %d 个）。工具分两类：
- 复合动作（标记 [复合]）：长耗时、单步即可完成一段工作（如装配、充电、巡逻、聊天）。若目标语义与某复合动作匹配，应优先使用复合动作。
- 原子动作（标记 [原子]）：短耗时、作为基本 building block（如移动、说话、等待、交互）。仅当复合动作无法覆盖 schedule 要求时，才用 2-5 个原子动作组合实现。
%s

要求：
1. 第一行输出 {"inner_thought":"一句话内心独白"}
2. 后续每行输出一个 {"action":"工具名","params":{...}}，按执行顺序排列
3. 队列必须以长复合动作（标记 [复合]）结尾——长复合动作会持续执行直到时段切换，让 NPC 一直工作到下一 schedule 节点被 worker 主动打断
4. 禁止输出 wait 动作；若无需移动/转身等前置步骤，可直接输出单个长复合动作
5. 仅当目标确实没有匹配的长复合动作时（极少见），才用 2-5 个原子动作组合实现目标，但仍禁止以 wait 结尾
6. move_to 的 target、interact 的 target_object_id、work_at_workbench 的 target_object_id、patrol_zone 的 target_zone 必须严格使用上面"可前往区域"和"可交互物体"中给出的 id，禁止编造、禁止拼接 zone/interaction 信息
7. 每行一个 JSON 对象，不要输出 JSON 数组，不要输出 markdown 围栏，不要输出任何其他文字
8. 必须以字符 {"inner_thought 开头，不要输出步骤说明、不要解释、不要编号列表、不要 markdown 加粗

示例（id 来自上方可用列表，不可照抄示例中的 id）：
%s`

// buildTacticalExample 根据当前 KB 和 goal 动态构造示例，确保示例中出现的
// zone id / object id 都在 KB 中合法存在，且示例工具与 goal 语义匹配。
//
// goal 关键词优先匹配（按优先级）：
//   - 巡视/巡检/巡逻      → patrol_zone 示例
//   - 充电/补能/休息/恢复  → charge_at_station 示例（找 charging_station object）
//   - 装配/工作/作业/打磨  → work_at_workbench 示例（找 workbench object）
//   - 聊天/社交/对话       → chat_with 示例（需 KB 有 ≥2 个 agent）
//   - 检查/自检/inspect    → interact inspect 示例
//
// 关键词未命中或对应工具/object 不存在时，降级到旧逻辑：按首个 object 的
// category 选示例（workbench→work_at_workbench，charging_station→charge_at_station，
// 其他→interact）。KB 为空时返回不引用任何具体 id 的通用示例。
//
// 关键约束：示例中的 move_to_location target 必须与示例 object 的 ZoneID
// 一致——否则示例本身就违反 prompt 中"interact 必须在 object 所在 zone 调用"
// 的约束 #5，LLM 会模仿错误模式（先 move 到任意 zone，再调用任意 object）。
// 因此有 object 时 exZone 取 obj.ZoneID 而非 ListZones()[0]。
//
// 动机（P0-2 修复）：旧版 buildTacticalExample(kb) 总是用首个 object（当前
// KB 首个是 charging_station_01），导致无论 goal 是"自检关节"还是"装配作业"，
// 示例都是"去充电"，LLM 模仿后把 goal 和示例机械拼接（如"巡视车间并记录日志，
// 最后去充电"）。
func buildTacticalExample(kb *worldkb.KB, goal string) string {
	const genericExample = `{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"<上方可前往区域的 id>"}}
{"action":"interact","params":{"target_object_id":"<上方可交互物体的 id>","interaction":"<可用 interaction>"}}`
	if kb == nil {
		return genericExample
	}
	objs := kb.ListObjects()
	zones := kb.ListZones()
	if len(zones) == 0 && len(objs) == 0 {
		return genericExample
	}

	// goal 关键词匹配（按优先级）。命中后若所需 object/agent 不存在则继续往下尝试。
	if ex := exampleForGoal(kb, goal, zones, objs); ex != "" {
		return ex
	}

	// 默认降级：无 object 时用第一个 zone（或占位符）作示例 target。
	if len(objs) == 0 {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"interact","params":{"target_object_id":"<上方可交互物体的 id>","interaction":"<可用 interaction>"}}`, exZone)
	}
	obj := objs[0]
	exObj := obj.ID
	// 有 object 时示例 zone 必须取 obj.ZoneID，保证示例 zone-object 配对正确。
	// obj.ZoneID 为空（KB 数据异常）时降级为占位符，避免拼出错配的 zone id。
	exZone := obj.ZoneID
	if exZone == "" {
		exZone = "<上方可前往区域的 id>"
	}
	// 按 category 选示例工具，避免 work_at_workbench 配 charging_station 的错配。
	switch obj.Category {
	case "workbench":
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"work_at_workbench","params":{"target_object_id":"%s","duration_sec":3600}}`, exZone, exObj)
	case "charging_station":
		return fmt.Sprintf(`{"inner_thought":"先去目标区域补充能量"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"charge_at_station","params":{"target_object_id":"%s"}}`, exZone, exObj)
	default:
		// rest_bench 或未知 category：用 interact + 第一个可用 interaction。
		verb := "<可用 interaction>"
		if len(obj.AvailableInteractions) > 0 {
			verb = obj.AvailableInteractions[0]
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"interact","params":{"target_object_id":"%s","interaction":"%s"}}`, exZone, exObj, verb)
	}
}

// exampleForGoal 按 goal 关键词匹配返回对应示例；未命中或所需资源不存在返回空串，
// 由调用方降级到默认示例。zones/objs 参数避免重复 KB 查询。
func exampleForGoal(kb *worldkb.KB, goal string, zones []worldkb.ZoneInfo, objs []worldkb.ObjectInfo) string {
	if kb == nil || goal == "" {
		return ""
	}
	gl := strings.ToLower(goal)

	// 1. 巡视/巡检/巡逻 → patrol_zone（用第一个 zone）
	if containsAny(gl, "巡视", "巡检", "巡逻", "patrol") {
		exZone := "<上方可前往区域的 id>"
		if len(zones) > 0 {
			exZone = zones[0].ID
		}
		return fmt.Sprintf(`{"inner_thought":"先去目标区域巡视一圈"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"patrol_zone","params":{"target_zone":"%s","duration_sec":1800}}`, exZone, exZone)
	}

	// 2. 充电/补能/休息/恢复/疲劳 → charge_at_station（找 charging_station object）
	if containsAny(gl, "充电", "补能", "休息", "恢复", "疲劳", "charge", "rest") {
		if obj := findObjectByCategory(objs, "charging_station"); obj != nil {
			exZone := obj.ZoneID
			if exZone == "" {
				exZone = "<上方可前往区域的 id>"
			}
			return fmt.Sprintf(`{"inner_thought":"先去目标区域补充能量"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"charge_at_station","params":{"target_object_id":"%s"}}`, exZone, obj.ID)
		}
	}

	// 3. 装配/工作/作业/打磨/加工 → work_at_workbench（找 workbench object）
	if containsAny(gl, "装配", "工作", "作业", "打磨", "加工", "assemble", "craft") {
		if obj := findObjectByCategory(objs, "workbench"); obj != nil {
			exZone := obj.ZoneID
			if exZone == "" {
				exZone = "<上方可前往区域的 id>"
			}
			return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"work_at_workbench","params":{"target_object_id":"%s","duration_sec":3600}}`, exZone, obj.ID)
		}
	}

	// 4. 聊天/社交/对话 → chat_with（需 KB 有 ≥2 个 agent，排除 self）
	//    当前 KB 仅 1 个 agent，此分支不会命中，降级到默认示例。
	if containsAny(gl, "聊天", "社交", "对话", "chat", "social") && len(kb.Agents) >= 2 {
		other := kb.Agents[1].ID // 简化：取第二个 agent（首个常是 self）
		return fmt.Sprintf(`{"inner_thought":"去找同事聊两句"}
{"action":"move_to_agent","params":{"target_agent_id":"%s","speed":"walk"}}
{"action":"chat_with","params":{"target_agent_id":"%s","topic":"工作"}}`, other, other)
	}

	// 5. 检查/自检/inspect → interact inspect（找有 inspect interaction 的 object）
	if containsAny(gl, "检查", "自检", "inspect", "examine") {
		for i := range objs {
			for _, v := range objs[i].AvailableInteractions {
				if v == "inspect" {
					exZone := objs[i].ZoneID
					if exZone == "" {
						exZone = "<上方可前往区域的 id>"
					}
					return fmt.Sprintf(`{"inner_thought":"先去目标区域检查设备"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"interact","params":{"target_object_id":"%s","interaction":"inspect"}}`, exZone, objs[i].ID)
				}
			}
		}
	}

	return ""
}

// containsAny 检查 s 是否包含 subs 中任一子串（大小写不敏感由调用方保证）。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// findObjectByCategory 在 ObjectInfo 列表中查找首个指定 category 的对象。
// 返回指针便于调用方判空；找不到返回 nil。
func findObjectByCategory(objs []worldkb.ObjectInfo, category string) *worldkb.ObjectInfo {
	for i := range objs {
		if objs[i].Category == category {
			o := objs[i] // 拷贝避免 range 复用
			return &o
		}
	}
	return nil
}

// buildTacticalPrompt 填充战术层 prompt 模板。kb 用于注入可用 zone/location/object
// 列表，避免 LLM 编造不存在的 ID（如 workbench_02、archives）。
// slot 形如 "HH:MM-HH:MM"，用于在 prompt 里提示当前时段时长，引导 LLM
// 给出总时长接近 slot 时长的步骤，减少队列提前耗尽导致的重分解。
// slot 为空或解析失败时该提示行降级为空，保持旧行为。
//
// registry 非 nil 时，工具列表段按 registry 对 agentID 的有效能力集动态
// 生成（per-agent 覆盖全局默认）；nil 时降级为全量内置工具（向后兼容）。
// buildTacticalPrompt 填充战术层 prompt 模板。kb 用于注入可用 zone/location/object
// 列表与 NPC 角色设定（buildAgentRoleContext），避免 LLM 编造不存在的 ID（如
// workbench_02、archives）并让分解体现角色性格（如"沉稳"→先检查再开工）。
// slot 形如 "HH:MM-HH:MM"，用于在 prompt 里提示当前时段时长，引导 LLM
// 给出总时长接近 slot 时长的步骤，减少队列提前耗尽导致的重分解。
// slot 为空或解析失败时该提示行降级为空，保持旧行为。
//
// registry 非 nil 时，工具列表段按 registry 对 agentID 的有效能力集动态
// 生成（per-agent 覆盖全局默认）；nil 时降级为全量内置工具（向后兼容）。
func buildTacticalPrompt(goal, zone, timeOfDay, slot string, physical *protocol.PhysicalState, kb *worldkb.KB, hint string, registry *CapabilityRegistry, agentID string) string {
	// 物理状态行：UE 未实现物理状态时 state_report 全 0，跳过注入避免 LLM
	// 误判"体力=0"触发不合理规划。UE 实现后自然恢复。
	physicalLine := ""
	if physical != nil && !physical.IsZero() {
		physicalLine = fmt.Sprintf("物理状态：能量 %.0f、疲劳 %.0f、关节磨损 %.0f、健康 %.0f。", physical.Energy, physical.Fatigue, physical.JointWear, physical.Health)
	}
	// 【你的角色】段：从 kb 注入 NPC 性格画像，让战术层分解体现角色风格。
	// kb==nil 或 agent 不存在时 roleLine 为空串，prompt 中该位置仅留空行。
	roleLine := ""
	if role := buildAgentRoleContext(kb, agentID); role != "" {
		roleLine = "【你的角色】\n" + role
	}
	hintLine := ""
	if hint != "" {
		hintLine = "【上次中断原因】" + hint + "（请据此调整本轮规划）"
	}
	// 物理告警强约束段：当 hint 含"物理状态告警"标记时（由 upgradeIfPhysicalAlert
	// 设置），在 prompt 中插入显式禁令——禁止工作类动作、要求恢复类动作。
	// 与 physicalAlertOverrideGoal（代码层改 goal）配合形成双保险：即使 LLM
	// 看到 override 后的恢复 goal，强约束段也防止它"自作主张"规划工作动作。
	if strings.Contains(hint, "物理状态告警") {
		hintLine += "\n【物理告警强制约束】当前物理状态已突破警戒阈值，必须立即规划恢复类动作：\n" +
			"- 优先 charge_at_station（充电）补能\n" +
			"- 充电后若仍疲劳高，追加 wait 或 rest 类动作\n" +
			"- 禁止规划 work_at_workbench / work_at_workshop / patrol_zone 等消耗体能的动作\n" +
			"- 禁止规划 move_to 到非充电站区域（除非当前已在充电站）"
	}
	toolList, toolCount := buildTacticalToolList(agentID, registry)
	return fmt.Sprintf(tacticalPromptBody, goal, zone, timeOfDay,
		physicalLine,
		roleLine,
		hintLine, buildSlotDurationHint(slot, timeOfDay), buildKBContext(kb), toolCount, toolList,
		buildTacticalExample(kb, goal))
}

// buildSlotDurationHint 根据 slot "HH:MM-HH:MM" 和当前 game_time 构造一行提示文本。
// 提示 LLM 按剩余时长（slot_end - timeOfDay）规划，避免长动作 overshoot 跨越到下个 slot。
// timeOfDay 为空或解析失败时降级为完整 slot 时长（旧行为）。
func buildSlotDurationHint(slot, timeOfDay string) string {
	start, end := slotRangeMinute(slot)
	if start < 0 {
		return ""
	}
	total := end - start
	curMin := parsePlanMinute(timeOfDay)
	if curMin < 0 {
		return fmt.Sprintf("当前时段 %s，约 %d 分钟；请让步骤总时长接近此时长，避免过短导致队列提前耗尽触发重分解。\n", slot, total)
	}
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

// slotRangeMinute 解析 "HH:MM-HH:MM" 返回 (start, end) 分钟数。
// 解析失败或 end ≤ start 返回 (-1, -1)。
func slotRangeMinute(slot string) (int, int) {
	parts := strings.SplitN(slot, "-", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	start := parsePlanMinute(parts[0])
	end := parsePlanMinute(parts[1])
	if start < 0 || end < 0 || end <= start {
		return -1, -1
	}
	return start, end
}

// slotExpired 检查当前游戏时间 tod 是否已到达或超出 currentSlot 的结束时间。
// currentSlot 为空或解析失败返回 false（无 slot 概念，不触发切换）。
// 用于 worker wake 时检测"时间到达下一次 schedule 节点"，触发打断长复合动作
// + 重新下发新 action_queue。
func slotExpired(currentSlot, tod string) bool {
	if currentSlot == "" || tod == "" {
		return false
	}
	_, endMin := slotRangeMinute(currentSlot)
	curMin := parsePlanMinute(tod)
	if endMin <= 0 || curMin < 0 {
		return false
	}
	return curMin >= endMin
}

// slotDurationMinute 解析 "HH:MM-HH:MM" 形如的 slot，返回 (end - start) 的分钟数。
// 解析失败或 end ≤ start 返回 -1。
func slotDurationMinute(slot string) int {
	s, e := slotRangeMinute(slot)
	if s < 0 {
		return -1
	}
	return e - s
}

// physicalAlertOverrideGoal 检测 replanHint 是否含物理告警标记，
// 若是则根据 physical 状态生成恢复类 goal 替换原 goal。
//
// 动机：反应层 upgradeIfPhysicalAlert 强制升级 continue/observe → replan 后，
// replanHint 含"物理状态告警自动升级(...)"。但战术层仍用原 goal 调 LLM，
// LLM 看到原 goal "车间装配作业" + 软引导 hint，仍规划 work_at_workbench。
// 此函数在代码层强制把 goal 改为恢复类 goal，配合 prompt 强约束段双保险。
//
// 返回 (overrideGoal, true) 当 hint 含"物理状态告警"且 physical 确有告警；
// 否则返回 (origGoal, false)。
func physicalAlertOverrideGoal(hint, origGoal string, physical *protocol.PhysicalState) (string, bool) {
	if !strings.Contains(hint, "物理状态告警") || physical == nil || physical.IsZero() {
		return origGoal, false
	}
	switch {
	case physical.Fatigue > fatigueAlertThreshold:
		return "前往充电站休息补能（疲劳过高，停止工作）", true
	case physical.Energy < energyAlertThreshold:
		return "前往充电站补能（体力过低）", true
	case physical.Health < healthAlertThreshold:
		return "前往维修点检修（健康过低）", true
	default:
		return origGoal, false
	}
}

// buildKBContext 拼接可用 zone/object 列表段落，供战术层 prompt 注入。
//
// 格式设计原则：每个 object 单独占一行，明确分离 id / 所在 zone / 可用 interaction，
// 避免 LLM 把 "id|zone[interactions]" 这种拼接串整体当作 target_object_id 复制。
// zone 也单独一行列出，避免与 object 拼接造成歧义。
func buildKBContext(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	var lines []string
	if zs := kb.ListZones(); len(zs) > 0 {
		parts := make([]string, 0, len(zs))
		for _, z := range zs {
			if z.DisplayName != "" && z.DisplayName != z.ID {
				parts = append(parts, fmt.Sprintf("%s（id=%s）", z.DisplayName, z.ID))
			} else {
				parts = append(parts, z.ID)
			}
		}
		lines = append(lines, "可前往区域（move_to_location 的 target 用 id）: "+strings.Join(parts, "、")+"。")
	}
	if os := kb.ListObjects(); len(os) > 0 {
		lines = append(lines, "可交互物体（interact/work_at_workbench 的 target_object_id 用 id，interact 的 interaction 用下列可用动词）:")
		for _, o := range os {
			label := o.ID
			if o.DisplayName != "" && o.DisplayName != o.ID {
				label = fmt.Sprintf("%s（id=%s）", o.DisplayName, o.ID)
			}
			zoneInfo := ""
			if o.ZoneID != "" {
				zoneInfo = "，位于 zone=" + o.ZoneID
			}
			interactionInfo := ""
			if len(o.AvailableInteractions) > 0 {
				interactionInfo = "，可用 interaction: " + strings.Join(o.AvailableInteractions, "/")
			}
			lines = append(lines, "  - "+label+zoneInfo+interactionInfo)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// generateTacticalPlan 调战术层 LLM 分解当前时段 goal（非流式路径）。
// 返回分解出的 action 列表 + inner_thought（作为整个时段独白）。
// 任一步失败返回 err，调用方决定回退兜底。
// 复用 strategicCaller 接口（venus.Client 已满足）。
func generateTacticalPlan(
	ctx context.Context,
	tc strategicCaller,
	agentID string,
	goal, zone, timeOfDay, slot string,
	physical *protocol.PhysicalState,
	kb *worldkb.KB,
	logger *slog.Logger,
	hint string,
	registry *CapabilityRegistry,
) ([]plannedAction, string, error) {
	prompt := buildTacticalPrompt(goal, zone, timeOfDay, slot, physical, kb, hint, registry, agentID)
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", prompt,
		"replan_hint", hint)

	resp, err := tc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		return nil, "", fmt.Errorf("tactical llm: %w", err)
	}
	tc.ResetSession() // 战术调用一次性，立即清链（与战略层一致）

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	actions, thought, err := parseTacticalNDJSON(raw, registry, agentID)
	if err != nil {
		return nil, "", fmt.Errorf("tactical parse: %w (raw=%s)", err, truncateText(raw, 200))
	}
	if len(actions) == 0 {
		return nil, "", fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"thought", thought, "actions", string(actionsJSON))
	return actions, thought, nil
}

// generateTacticalPlanStreaming 是 generateTacticalPlan 的流式版本：
// 调 LLM 客户端 SendStreaming 边接收边增量解析 NDJSON，每解析出一个
// action 即调 onAction 回调，使调用方能在首 action 到达时立即下发，
// 将首动作体感延迟从 ~14s 降至 ~2-3s。
//
// 走 llmClient 接口（venus.Client 实现）。
func generateTacticalPlanStreaming(
	ctx context.Context,
	tc llmClient,
	agentID, goal, zone, timeOfDay, slot string,
	physical *protocol.PhysicalState,
	kb *worldkb.KB,
	logger *slog.Logger,
	hint string,
	registry *CapabilityRegistry,
	onAction func(plannedAction),
) ([]plannedAction, string, error) {
	prompt := buildTacticalPrompt(goal, zone, timeOfDay, slot, physical, kb, hint, registry, agentID)
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", prompt,
		"streaming", true, "replan_hint", hint)

	var actions []plannedAction
	acc := &streamAccumulator{
		registry: registry,
		agentID:  agentID,
		onComplete: func(pa plannedAction) {
			actions = append(actions, pa)
			if onAction != nil {
				onAction(pa)
			}
		},
	}

	resp, err := tc.SendStreaming(ctx, prompt, func(delta string) {
		acc.feed(delta)
	})
	if err != nil {
		logger.Warn("[LLM→MCP/TACTICAL-STREAM] stream error, keeping actions already parsed",
			"agent_id", agentID, "parsed_actions", len(actions), "err", err)
		return actions, acc.thought, fmt.Errorf("tactical llm stream: %w", err)
	}
	acc.flush()
	tc.ResetSession()

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw, "streaming", true)

	if len(actions) == 0 {
		return nil, "", fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"thought", acc.thought, "actions", string(actionsJSON))
	return actions, acc.thought, nil
}

// parseTacticalNDJSON 从 LLM 的 NDJSON 输出解析 action 列表 + inner_thought。
// 容错：剥 ```json 围栏 → 按行解析 → 跳过空行/parse 失败行 → 过滤非法工具。
// 返回的 actions 已经过 filterValidActions。
//
// registry 非 nil 时，过滤还会剔除依赖的 cmd 在 registry 中对 agentID 不可用
// 的工具（与 prompt 工具列表保持一致）。
func parseTacticalNDJSON(raw string, registry *CapabilityRegistry, agentID string) ([]plannedAction, string, error) {
	s := strings.TrimSpace(raw)
	// 剥 markdown 围栏（LLM 可能仍加，即使 prompt 禁止）
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}

	var actions []plannedAction
	var thought string
	for _, line := range strings.Split(s, "\n") {
		pa, th, isAction, ok := parseTacticalNDJSONLine(line)
		if !ok {
			continue
		}
		if isAction {
			actions = append(actions, pa)
		} else {
			thought = th
		}
	}
	actions = filterValidActions(actions, registry, agentID)
	return actions, thought, nil
}

// parseTacticalNDJSONLine 解析单行 NDJSON。返回 (action, thought, isAction, ok)。
// ok=false 表示空行或 parse 失败（调用方跳过）。isAction=true 表示该行是 action；
// isAction=false 且 ok=true 表示该行是 inner_thought。
func parseTacticalNDJSONLine(line string) (pa plannedAction, thought string, isAction bool, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return plannedAction{}, "", false, false
	}
	var nl ndjsonLine
	if err := json.Unmarshal([]byte(line), &nl); err != nil {
		return plannedAction{}, "", false, false
	}
	if nl.Action != "" {
		return plannedAction{Action: nl.Action, Params: nl.Params}, "", true, true
	}
	if nl.InnerThought != "" {
		return plannedAction{}, nl.InnerThought, false, true
	}
	return plannedAction{}, "", false, false
}

// streamAccumulator 是流式回调的增量 NDJSON 解析器。
// feed(delta) 追加 delta 到内部 buffer，按 \n 分割出完整行并即时解析；
// 最后一行（可能不完整）保留在 buffer 等下次 feed 补全。
// flush() 在流结束后调用，处理 buffer 中的残余内容。
type streamAccumulator struct {
	buf        strings.Builder
	onComplete func(plannedAction) // 每完整解析出一个合法 action 调一次
	thought    string
	registry   *CapabilityRegistry // 用于过滤依赖 cmd 不可用的工具；nil = 不过滤
	agentID    string              // 配合 registry 做 per-agent 过滤
}

// feed 追加一段 delta 文本并处理所有已完成的行（以 \n 结尾）。
func (a *streamAccumulator) feed(delta string) {
	a.buf.WriteString(delta)
	content := a.buf.String()
	lines := strings.Split(content, "\n")
	// 除最后一行外都是完整行（以 \n 结尾）。
	for _, line := range lines[:len(lines)-1] {
		a.processLine(line)
	}
	// 最后一行可能不完整，留在 buffer 等下次 feed。
	a.buf.Reset()
	a.buf.WriteString(lines[len(lines)-1])
}

// flush 在流结束后调用，处理 buffer 中残余的最后一行。
func (a *streamAccumulator) flush() {
	remaining := strings.TrimSpace(a.buf.String())
	a.buf.Reset()
	if remaining == "" {
		return
	}
	a.processLine(remaining)
}

// processLine 解析单行：合法 action 调 onComplete，inner_thought 存入 thought。
func (a *streamAccumulator) processLine(line string) {
	pa, thought, isAction, ok := parseTacticalNDJSONLine(line)
	if !ok {
		return
	}
	if isAction {
		if !tacticalActionAvailable(pa.Action, a.agentID, a.registry) {
			return // 过滤非法工具或依赖 cmd 不可用的工具（与 parseTacticalNDJSON 一致）
		}
		if a.onComplete != nil {
			a.onComplete(pa)
		}
	} else {
		a.thought = thought
	}
}

// filterValidActions 过滤掉 scan_area/stop/未知工具，保留可排队工具。
// registry 非 nil 时同时过滤依赖 cmd 对 agentID 不可用的工具。
func filterValidActions(actions []plannedAction, registry *CapabilityRegistry, agentID string) []plannedAction {
	out := make([]plannedAction, 0, len(actions))
	for _, a := range actions {
		if tacticalActionAvailable(a.Action, agentID, registry) {
			out = append(out, a)
		}
	}
	return out
}

// mapTacticalAction 把战术层 plannedAction 映射到 ws.SendAction 的 (cmd, params)。
// 复合工具 → 各自 Composite cmd；原子工具 → 各自 Atomic cmd；
// move_to_location 需 KB 解析坐标。映射规则与 composite.go/atomic.go
// 工具处理函数一致。非法/不可排队工具返回 err，调用方跳过。
//
// registry != nil 时，未匹配内置 case 的 action 走默认 passthrough 路径：
// 从 registry.EffectiveActions(agentID) 反查 cmd，params 原样转发。这覆盖
// UE 通过 capability_registry 新推送的 cmd（无强类型 Go struct，依赖通用工具
// 注册路径）。registry == nil 时默认分支返回 err（向后兼容旧测试）。
func mapTacticalAction(pa plannedAction, agentID string, kb *worldkb.KB, registry *CapabilityRegistry) (cmd string, params map[string]any, err error) {
	switch pa.Action {
	// ─── Composite tools → 各自 cmd ───
	case "work_at_workbench":
		params := map[string]any{
			"target_object_id": pa.Params["target_object_id"],
		}
		if d := toFloat(pa.Params["duration_sec"]); d > 0 {
			params["duration_sec"] = d
		}
		return protocol.CmdWorkAtWorkbench, params, nil
	case "work_at_workshop":
		return protocol.CmdWorkAtWorkshop, map[string]any{}, nil
	case "chat_with":
		params := map[string]any{
			"target_agent_id": pa.Params["target_agent_id"],
		}
		if t, ok := pa.Params["topic"].(string); ok && t != "" {
			params["topic"] = t
		}
		return protocol.CmdChatWith, params, nil
	case "repair_target":
		params := map[string]any{
			"target_agent_id": pa.Params["target_agent_id"],
		}
		if t, ok := pa.Params["tool_id"].(string); ok && t != "" {
			params["tool_id"] = t
		}
		return protocol.CmdRepairTarget, params, nil
	case "charge_at_station":
		// 全量透传 params：真实 UE5 声明 smart_object + interaction，
		// mock UE 声明 target_object_id + duration_sec。不挑字段避免丢参数。
		params := make(map[string]any, len(pa.Params))
		for k, v := range pa.Params {
			params[k] = v
		}
		return protocol.CmdChargeAtStation, params, nil
	case "patrol_zone":
		params := map[string]any{
			"target_zone": pa.Params["target_zone"],
		}
		if d := toFloat(pa.Params["duration_sec"]); d > 0 {
			params["duration_sec"] = d
		}
		return protocol.CmdPatrolZone, params, nil
	// ─── Atomic tools ───
	case "move_to_location":
		target, _ := pa.Params["target"].(string)
		coord, _, e := kb.GetPosition(target) // 与 atomic.go 一致
		if e != nil {
			return "", nil, fmt.Errorf("move_to_location resolve %q: %w", target, e)
		}
		speed, _ := pa.Params["speed"].(string)
		if speed == "" {
			speed = "walk"
		}
		return protocol.CmdMoveToLocation, map[string]any{
			"dest":  []float64{coord[0], coord[1], coord[2]},
			"speed": speed,
		}, nil
	case "move_to_agent":
		speed, _ := pa.Params["speed"].(string)
		if speed == "" {
			speed = "walk"
		}
		return protocol.CmdMoveToAgent, map[string]any{
			"target_agent_id": pa.Params["target_agent_id"],
			"speed":           speed,
			"stop_distance":   toFloat(pa.Params["stop_distance"]),
			"keep_following":  pa.Params["keep_following"],
		}, nil
	case "turn_to":
		params := map[string]any{}
		if t, ok := pa.Params["target_agent_id"].(string); ok && t != "" {
			params["target_agent_id"] = t
		}
		if d, ok := pa.Params["direction"].([]float64); ok && len(d) > 0 {
			params["direction"] = d
		}
		return protocol.CmdTurnTo, params, nil
	case "play_montage":
		return protocol.CmdPlayMontage, map[string]any{
			"montage_id":  pa.Params["montage_id"],
			"wait_finish": pa.Params["wait_finish"],
		}, nil
	case "speak":
		return protocol.CmdSpeak, map[string]any{
			"content":         pa.Params["content"],
			"target_agent_id": pa.Params["target_agent_id"],
			"audio_url":       pa.Params["audio_url"],
		}, nil
	case "emote":
		mode, _ := pa.Params["mode"].(string)
		if mode == "" {
			mode = "oneshot"
		}
		return protocol.CmdEmote, map[string]any{
			"emotion": pa.Params["emotion"],
			"mode":    mode,
		}, nil
	case "interact":
		return protocol.CmdInteractSmartObject, map[string]any{
			"target_object_id": pa.Params["target_object_id"],
			"interaction":      pa.Params["interaction"],
		}, nil
	case "wait":
		return protocol.CmdWait, map[string]any{
			"duration_sec": toFloat(pa.Params["duration_sec"]),
		}, nil
	default:
		// 新 cmd passthrough：从 registry 反查 cmd，params 原样转发
		if registry == nil {
			return "", nil, fmt.Errorf("unknown/unsupported tactical action: %s", pa.Action)
		}
		for _, act := range registry.EffectiveActions(agentID) {
			if tools.CmdToToolName(act.Cmd) != pa.Action {
				continue
			}
			// 复制 params 避免调用方误改原 map
			out := make(map[string]any, len(pa.Params))
			for k, v := range pa.Params {
				out[k] = v
			}
			return act.Cmd, out, nil
		}
		return "", nil, fmt.Errorf("unknown/unsupported tactical action: %s", pa.Action)
	}
}

// toFloat 容错地把 any 转 float64（LLM 可能输出 int/float/string/json.Number）。
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return f
	case string:
		var f float64
		fmt.Sscanf(n, "%f", &f)
		return f
	default:
		return 0
	}
}

// selectCurrentGoal 根据当前游戏时间从 daily_plan 中选出要分解的 goal。
// 返回 goal 文本、所在时段 "HH:MM-HH:MM"、item 索引。
// 无匹配时段（时间在所有时段之前/之后或计划为空）返回 ("", "", -1)。
// 调用方用返回的 slot 与自身 currentSlot 比较来决定是否重复分解。
func selectCurrentGoal(dailyPlan, timeOfDay string) (goal, slot string, index int) {
	if dailyPlan == "" {
		return "", "", -1
	}
	items := parseFormattedPlan(dailyPlan)
	if len(items) == 0 {
		return "", "", -1
	}
	slot = matchPlanSlot(items, timeOfDay)
	if slot == "" {
		return "", "", -1
	}
	for i, item := range items {
		if item.Time == slot {
			return item.Goal, slot, i
		}
	}
	return "", "", -1
}
