package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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
	sourceHermes   actionSource = "hermes"
	sourceTactical actionSource = "tactical"
)

// tacticalToolMeta 是战术层可选用工具的元数据表，同时是 prompt 工具列表段、
// action 合法性校验、cmd 依赖关系的单一来源（替代原 tacticalValidActions
// 全局 map + tacticalPromptTemplate 硬编码 bullet 列表）。
//
// 顺序即 prompt 中展示的顺序；scan_area/stop 不在此表（战术层不可排队）。
var tacticalToolMeta = []struct {
	Name        string
	RequiredCmd string // 该工具依赖的 UE cmd；"" 表示不依赖 UE（理论上不会出现在此表）
	Desc        string // prompt 中的工具描述
	Params      string // prompt 中的 params 示例
}{
	// Atomic tools (8)
	{"move_to_location", protocol.CmdMoveToLocation, "移动到目标位置", `{"target":"区域或位置id"}`},
	{"move_to_agent", protocol.CmdMoveToAgent, "跟随目标agent", `{"target_agent_id":"...","speed":"walk|run"}`},
	{"turn_to", protocol.CmdTurnTo, "转向目标", `{"target_agent_id":"实体id"} 或 {"direction":[dx,dy,dz]}`},
	{"play_montage", protocol.CmdPlayMontage, "播放蒙太奇", `{"montage_id":"...","wait_finish":true}`},
	{"speak", protocol.CmdSpeak, "说话", `{"content":"...","target_agent_id":"目标agent_id（可空）"}`},
	{"emote", protocol.CmdEmote, "表达情绪", `{"emotion":"happy|sad|...","mode":"oneshot|sustained"}`},
	{"interact", protocol.CmdInteractSmartObject, "与智能物体交互", `{"target_object_id":"...","interaction":"动词"}`},
	{"wait", protocol.CmdWait, "原地等待", `{"duration_sec":秒数}`},
	// Composite tools (6)
	{"work_at_workbench", protocol.CmdWorkAtWorkbench, "在工作台装配", `{"target_object_id":"工作台id","duration_sec":秒数}`},
	{"work_at_workshop", protocol.CmdWorkAtWorkshop, "车间例行工作", `{}`},
	{"chat_with", protocol.CmdChatWith, "与其他agent聊天", `{"target_agent_id":"...","topic":"话题（可选）"}`},
	{"repair_target", protocol.CmdRepairTarget, "修理其他agent", `{"target_agent_id":"...","tool_id":"工具id（可选）"}`},
	{"charge_at_station", protocol.CmdChargeAtStation, "充电", `{"target_object_id":"充电站id（可空）"}`},
	{"patrol_zone", protocol.CmdPatrolZone, "巡逻区域", `{"target_zone":"区域id","duration_sec":秒数}`},
}

// tacticalActionAvailable 判断 action 是否为战术层可用工具，且其依赖的
// cmd 在 registry 中对 agentID 有效。registry == nil 时降级为仅检查是否
// 内置战术工具（向后兼容测试与未启用 capability 的场景）。
func tacticalActionAvailable(action, agentID string, registry *CapabilityRegistry) bool {
	for _, tm := range tacticalToolMeta {
		if tm.Name != action {
			continue
		}
		if registry == nil {
			return true
		}
		return tm.RequiredCmd == "" || registry.HasCmd(agentID, tm.RequiredCmd)
	}
	return false
}

// buildTacticalToolList 按 registry 对 agentID 的有效能力集过滤
// tacticalToolMeta，构造 prompt 中的工具 bullet 列表。registry == nil
// 时返回全量列表（向后兼容）。
// 返回 (拼接好的 bullet 段, 可用工具数)。
func buildTacticalToolList(agentID string, registry *CapabilityRegistry) (string, int) {
	lines := make([]string, 0, len(tacticalToolMeta))
	count := 0
	for _, tm := range tacticalToolMeta {
		if registry != nil && !registry.HasCmd(agentID, tm.RequiredCmd) {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s。params: %s", tm.Name, tm.Desc, tm.Params))
		count++
	}
	return strings.Join(lines, "\n"), count
}

// tacticalPromptBody 是 prompt 的固定骨架，%s 占位符依次为：
// goal / zone / timeOfDay / energy / fatigue / joint_wear / health /
// hintLine / slotDurationHint / kbContext / toolCount / toolList / exampleBlock。
// exampleBlock 由 buildTacticalExample 动态从 KB 取合法 id 生成，避免示例
// 本身编造 KB 外 id（旧版示例写死 main_workshop / workbench_01 诱导 LLM 跟随编造）。
const tacticalPromptBody = `[战术层/任务分解] 当前时段目标：%s
你目前在：%s，游戏时间 %s。
物理状态：能量 %.0f、疲劳 %.0f、关节磨损 %.0f、健康 %.0f。

请把这个目标分解为 3-5 步具体的 action，按顺序执行。
%s
%s
%s
可用工具（仅限以下 %d 个，禁止使用 scan_area / stop）：
%s

要求：
1. 第一行输出 {"inner_thought":"一句话内心独白"}
2. 后续每行输出一个 {"action":"工具名","params":{...}}，3-5 步，按执行顺序排列
3. 第一步通常是 move_to_location 到目标区域
4. move_to_location 的 target、interact 的 target_object_id、work_at_workbench 的 target_object_id、patrol_zone 的 target_zone 必须严格使用上面"可前往区域"和"可交互物体"中给出的 id，禁止编造、禁止拼接 zone/interaction 信息
5. 每行一个 JSON 对象，不要输出 JSON 数组，不要输出 markdown 围栏，不要输出任何其他文字
6. 必须以字符 {"inner_thought 开头，不要输出步骤说明、不要解释、不要编号列表、不要 markdown 加粗
7. 步骤总时长应接近当前 slot 时长，避免过短导致队列提前耗尽触发重分解

示例（id 来自上方可用列表，不可照抄示例中的 id）：
%s`

// buildTacticalExample 根据当前 KB 动态构造示例，确保示例中出现的
// zone id / object id 都在 KB 中合法存在。KB 为空时返回一个不引用
// 任何具体 id 的通用示例，避免误导 LLM 编造。
func buildTacticalExample(kb *worldkb.KB) string {
	if kb == nil {
		return `{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"<上方可前往区域的 id>"}}
{"action":"work_at_workbench","params":{"target_object_id":"<上方可交互物体的 id>","duration_sec":3600}}`
	}
	zoneID := ""
	if zs := kb.ListZones(); len(zs) > 0 {
		zoneID = zs[0].ID
	}
	objID := ""
	if os := kb.ListObjects(); len(os) > 0 {
		objID = os[0].ID
	}
	if zoneID == "" && objID == "" {
		return `{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"<上方可前往区域的 id>"}}
{"action":"work_at_workbench","params":{"target_object_id":"<上方可交互物体的 id>","duration_sec":3600}}`
	}
	exZone := zoneID
	if exZone == "" {
		exZone = "<上方可前往区域的 id>"
	}
	exObj := objID
	if exObj == "" {
		exObj = "<上方可交互物体的 id>"
	}
	return fmt.Sprintf(`{"inner_thought":"先去目标区域再开始作业"}
{"action":"move_to_location","params":{"target":"%s"}}
{"action":"work_at_workbench","params":{"target_object_id":"%s","duration_sec":3600}}`, exZone, exObj)
}

// buildTacticalPrompt 填充战术层 prompt 模板。kb 用于注入可用 zone/location/object
// 列表，避免 LLM 编造不存在的 ID（如 workbench_02、archives）。
// slot 形如 "HH:MM-HH:MM"，用于在 prompt 里提示当前时段时长，引导 LLM
// 给出总时长接近 slot 时长的步骤，减少队列提前耗尽导致的重分解。
// slot 为空或解析失败时该提示行降级为空，保持旧行为。
//
// registry 非 nil 时，工具列表段按 registry 对 agentID 的有效能力集动态
// 生成（per-agent 覆盖全局默认）；nil 时降级为全量内置工具（向后兼容）。
func buildTacticalPrompt(goal, zone, timeOfDay, slot string, physical *protocol.PhysicalState, kb *worldkb.KB, hint string, registry *CapabilityRegistry, agentID string) string {
	e, f, j, h := 0.0, 0.0, 0.0, 0.0
	if physical != nil {
		e, f, j, h = physical.Energy, physical.Fatigue, physical.JointWear, physical.Health
	}
	hintLine := ""
	if hint != "" {
		hintLine = "【上次中断原因】" + hint + "（请据此调整本轮规划）"
	}
	toolList, toolCount := buildTacticalToolList(agentID, registry)
	return fmt.Sprintf(tacticalPromptBody, goal, zone, timeOfDay, e, f, j, h,
		hintLine, buildSlotDurationHint(slot), buildKBContext(kb), toolCount, toolList,
		buildTacticalExample(kb))
}

// buildSlotDurationHint 根据slot "HH:MM-HH:MM" 构造一行提示文本。
// 解析失败或时长 ≤ 0 返回空串（prompt 该行降级为空）。
func buildSlotDurationHint(slot string) string {
	min := slotDurationMinute(slot)
	if min <= 0 {
		return ""
	}
	return fmt.Sprintf("当前时段 %s，约 %d 分钟；请让步骤总时长接近此时长，避免过短导致队列提前耗尽触发重分解。\n", slot, min)
}

// slotDurationMinute 解析 "HH:MM-HH:MM" 形如的 slot，返回 (end - start) 的分钟数。
// 解析失败或 end ≤ start 返回 -1。
func slotDurationMinute(slot string) int {
	parts := strings.SplitN(slot, "-", 2)
	if len(parts) != 2 {
		return -1
	}
	start := parsePlanMinute(parts[0])
	end := parsePlanMinute(parts[1])
	if start < 0 || end < 0 || end <= start {
		return -1
	}
	return end - start
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
// 任一步失败返回 err，调用方决定回退到 Hermes。
// 复用 strategicCaller 接口（hermes.Client 已满足）。
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
	logger.Info("[MCP→Hermes/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", prompt,
		"replan_hint", hint)

	resp, err := tc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		return nil, "", fmt.Errorf("tactical llm: %w", err)
	}
	tc.ResetSession() // 战术调用一次性，立即清链（与战略层一致）

	raw := resp.ExtractText()
	logger.Info("[Hermes→MCP/TACTICAL-RESPONSE]",
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
// 走 llmClient 接口（hermes.Client 和 venus.Client 均实现），由 main.go
// 的 --llm-backend 决定具体后端。
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
	logger.Info("[MCP→Hermes/TACTICAL-PROMPT]",
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
		logger.Warn("[Hermes→MCP/TACTICAL-STREAM] stream error, keeping actions already parsed",
			"agent_id", agentID, "parsed_actions", len(actions), "err", err)
		return actions, acc.thought, fmt.Errorf("tactical llm stream: %w", err)
	}
	acc.flush()
	tc.ResetSession()

	raw := resp.ExtractText()
	logger.Info("[Hermes→MCP/TACTICAL-RESPONSE]",
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
func mapTacticalAction(pa plannedAction, kb *worldkb.KB) (cmd string, params map[string]any, err error) {
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
		params := map[string]any{}
		if s, ok := pa.Params["target_object_id"].(string); ok && s != "" {
			params["target_object_id"] = s
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
