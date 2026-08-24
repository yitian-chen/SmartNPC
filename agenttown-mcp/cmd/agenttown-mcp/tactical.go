package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// plannedAction 是战术层分解出的单步 action，对应一个 MCP 工具调用。
// 类型定义已迁移到 pkg/agentstate（导出名 PlannedAction），此处保留
// alias 供 main 包过渡期使用，避免一次性重命名几十处引用。
type plannedAction = agentstate.PlannedAction

// actionSource 标识一个在途 action 由哪一层下发，决定 completion 后的路由。
// 类型定义已迁移到 pkg/agentstate（导出名 ActionSource），此处保留 alias。
type actionSource = agentstate.ActionSource

const (
	sourceTool     actionSource = agentstate.SourceTool
	sourceTactical actionSource = agentstate.SourceTactical
)

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
	// 旧工具名 interact 已改名 InteractSmartObject（与 UE 注册 cmd 同名）；
	// LLM 偶发输出旧名时按新名处理，避免动作被静默丢弃。
	if action == "interact" {
		action = "InteractSmartObject"
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

// physicalAlertOverrideGoal 检测 replanHint 是否含物理告警标记，
// 若是则根据 physical 状态生成恢复类 goal 替换原 goal。
//
// 动机：反应层 upgradeIfPhysicalAlert 强制升级 continue/observe → replan 后，
// replanHint 含"物理状态告警自动升级(...)"。但战术层仍用原 goal 调 LLM，
// LLM 看到原 goal "车间装配作业" + 软引导 hint，仍规划 work_at_workbench。
// 此函数在代码层强制把 goal 改为恢复类 goal，配合 prompt 强约束段双保险。
//
// 返回 (overrideGoal, true) 当 hint 含"物理状态告警"且 physical 确有告警；
// 否则返回 (origGoal, false)。th 为该 NPC 的 per-NPC 分段阈值（profile
// ## 属性分段），零值回退全局默认。
func physicalAlertOverrideGoal(hint, origGoal string, physical *protocol.PhysicalState, th prompt.BandThresholds) (string, bool) {
	if !strings.Contains(hint, "物理状态告警") || physical == nil || physical.IsZero() {
		return origGoal, false
	}
	th = th.OrDefault()
	switch {
	case physical.Fatigue > th.FatigueAlert():
		return "前往充电站休息补能（疲劳过高，停止工作）", true
	case physical.Energy < th.EnergyAlert():
		return "前往充电站补能（体力过低）", true
	case physical.JointWear > th.JointWearAlert():
		return "前往维护点进行保养检修（关节磨损过高）", true
	default:
		return origGoal, false
	}
}

// generateTacticalPlan 调战术层 LLM 分解当前时段 goal（非流式，多轮）。
// conversation 是此前累积的对话历史（system 由本函数重建并置于最前）。
// 返回分解出的 action 段 + assistant 消息（调用方 append 回 conversation）。
// 任一步失败返回 err，调用方决定回退兜底。
func generateTacticalPlan(
	ctx context.Context,
	tc llmClient,
	conversation []llmtypes.Message,
	agentID string,
	goal, zone, timeOfDay, slot, dailyPlan string,
	physical *protocol.PhysicalState,
	kb *worldkb.KB,
	profiles map[string]*profile.Profile,
	logger *slog.Logger,
	hint string,
	memories string,
	relationships string,
	registry *CapabilityRegistry,
	objectStatus map[string]protocol.ObjectCategoryStatus,
	nearbyObjects []protocol.NearbyObject,
	visibleAgents []protocol.VisibleAgent,
) ([]plannedAction, llmtypes.Message, error) {
	// System prompt：与战略层共享三模块（世界背景/人物背景/世界详细
	// 信息），会话内稳定可缓存；user prompt 携带四段结构，工具经
	// function calling 的 tools 字段下发，不再注入 prompt 文本。
	system := prompt.BuildTacticalSystemPrompt(kb, profiles, agentID)
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:          goal,
		Zone:          zone,
		TimeOfDay:     timeOfDay,
		Slot:          slot,
		DailyPlan:     dailyPlan,
		Physical:      physical,
		KB:            kb,
		Profiles:      profiles,
		Hint:          hint,
		Memories:      memories,
		Relationships: relationships,
		AgentID:       agentID,
		ObjectStatus:  objectStatus,
		NearbyObjects: nearbyObjects,
		VisibleAgents: visibleAgents,
	})
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", promptText,
		"replan_hint", hint, "history_turns", len(conversation))
	// function calling tools：仅经请求体 tools 字段下发。
	ftools := tacticalToolsFromRegistry(registry, agentID)

	// 多轮 messages：[system, ...历史, user(最新实时状态)]。
	messages := make([]llmtypes.Message, 0, len(conversation)+2)
	messages = append(messages, llmtypes.Message{Role: "system", Content: system})
	messages = append(messages, conversation...)
	messages = append(messages, llmtypes.Message{Role: "user", Content: promptText})

	resp, err := tc.SendMessagesTools(ctx, messages, ftools)
	// 实际 prompt 文档：记录 H-01 最新一次战术层请求体完整 JSON（无论成败）。
	dumpLastRequestBody(agentID, "tactical", tc, logger)
	if err != nil {
		return nil, llmtypes.Message{}, fmt.Errorf("tactical llm: %w", err)
	}
	tc.ResetSession() // 战术调用一次性，立即清链（与战略层一致）

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw,
		"tool_calls", len(resp.ToolCalls))

	if len(resp.ToolCalls) == 0 {
		return nil, llmtypes.Message{}, fmt.Errorf("tactical plan has no tool calls (raw=%s)", truncateText(raw, 200))
	}
	actions := parseToolCalls(resp.ToolCalls, registry, agentID)
	if len(actions) == 0 {
		return nil, llmtypes.Message{}, fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"actions", string(actionsJSON))
	assistant := llmtypes.Message{Role: "assistant", Content: raw, ToolCalls: resp.ToolCalls}
	return actions, assistant, nil
}

// parseToolCalls 把 LLM 返回的 tool_calls 解析为 plannedAction 队列。
// function.name → Action；function.arguments（JSON）→ Params。arguments
// 解析失败或 name 为空时跳过该条；末尾过 filterValidActions。
func parseToolCalls(tcs []llmtypes.ToolCall, registry *CapabilityRegistry, agentID string) []plannedAction {
	var actions []plannedAction
	for _, tc := range tcs {
		name := tc.Function.Name
		if name == "interact" { // 旧工具名，保留兼容 LLM 偶发输出
			name = "InteractSmartObject"
		}
		if name == "" {
			continue
		}
		params := map[string]any{}
		if args := strings.TrimSpace(tc.Function.Arguments); args != "" {
			if err := json.Unmarshal([]byte(args), &params); err != nil {
				continue // arguments 不是合法 JSON，跳过该工具调用
			}
		}
		actions = append(actions, plannedAction{Action: name, Params: params, ToolCallID: tc.ID})
	}
	return filterValidActions(actions, registry, agentID)
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

// tacticalToolsFromRegistry 从 capability registry 派生 OpenAI function
// calling 的 tools 数组，注入战术层请求体。工具名由 CmdToToolName 生成，
// 描述与参数 schema 来自 CapabilityAction。跳过 scan_area/stop/wait
// （非战术层排队工具）。registry == nil → nil（UE 未连接时请求体不带
// tools）。工具清单仅经此下发，不再注入 prompt 文本。
func tacticalToolsFromRegistry(registry *CapabilityRegistry, agentID string) []venus.Tool {
	if registry == nil {
		return nil
	}
	actions := registry.EffectiveActions(agentID)
	out := make([]venus.Tool, 0, len(actions))
	for _, act := range actions {
		name := tools.CmdToToolName(act.Cmd)
		if name == "scan_area" || name == "stop" || name == "wait" {
			continue
		}
		desc := act.Description
		if desc == "" {
			desc = name
		}
		out = append(out, venus.Tool{
			Type: "function",
			Function: venus.ToolFunction{
				Name:        name,
				Description: desc,
				Parameters:  capabilityParamsSchema(act.Params),
			},
		})
	}
	return out
}

// capabilityParamsSchema 把 CapabilityParam 列表转成 function calling 的
// parameters JSON Schema（object 类型）。不包含 MCP 侧 meta 字段
// （agent_id/decision_epoch）——function calling 的参数就是 UE cmd 的参数。
// 额外追加一个可选的 time_to_stop（秒，MCP 侧控制字段，长动作定时终止）。
func capabilityParamsSchema(params []protocol.CapabilityParam) json.RawMessage {
	props := map[string]any{}
	required := make([]string, 0, len(params))
	for _, p := range params {
		prop := map[string]any{
			"type":        capabilityJSONSchemaType(p.Type),
			"description": p.Description,
		}
		if len(p.EnumValues) > 0 {
			prop["enum"] = p.EnumValues
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	// time_to_stop：长动作定时终止（MCP 侧轮询 game_time，不传 UE）。可选。
	props["time_to_stop"] = map[string]any{
		"type":        "number",
		"description": "长动作执行时长（秒），到点后系统打断该动作并进入下一轮；冥想/整理/上网等宜设较短时长（如 1800），工作/睡眠可不设",
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	return b
}

// capabilityJSONSchemaType 映射 CapabilityParam.Type 到 JSON Schema type。
// vector→"array"（UE5 [x,y,z]）；enum→"string"（配合 enum 字段）。
func capabilityJSONSchemaType(t string) string {
	switch t {
	case "string", "enum":
		return "string"
	case "number":
		return "number"
	case "bool":
		return "boolean"
	case "vector":
		return "array"
	default:
		return "string"
	}
}

// mapTacticalAction 把战术层 plannedAction 映射到 ws.SendAction 的 (cmd, params)。
// 复合工具 → 各自 Composite cmd；原子工具 → 各自 Atomic cmd。
// 映射规则与 composite.go/atomic.go 工具处理函数一致。非法/不可排队工具返回 err，调用方跳过。
//
// 新 12 cmd 体系（2026-08-11）：MoveTo 不再做 MCP 侧 KB 解析，UE 自己解析
// target_type + target_id/target_position。InteractSmartObject / 5 个复合 cmd
// 统一用 semantic_group 引用 world_kb 中对应 category 的物体 id（语义组名），
// auto_queue 作为 params 内字段传 "true"（复合）/ true（interact），符合真实 UE5
// capability_registry 声明。
//
// registry != nil 时，未匹配内置 case 的 action 走默认 passthrough 路径：
// 从 registry.EffectiveActions(agentID) 反查 cmd，params 原样转发。这覆盖
// UE 通过 capability_registry 新推送的 cmd（无强类型 Go struct，依赖通用工具
// 注册路径）。registry == nil 时默认分支返回 err（向后兼容旧测试）。
func mapTacticalAction(pa plannedAction, agentID string, kb *worldkb.KB, registry *CapabilityRegistry) (cmd string, params map[string]any, err error) {
	switch pa.Action {
	// ─── Composite tools → 各自 cmd ───
	case "work_shift":
		return protocol.CmdWorkShift, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     "true",
		}, nil
	case "charge_at_station":
		return protocol.CmdChargeAtStation, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     "true",
		}, nil
	case "self_maintenance":
		return protocol.CmdSelfMaintenance, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     "true",
		}, nil
	case "rest_at_residence":
		return protocol.CmdRestAtResidence, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     "true",
		}, nil
	case "surf_internet":
		return protocol.CmdSurfInternet, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     "true",
		}, nil
	case "social_chat":
		// Phase 2 Module C: proactive dialogue. params are target_agent_id
		// + content only — no semantic_group/interaction (target is an NPC,
		// not a Smart Object) and no auto_queue (not queueable).
		return protocol.CmdSocialChat, map[string]any{
			"target_agent_id": pa.Params["target_agent_id"],
			"content":         pa.Params["content"],
		}, nil
	// ─── Atomic tools ───
	case "generic_act":
		params := map[string]any{
			"thought": pa.Params["thought"],
		}
		if b, ok := pa.Params["behavior"].(string); ok && b != "" {
			params["behavior"] = b
		}
		return protocol.CmdGenericAct, params, nil
	case "move_to":
		params := map[string]any{}
		if t, ok := pa.Params["target_type"].(string); ok && t != "" {
			params["target_type"] = t
		}
		if id, ok := pa.Params["target_id"].(string); ok && id != "" {
			params["target_id"] = id
		}
		if pos, ok := pa.Params["target_position"].([]float64); ok && len(pos) > 0 {
			params["target_position"] = pos
		}
		return protocol.CmdMoveTo, params, nil
	case "turn_to":
		params := map[string]any{}
		if t, ok := pa.Params["target_type"].(string); ok && t != "" {
			params["target_type"] = t
		}
		if id, ok := pa.Params["target_id"].(string); ok && id != "" {
			params["target_id"] = id
		}
		if pos, ok := pa.Params["target_position"].([]float64); ok && len(pos) > 0 {
			params["target_position"] = pos
		}
		return protocol.CmdTurnTo, params, nil
	case "speak":
		return protocol.CmdSpeak, map[string]any{
			"content": pa.Params["content"],
		}, nil
	case "emote":
		return protocol.CmdEmote, map[string]any{
			"emotion": pa.Params["emotion"],
		}, nil
	case "InteractSmartObject", "interact": // interact 为旧工具名，保留兼容 LLM 偶发输出
		return protocol.CmdInteractSmartObject, map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     true,
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
	// 06:00-07:00 是战略规划时间（dayStartMinute=07:00），屏蔽战术层分解。
	// 避免 LLM 生成的夜间 slot（如 "22:00-07:00"）在 06:00-07:00 仍被
	// matchPlanSlot 的跨午夜分支命中（cur < end），导致战术层反复分解
	// 夜间睡眠任务。活动从 07:00 开始，此窗口内 NPC 保持空闲——若在途
	// composite 仍执行，由 advanceSlotIfNeeded 在 slot 过期时打断。
	cur := prompt.ParsePlanMinute(timeOfDay)
	if cur >= 0 && cur >= dayStartMinute-60 && cur < dayStartMinute {
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
