package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// plannedAction 是战术层分解出的单步 action，对应一个 MCP 工具调用。
// 类型定义已迁移到 pkg/agentstate（导出名 PlannedAction），此处保留
// alias 供 main 包过渡期使用，避免一次性重命名几十处引用。
type plannedAction = agentstate.PlannedAction

// ndjsonLine 是战术层 NDJSON 输出的单行判别联合体：当前仅支持 action 行
// （inner_thought 字段已废弃——内心独白改由 prompt 要求首个 action 为 speak 表达）。
// 保留 InnerThought 字段仅用于向后兼容解析旧 LLM 输出，解析时直接忽略。
type ndjsonLine struct {
	InnerThought string         `json:"inner_thought,omitempty"` // deprecated, 解析时忽略
	Action       string         `json:"action,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
}

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
// 否则返回 (origGoal, false)。
func physicalAlertOverrideGoal(hint, origGoal string, physical *protocol.PhysicalState) (string, bool) {
	if !strings.Contains(hint, "物理状态告警") || physical == nil || physical.IsZero() {
		return origGoal, false
	}
	switch {
	case physical.Fatigue > prompt.FatigueAlertThreshold:
		return "前往充电站休息补能（疲劳过高，停止工作）", true
	case physical.Energy < prompt.EnergyAlertThreshold:
		return "前往充电站补能（体力过低）", true
	case physical.JointWear > prompt.JointWearAlertThreshold:
		return "前往维护点进行保养检修（关节磨损过高）", true
	default:
		return origGoal, false
	}
}

// generateTacticalPlan 调战术层 LLM 分解当前时段 goal（非流式路径）。
// 返回分解出的 action 列表。
// 任一步失败返回 err，调用方决定回退兜底。
// 复用 strategicCaller 接口（venus.Client 已满足）。
func generateTacticalPlan(
	ctx context.Context,
	tc strategicCaller,
	agentID string,
	goal, zone, timeOfDay, slot string,
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
) ([]plannedAction, error) {
	var capActions []protocol.CapabilityAction
	if registry != nil {
		capActions = registry.EffectiveActions(agentID)
	}
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:          goal,
		Zone:          zone,
		TimeOfDay:     timeOfDay,
		Slot:          slot,
		Physical:      physical,
		KB:            kb,
		Profiles:      profiles,
		Hint:          hint,
		Memories:      memories,
		Relationships: relationships,
		Actions:       capActions,
		AgentID:       agentID,
		ObjectStatus:  objectStatus,
		NearbyObjects: nearbyObjects,
	})
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", promptText,
		"replan_hint", hint)

	resp, err := tc.SendWithSummary(ctx, promptText, "")
	if err != nil {
		return nil, fmt.Errorf("tactical llm: %w", err)
	}
	tc.ResetSession() // 战术调用一次性，立即清链（与战略层一致）

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	actions, err := parseTacticalNDJSON(raw, registry, agentID)
	if err != nil {
		return nil, fmt.Errorf("tactical parse: %w (raw=%s)", err, truncateText(raw, 200))
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"actions", string(actionsJSON))
	return actions, nil
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
	profiles map[string]*profile.Profile,
	logger *slog.Logger,
	hint string,
	memories string,
	relationships string,
	registry *CapabilityRegistry,
	objectStatus map[string]protocol.ObjectCategoryStatus,
	nearbyObjects []protocol.NearbyObject,
	onAction func(plannedAction),
) ([]plannedAction, error) {
	var capActions []protocol.CapabilityAction
	if registry != nil {
		capActions = registry.EffectiveActions(agentID)
	}
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:          goal,
		Zone:          zone,
		TimeOfDay:     timeOfDay,
		Slot:          slot,
		Physical:      physical,
		KB:            kb,
		Profiles:      profiles,
		Hint:          hint,
		Memories:      memories,
		Relationships: relationships,
		Actions:       capActions,
		AgentID:       agentID,
		ObjectStatus:  objectStatus,
		NearbyObjects: nearbyObjects,
	})
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "text", promptText,
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

	resp, err := tc.SendStreaming(ctx, promptText, func(delta string) {
		acc.feed(delta)
	})
	if err != nil {
		logger.Warn("[LLM→MCP/TACTICAL-STREAM] stream error, keeping actions already parsed",
			"agent_id", agentID, "parsed_actions", len(actions), "err", err)
		return actions, fmt.Errorf("tactical llm stream: %w", err)
	}
	acc.flush()
	tc.ResetSession()

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw, "streaming", true)

	if len(actions) == 0 {
		return nil, fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"actions", string(actionsJSON))
	return actions, nil
}

// parseTacticalNDJSON 从 LLM 的 NDJSON 输出解析 action 列表。
// 容错：剥 ```json 围栏 → 按行解析 → 跳过空行/parse 失败行 → 过滤非法工具。
// 返回的 actions 已经过 filterValidActions。
//
// registry 非 nil 时，过滤还会剔除依赖的 cmd 在 registry 中对 agentID 不可用
// 的工具（与 prompt 工具列表保持一致）。
//
// 内心独白已不再通过 inner_thought 字段传递——prompt 要求首个 action 直接是
// speak。若 LLM 仍输出 inner_thought 行（向后兼容），该行被静默忽略。
func parseTacticalNDJSON(raw string, registry *CapabilityRegistry, agentID string) ([]plannedAction, error) {
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
	for _, line := range strings.Split(s, "\n") {
		pa, isAction, ok := parseTacticalNDJSONLine(line)
		if !ok || !isAction {
			continue // 跳过空行、parse 失败行、inner_thought 行（向后兼容）
		}
		actions = append(actions, pa)
	}
	actions = filterValidActions(actions, registry, agentID)
	return actions, nil
}

// parseTacticalNDJSONLine 解析单行 NDJSON。返回 (action, isAction, ok)。
// ok=false 表示空行或 parse 失败（调用方跳过）。isAction=true 表示该行是 action；
// isAction=false 且 ok=true 表示该行是 inner_thought（向后兼容，调用方忽略）。
func parseTacticalNDJSONLine(line string) (pa plannedAction, isAction bool, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return plannedAction{}, false, false
	}
	var nl ndjsonLine
	if err := json.Unmarshal([]byte(line), &nl); err != nil {
		return plannedAction{}, false, false
	}
	if nl.Action != "" {
		return plannedAction{Action: nl.Action, Params: nl.Params}, true, true
	}
	// inner_thought 行（已废弃）：ok=true 但 isAction=false，调用方跳过
	if nl.InnerThought != "" {
		return plannedAction{}, false, true
	}
	return plannedAction{}, false, false
}

// streamAccumulator 是流式回调的增量 NDJSON 解析器。
// feed(delta) 追加 delta 到内部 buffer，按 \n 分割出完整行并即时解析；
// 最后一行（可能不完整）保留在 buffer 等下次 feed 补全。
// flush() 在流结束后调用，处理 buffer 中的残余内容。
type streamAccumulator struct {
	buf        strings.Builder
	onComplete func(plannedAction) // 每完整解析出一个合法 action 调一次
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

// processLine 解析单行：合法 action 调 onComplete；inner_thought 行（向后兼容）忽略。
func (a *streamAccumulator) processLine(line string) {
	pa, isAction, ok := parseTacticalNDJSONLine(line)
	if !ok || !isAction {
		return // 跳过空行、parse 失败行、inner_thought 行
	}
	if !tacticalActionAvailable(pa.Action, a.agentID, a.registry) {
		return // 过滤非法工具或依赖 cmd 不可用的工具（与 parseTacticalNDJSON 一致）
	}
	if a.onComplete != nil {
		a.onComplete(pa)
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
	case "interact":
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
