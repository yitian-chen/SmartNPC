package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// plannedAction 是战术层分解出的单步 action，对应一个 MCP 工具调用。
type plannedAction struct {
	Action string         `json:"action"` // 工具名：work_assemble / move_to / ...
	Params map[string]any `json:"params"` // 工具参数（LLM 原样输出，duration_min 等未换算）
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

// tacticalValidActions 列出战术层允许使用的工具（不含 scan_area/stop）。
var tacticalValidActions = map[string]bool{
	"move_to":          true,
	"turn_to":          true,
	"speak":            true,
	"emote":            true,
	"interact":         true,
	"wait":             true,
	"work_assemble":    true,
	"patrol_route":     true,
	"charge_at":        true,
	"repair_target":    true,
	"social_chat_with": true,
	"rest_idle":        true,
	"archive_research": true,
}

const tacticalPromptTemplate = `[战术层/任务分解] 当前时段目标：%s
你目前在：%s，游戏时间 %s。
物理状态：能量 %.0f、疲劳 %.0f、关节磨损 %.0f、健康 %.0f。

请把这个目标分解为 3-5 步具体的 action，按顺序执行。

%s
可用工具（仅限以下 13 个，禁止使用 scan_area / stop）：
- move_to: 移动到目标位置。params: {"target":"区域或位置id"}
- turn_to: 转向目标。params: {"target":"实体id"}
- speak: 说话。params: {"content":"...","target":"目标agent_id（可空）"}
- emote: 表达情绪。params: {"emotion":"happy|sad|...","mode":"oneshot|sustained"}
- interact: 与智能物体交互。params: {"object_id":"...","action":"动词"}
- wait: 原地等待。params: {"duration_sec":秒数}
- work_assemble: 在工作台装配。params: {"target":"工作台id","duration_min":分钟}
- patrol_route: 巡逻路线。params: {"route_id":"路线id"}
- charge_at: 充电。params: {"station_id":"充电站id","duration_min":分钟}
- repair_target: 修理其他agent。params: {"target_agent_id":"..."}
- social_chat_with: 与其他agent聊天。params: {"target_agent_id":"..."}
- rest_idle: 休息。params: {"duration_min":分钟}
- archive_research: 档案研究。params: {"duration_min":分钟}

要求：
1. 第一行输出 {"inner_thought":"一句话内心独白"}
2. 后续每行输出一个 {"action":"工具名","params":{...}}，3-5 步，按执行顺序排列
3. 第一步通常是 move_to 到目标区域
4. move_to 的 target、interact 的 object_id、work_assemble 的 target 必须从上面的可用列表中选取，禁止编造
5. 每行一个 JSON 对象，不要输出 JSON 数组，不要输出 markdown 围栏，不要输出任何其他文字

示例：
{"inner_thought":"先去车间再开始装配"}
{"action":"move_to","params":{"target":"main_workshop"}}
{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}`

// buildTacticalPrompt 填充战术层 prompt 模板。kb 用于注入可用 zone/location/object
// 列表，避免 LLM 编造不存在的 ID（如 workbench_02、archives）。
func buildTacticalPrompt(goal, zone, timeOfDay string, physical *protocol.PhysicalState, kb *worldkb.KB) string {
	e, f, j, h := 0.0, 0.0, 0.0, 0.0
	if physical != nil {
		e, f, j, h = physical.Energy, physical.Fatigue, physical.JointWear, physical.Health
	}
	return fmt.Sprintf(tacticalPromptTemplate, goal, zone, timeOfDay, e, f, j, h, buildKBContext(kb))
}

// buildKBContext 拼接可用 zone/location/object 列表段落，供战术层 prompt 注入。
func buildKBContext(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	var lines []string
	if zs := kb.ListZones(); len(zs) > 0 {
		parts := make([]string, 0, len(zs))
		for _, z := range zs {
			if z.Name != "" && z.Name != z.ID {
				parts = append(parts, fmt.Sprintf("%s(%s)", z.Name, z.ID))
			} else {
				parts = append(parts, z.ID)
			}
		}
		lines = append(lines, "可前往区域: "+strings.Join(parts, "、")+"。")
	}
	if ls := kb.ListLocations(); len(ls) > 0 {
		parts := make([]string, 0, len(ls))
		for _, l := range ls {
			if l.Name != "" && l.Name != l.ID {
				parts = append(parts, fmt.Sprintf("%s(%s)", l.Name, l.ID))
			} else {
				parts = append(parts, l.ID)
			}
		}
		lines = append(lines, "可前往地点: "+strings.Join(parts, "、")+"。")
	}
	if os := kb.ListObjects(); len(os) > 0 {
		parts := make([]string, 0, len(os))
		for _, o := range os {
			label := o.ID
			if o.Name != "" && o.Name != o.ID {
				label = fmt.Sprintf("%s(%s)", o.Name, o.ID)
			}
			if len(o.AvailableActions) > 0 {
				label += "[" + strings.Join(o.AvailableActions, "/") + "]"
			}
			parts = append(parts, label)
		}
		lines = append(lines, "可交互物体: "+strings.Join(parts, "、")+"。")
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
	goal, zone, timeOfDay string,
	physical *protocol.PhysicalState,
	kb *worldkb.KB,
	logger *slog.Logger,
) ([]plannedAction, string, error) {
	prompt := buildTacticalPrompt(goal, zone, timeOfDay, physical, kb)
	logger.Info("[MCP→Hermes/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", prompt)

	resp, err := tc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		return nil, "", fmt.Errorf("tactical llm: %w", err)
	}
	tc.ResetSession() // 战术调用一次性，立即清链（与战略层一致）

	raw := resp.ExtractText()
	logger.Info("[Hermes→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	actions, thought, err := parseTacticalNDJSON(raw)
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
// 调 hermes.Client.SendStreaming 边接收边增量解析 NDJSON，每解析出一个
// action 即调 onAction 回调，使调用方能在首 action 到达时立即下发，
// 将首动作体感延迟从 ~14s 降至 ~2-3s。
//
// 直接用 *hermes.Client（不走 strategicCaller 窄接口），因为需要 SendStreaming。
func generateTacticalPlanStreaming(
	ctx context.Context,
	tc *hermes.Client,
	agentID, goal, zone, timeOfDay string,
	physical *protocol.PhysicalState,
	kb *worldkb.KB,
	logger *slog.Logger,
	onAction func(plannedAction),
) ([]plannedAction, string, error) {
	prompt := buildTacticalPrompt(goal, zone, timeOfDay, physical, kb)
	logger.Info("[MCP→Hermes/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", prompt, "streaming", true)

	var actions []plannedAction
	acc := &streamAccumulator{
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
func parseTacticalNDJSON(raw string) ([]plannedAction, string, error) {
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
	actions = filterValidActions(actions)
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
		if !tacticalValidActions[pa.Action] {
			return // 过滤非法工具（与 parseTacticalNDJSON 一致）
		}
		if a.onComplete != nil {
			a.onComplete(pa)
		}
	} else {
		a.thought = thought
	}
}

// filterValidActions 过滤掉 scan_area/stop/未知工具，保留可排队工具。
func filterValidActions(actions []plannedAction) []plannedAction {
	out := make([]plannedAction, 0, len(actions))
	for _, a := range actions {
		if tacticalValidActions[a.Action] {
			out = append(out, a)
		}
	}
	return out
}

// mapTacticalAction 把战术层 plannedAction 映射到 ws.SendAction 的 (cmd, params)。
// 复合工具 → CmdExecuteComposite；原子工具 → 各自 cmd；move_to 需 KB 解析坐标。
// 映射规则与 composite.go/atomic.go 工具处理函数一致。
// 非法/不可排队工具返回 err，调用方跳过。
func mapTacticalAction(pa plannedAction, kb *worldkb.KB) (cmd string, params map[string]any, err error) {
	switch pa.Action {
	// ─── 复合工具 → ExecuteComposite ───
	case "work_assemble":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":         "work_assemble",
			"target":       pa.Params["target"],
			"duration_sec": toFloat(pa.Params["duration_min"]) * 60,
		}, nil
	case "patrol_route":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":     "patrol_route",
			"route_id": pa.Params["route_id"],
		}, nil
	case "charge_at":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":         "charge_at",
			"station_id":   pa.Params["station_id"],
			"duration_sec": toFloat(pa.Params["duration_min"]) * 60,
		}, nil
	case "repair_target":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":            "repair_target",
			"target_agent_id": pa.Params["target_agent_id"],
		}, nil
	case "social_chat_with":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":            "social_chat_with",
			"target_agent_id": pa.Params["target_agent_id"],
		}, nil
	case "rest_idle":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":         "rest_idle",
			"duration_sec": toFloat(pa.Params["duration_min"]) * 60,
		}, nil
	case "archive_research":
		return protocol.CmdExecuteComposite, map[string]any{
			"name":         "archive_research",
			"duration_sec": toFloat(pa.Params["duration_min"]) * 60,
		}, nil
	// ─── 原子工具 ───
	case "move_to":
		target, _ := pa.Params["target"].(string)
		coord, kind, e := kb.GetPosition(target) // 与 atomic.go:95 一致
		if e != nil {
			return "", nil, fmt.Errorf("move_to resolve %q: %w", target, e)
		}
		return protocol.CmdMoveTo, map[string]any{
			"dest":   []float64{coord[0], coord[1], coord[2]},
			"target": target,
			"kind":   kind,
			"speed":  "walk",
		}, nil
	case "turn_to":
		return protocol.CmdTurnTo, map[string]any{"target": pa.Params["target"]}, nil
	case "speak":
		return protocol.CmdSpeak, map[string]any{
			"content":   pa.Params["content"],
			"target":    pa.Params["target"],
			"audio_url": nil,
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
			"object_id": pa.Params["object_id"],
			"action":    pa.Params["action"],
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
