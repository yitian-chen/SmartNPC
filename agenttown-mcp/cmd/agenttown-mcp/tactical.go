package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// plannedAction 是战术层分解出的单步 action，对应一个 MCP 工具调用。
// 类型定义已迁移到 pkg/agentstate（导出名 PlannedAction），此处保留
// alias 供 main 包过渡期使用，避免一次性重命名几十处引用。
type plannedAction = agentstate.PlannedAction

// maxTacticalRetries 是战术层对 venus 4001（tools JSON 校验失败）的相同请求体重试上限。
// 4001 是 venus 侧校验响应失败，重试相同请求体通常能绕开瞬时坏输出；超时/连接错误等
// 其他失败不重试，交给调用方兜底（speak+look_around + 下一感知周期再分解）。
const maxTacticalRetries = 3

// isVenusErrorCode 判断 venus 网关返回的错误码。错误消息形如
// "venus status 500: {\"error\":{...,\"code\":\"4001\",...}}"，按 "code":"<code>" 匹配。
func isVenusErrorCode(err error, code string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), `"code":"`+code+`"`)
}

// isRateLimited 判断 venus 网关限流错误：HTTP 429 或 venus 错误码 4029
// （"当前使用的是公共模型服务, 并发有限; 当前的限流为: 30/min"）。
// 此类错误退避等待后重试可恢复（限流窗口按分钟滚动）。
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	return isVenusErrorCode(err, "4029") || strings.Contains(err.Error(), "status 429")
}

// actionSource 标识一个在途 action 由哪一层下发，决定 completion 后的路由。
// 类型定义已迁移到 pkg/agentstate（导出名 ActionSource），此处保留 alias。
type actionSource = agentstate.ActionSource

const (
	sourceTool     actionSource = agentstate.SourceTool
	sourceTactical actionSource = agentstate.SourceTactical
)

// maskedTacticalTools 是战术层 function calling 目录中**屏蔽**的工具（不在
// tools 数组里下发、校验时也拒绝）。分两类：
//   - 复合/快捷工具（work_shift / charge_at_station / rest_at_residence /
//     self_maintenance / surf_internet / use_exercise_equipment / read）与
//     InteractSmartObject 功能重叠——system prompt 的设施详情已列出所有
//     (semantic_group, interaction) 组合，模型统一用 InteractSmartObject 即可，
//     屏蔽这些快捷工具可把 tools 数组从 15 个瘦到 6 个（省 ~62%）。
//   - turn_to / emote 为非必要瞬时动作，一并屏蔽。
//   - scan_area / stop / wait 本就不属于战术层排队工具。
//
// 目录派生（tacticalToolsFromRegistry）与校验（tacticalActionAvailable）
// 共用这一份清单，保证「不展示 = 拒绝」一致。
var maskedTacticalTools = map[string]bool{
	"scan_area": true, "stop": true, "wait": true,
	"work_shift": true, "use_exercise_equipment": true,
	"charge_at_station": true, "read": true,
	"rest_at_residence": true, "self_maintenance": true,
	"surf_internet": true, "turn_to": true, "emote": true,
}

// tacticalActionAvailable 判断 action 是否为战术层可用工具，且其依赖的
// cmd 在 registry 中对 agentID 有效。registry == nil 时降级为仅检查是否
// 内置战术工具（向后兼容测试与未启用 capability 的场景）。
//
// 被 maskedTacticalTools 屏蔽的工具无论 registry 是否 nil 都返回 false。
func tacticalActionAvailable(action, agentID string, registry *CapabilityRegistry) bool {
	if maskedTacticalTools[action] {
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

// generateTacticalPlan 调战术层 LLM 分解当前时段 goal（统一 agentic loop
// 战术轮）。会话历史由 ac.agenticTurn 统一读写（[system, ...当天历史, user]，
// 历史含战略轮与对话轮），成功后 user+assistant 自动追加进历史，tool 结果由
// recordActionCompletion 回填。当天同计划的第二条及后续轮次走精简形态
// （省略【全天日程】与完整规则，见 TacticalInput.Compact）。返回分解出的
// action 段；任一步失败返回 err，调用方决定回退兜底。
func generateTacticalPlan(
	ctx context.Context,
	ac *agentContext,
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
) ([]plannedAction, error) {
	// P1-4（§3.3 唯一盲区）：注册本次 LLM 调用的取消句柄——force 事件到达时
	// 由 handleForceEvent 同步掐掉（半截思考丢弃：agenticTurn 成功才落历史），
	// 被取消方经 cancelledByForce 判定后跳过失败兜底。三个调用方（worker
	// tacticalRefill / tacticalRefillForReplan / /debug/schedule）共用本咽喉点。
	ctx, deregLLM := ac.registerTacticalLLMCall(ctx)
	defer deregLLM()

	// 精简引用判定：当天同一 dailyPlan 的全量头（【全天日程】+完整
	// 【分解规则】）已在本日会话历史中（由上一次成功全量轮写入
	// tacticalHeaderPlan 标记）→ 本轮省略日内不变块，改为核心约束速览 +
	// 引用行。dailyPlan==""（/debug/schedule 手动分解）永不精简：手动
	// 调试要确定性，且 auto-plan=false 时历史可能从无全量头，纯引用会
	// 指向不存在的规则。计划变化（日内重规划）→ 比对不等 → 重新全量
	// 注入。单次读取存局部变量，判定与置位复用同一值。
	// P3-7 安全点 drain（§4.4/§5.1 第三输入）：攒下的非 force 事件一次性
	// 全取注入本轮战术 prompt。快照注入 + 成功后清空——LLM 失败/被 force
	// 取消时事件保留在队列，下一次分解重新看到（事件是决策输入，不因一次
	// 失败调用丢弃）。drain 在本咽喉点覆盖三个调用方：worker tacticalRefill
	// / tacticalRefillForReplan（force 与路由打断的重规划）/ /debug/schedule。
	worldEvents := ac.as.WorldEventQueueSnapshot()
	// P3-9 修复 A：持续情境注入【当前处境】段（combat_exit/TTL 解除前
	// 每轮可见——防"威胁解除了"幻觉）。
	situations := formatActiveSituations(ac.as.Snapshot().ActiveSituations, ac.as.LatestGameTimeSec())
	headerPlan := ac.as.TacticalHeaderPlan()
	compact := dailyPlan != "" && headerPlan == dailyPlan
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:          goal,
		Zone:          zone,
		TimeOfDay:     timeOfDay,
		Slot:          slot,
		DailyPlan:     dailyPlan,
		Compact:       compact,
		Physical:      physical,
		KB:            kb,
		Profiles:      profiles,
		Hint:          hint,
		Memories:      memories,
		Relationships: relationships,
		Events:        prompt.FormatWorldEventList(worldEvents),
		Situations:    situations,
		AgentID:       agentID,
		ObjectStatus:  objectStatus,
		NearbyObjects: nearbyObjects,
		VisibleAgents: visibleAgents,
	})
	logger.Info("[MCP→LLM/TACTICAL-PROMPT]",
		"agent_id", agentID, "goal", goal, "game_time", timeOfDay, "compact", compact, "text", promptText,
		"replan_hint", hint, "history_turns", len(ac.as.Conversation()))

	// 统一 agentic loop 战术轮：tool_choice=required（必须调用工具），
	// 4001 重试与历史追加由 agenticTurn 统一处理。
	resp, err := ac.agenticTurn(ctx, ac.tacticalHc, kb, profiles, logger, agentID,
		"tactical", promptText, "required", "", nil)
	if err != nil {
		return nil, fmt.Errorf("tactical llm: %w", err)
	}
	// 全量轮成功：含全量头的 user 消息已由 agenticTurn 落进历史，此刻
	// 置位。放在 parse 校验之前——即使后续 tool_calls 解析失败，全量头
	// 也确已在历史中，下一轮走精简是安全的（置位 ⟺ 全量 user 消息
	// 已在历史）。
	if !compact && dailyPlan != "" {
		ac.as.SetTacticalHeaderPlan(dailyPlan)
	}

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/TACTICAL-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw,
		"tool_calls", len(resp.ToolCalls))

	if len(resp.ToolCalls) == 0 {
		llmMetricsCollector.RecordJSON("tactical", false)
		return nil, fmt.Errorf("tactical plan has no tool calls (raw=%s)", truncateText(raw, 200))
	}
	actions := parseToolCalls(resp.ToolCalls, registry, agentID)
	if len(actions) == 0 {
		llmMetricsCollector.RecordJSON("tactical", false)
		return nil, fmt.Errorf("tactical plan has no actions (raw=%s)", truncateText(raw, 200))
	}
	// 分解成功：安全点 drain 完成，消费事件队列（一次性交出）。
	ac.as.ClearWorldEvents()
	// JSON 正确率埋点：agenticTurn 成功后（LLM 已返回 tool_calls）按
	// parseToolCalls 结果记 ok。venus 层的坏 JSON（4001）已在 agenticTurn
	// 记作 bad_json_4001 错误类别，此处只记 MCP 层解析结果。
	llmMetricsCollector.RecordJSON("tactical", true)
	actions = fillDefaultDurationForRest(actions)
	actions = fillDefaultDurationForWork(actions)
	actions = insertRestBetweenDuplicateWork(actions)
	actionsJSON, _ := json.Marshal(actions)
	logger.Info("[战术层] 分解成功",
		"agent_id", agentID, "steps", len(actions),
		"actions", string(actionsJSON))
	return actions, nil
}

// defaultRestDurationSec 是非队尾休息类动作的默认 duration（30 分钟）。
// LLM 常给工作段设 duration 却给中间的"长椅休息"漏设，导致休息段自然
// 持续到 slot 切换、卡住后续工作动作。此处为兜底，不依赖 LLM 自觉。
const defaultRestDurationSec = 1800

// fillDefaultDurationForRest 给队列中所有休息类动作（含队尾）补齐默认
// duration（30 分钟）。所有长动作必须设 duration——队尾不设会导致
// P3-8 延迟切换后 processSlotSwitch 无法靠 time_to_stop 终止它。
func fillDefaultDurationForRest(actions []plannedAction) []plannedAction {
	if len(actions) == 0 {
		return actions
	}
	for i := 0; i < len(actions); i++ {
		a := &actions[i]
		if a.Action != "InteractSmartObject" || !paramIs(a.Params, "interaction", "rest") {
			continue
		}
		if _, ok := a.Params["duration"]; ok {
			continue
		}
		if a.Params == nil {
			a.Params = map[string]any{}
		}
		a.Params["duration"] = defaultRestDurationSec
	}
	return actions
}

// paramIs 判断 Params 中 key 对应的字符串值是否等于 want。
func paramIs(params map[string]any, key, want string) bool {
	if params == nil {
		return false
	}
	v, ok := params[key]
	if !ok {
		return false
	}
	s, _ := v.(string)
	return s == want
}

// fallbackRetryActions 构造战术层 LLM 分解失败时的兜底动作序列：
// speak 告知网络波动 + generic_act(behavior=look_around) 原地观察，
// 避免队列空时 NPC 呆站。兜底动作执行期间形成自然退避——执行完成后
// completion 唤醒 worker 再重试分解，而不会每轮感知都立即重试。
func fallbackRetryActions() []plannedAction {
	return []plannedAction{
		{Action: "speak", Params: map[string]any{
			"content": "网络波动了，我稍等一下，正在重试……",
		}},
		{Action: "generic_act", Params: map[string]any{
			"behavior": "look_around",
			"thought":  "网络波动，原地观察等待重试",
			"duration": 30,
		}},
	}
}

// defaultWorkDurationSec 是非队尾工作动作的默认 duration（90 分钟）。
// LLM 偶尔会给中间的工作段漏设 duration，使其自然持续到 slot 切换、
// 卡住后续动作。此处兜底，不依赖 LLM 自觉。
const defaultWorkDurationSec = 5400

// workInteractions 是六种工种的交互动词（含 InteractSmartObject 直接工作）。
var workInteractions = map[string]bool{
	"assemble":   true, // 工作台装配
	"sort_cargo": true, // 分拣
	"dismantle":  true, // 拆解
	"debug":      true, // 调试
	"inspect":    true, // 质检
	"process":    true, // 加工
}

// isWorkAction 判断是否为"工作类"动作：InteractSmartObject + 工种交互动词
// （assemble/sort_cargo 等）。work_shift 分支保留兼容（已被目录屏蔽，但
// /debug/action 或历史队列仍可能含该动作）。
func isWorkAction(a *plannedAction) bool {
	if a.Action == "work_shift" {
		return true
	}
	if a.Action != "InteractSmartObject" {
		return false
	}
	inter := ""
	if v, ok := a.Params["interaction"].(string); ok {
		inter = v
	}
	return workInteractions[inter]
}

// fillDefaultDurationForWork 给队列中所有工作类动作（含队尾）补齐默认
// duration（90 分钟）。所有长动作必须设 duration——队尾不设会导致
// P3-8 延迟切换后 processSlotSwitch 无法靠 time_to_stop 终止它。
func fillDefaultDurationForWork(actions []plannedAction) []plannedAction {
	if len(actions) == 0 {
		return actions
	}
	for i := 0; i < len(actions); i++ {
		a := &actions[i]
		if !isWorkAction(a) {
			continue
		}
		if _, ok := a.Params["duration"]; ok {
			continue
		}
		if a.Params == nil {
			a.Params = map[string]any{}
		}
		a.Params["duration"] = defaultWorkDurationSec
	}
	return actions
}

// restSegmentBetweenWork 返回一个"相邻重复工作类动作之间"插入的休息/活动段，
// 随机三选一：原地拉伸（exercise/stretch）、长椅休息（InteractSmartObject bench
// rest）、散步（exercise/walk）。duration 用 defaultRestDurationSec（30 分钟）。
func restSegmentBetweenWork() plannedAction {
	switch rand.IntN(3) {
	case 0:
		return plannedAction{Action: "exercise", Params: map[string]any{"exercise_type": "stretch", "duration": defaultRestDurationSec}}
	case 1:
		return plannedAction{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "bench", "interaction": "rest", "duration": defaultRestDurationSec}}
	default:
		return plannedAction{Action: "exercise", Params: map[string]any{"exercise_type": "walk", "duration": defaultRestDurationSec}}
	}
}

// sameParam 判断两个 params map 的同一 string 参数是否相等（都缺失视为不等）。
func sameParam(a, b map[string]any, key string) bool {
	av, aok := a[key].(string)
	bv, bok := b[key].(string)
	return aok && bok && av == bv
}

// insertRestBetweenDuplicateWork 在相邻的"相同工作类动作（同 semantic_group +
// interaction，如连续两次 InteractSmartObject 去同一工作台装配）"之间随机插入
// 一个休息段，打破"连续两次工作同地点"。规则 4 已禁止但 LLM 常无视，此处做
// 执行层兜底。只处理非队尾的相邻对（队尾保持自然持续到时段切换，不插）。
func insertRestBetweenDuplicateWork(actions []plannedAction) []plannedAction {
	if len(actions) < 2 {
		return actions
	}
	out := make([]plannedAction, 0, len(actions)+2)
	for i, a := range actions {
		out = append(out, a)
		if i == len(actions)-1 {
			break
		}
		cur, nxt := a, actions[i+1]
		if isWorkAction(&cur) && isWorkAction(&nxt) &&
			sameParam(cur.Params, nxt.Params, "semantic_group") &&
			sameParam(cur.Params, nxt.Params, "interaction") {
			out = append(out, restSegmentBetweenWork())
		}
	}
	return out
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
// 描述与参数 schema 来自 CapabilityAction。跳过 maskedTacticalTools 屏蔽的
// 工具（复合/快捷工具与 InteractSmartObject 重叠，以及 scan_area/stop/wait）。
// registry == nil → nil（UE 未连接时请求体不带 tools）。工具清单仅经此
// 下发，不再注入 prompt 文本。
func tacticalToolsFromRegistry(registry *CapabilityRegistry, agentID string) []venus.Tool {
	if registry == nil {
		return nil
	}
	actions := registry.EffectiveActions(agentID)
	out := make([]venus.Tool, 0, len(actions))
	for _, act := range actions {
		name := tools.CmdToToolName(act.Cmd)
		if maskedTacticalTools[name] {
			continue
		}
		desc := act.Description
		if desc == "" {
			desc = name
		}
		// UsageHint 是 UE capability_registry 声明的"何时使用该工具"提示
		// （如"能量低时使用""磨损高或需要维护时使用"），对 LLM 在 function
		// calling 阶段选型有直接帮助。此前被遗漏——tools 字段只有动作描述、
		// 没有使用时机，LLM 只能靠 system prompt 里的设施详情间接推断。
		if act.UsageHint != "" {
			desc = strings.TrimRight(desc, "。") + "。" + strings.TrimRight(act.UsageHint, "。")
		}
		out = append(out, venus.Tool{
			Type: "function",
			Function: venus.ToolFunction{
				Name:        name,
				Description: desc,
				Parameters:  capabilityParamsSchema(act.Params, name),
			},
		})
	}
	return out
}

// noDurationTool 判断工具是否不追加 duration 参数。两类：
//   - 瞬时动作（speak/emote/turn_to/generic_act）：立即完成，无时长概念；
//   - move_to：移动时长由 UE 寻路决定，LLM 不设置（UE usage_hint 声明
//     "此动作无需传入 duration"）。
//
// 这些工具 schema 层不暴露 duration，LLM 无从填写，与 TacticalRules 规则 7
// "瞬时动作不填 duration、move_to 由 UE 决定"的约定一致。
func noDurationTool(name string) bool {
	switch name {
	case "speak", "emote", "turn_to", "generic_act", "move_to":
		return true
	}
	return false
}

// capabilityParamsSchema 把 CapabilityParam 列表转成 function calling 的
// parameters JSON Schema（object 类型）。不包含 MCP 侧 meta 字段
// （agent_id/decision_epoch）——function calling 的参数就是 UE cmd 的参数。
// 额外追加 duration（秒，MCP 侧控制字段，长动作定时终止）并列入 required
// （2026-09-03：实测 LLM 在 function calling 中只填 required 参数，optional
// 的 duration 从不被填——动作时长全靠 slot 切换兜底、队列频繁提前耗尽。
// 必填后 LLM 被迫为每个非瞬时动作声明时长；"最后一段不设 duration 自然
// 持续到 slot 切换"的旧约定改为"最后一段 duration = 时段剩余时长"，
// TacticalRules 规则 7/8 已同步措辞）。例外：social_chat 是"挂起直到对话
// 结束"的复合动作，duration 到点会打断对话；瞬时工具无时长概念。
func capabilityParamsSchema(params []protocol.CapabilityParam, name string) json.RawMessage {
	props := map[string]any{}
	required := make([]string, 0, len(params))
	for _, p := range params {
		desc := p.Description
		// 参数描述精简：enum 已约束合法值、system prompt 有完整设施详情，
		// 这里只保留防止 LLM 犯错的关键语义，去掉"固定为 X""如 xxx、yyy"等
		// 与 enum / system prompt 重复的内容。
		switch p.Name {
		case "zone":
			desc = "目标设施所在的 zone id。日程明确指定区域时必须填该区域 id（如 central_plaza、logistics_hub）；不填默认优先找 NPC 自己所在 zone 的设施。"
		case "semantic_group":
			desc = "设施语义组名（UE 从该组自动选一个空闲实例，勿传具体编号）"
		case "interaction":
			// 有 enum 的固定值工具，"固定为 X"与 enum 重复，可精简；无 enum
			// 的（如 InteractSmartObject）描述含 semantic_group↔interaction
			// 配对，删掉会丢关键信息，保留。
			if len(p.EnumValues) > 0 {
				desc = "交互动作类型（合法值见 enum）"
			}
		}
		prop := map[string]any{
			"type":        capabilityJSONSchemaType(p.Type),
			"description": desc,
		}
		if len(p.EnumValues) > 0 {
			prop["enum"] = p.EnumValues
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	// duration：非瞬时动作的持续时长（秒，MCP 侧轮询 game_time，不传 UE）。
	// 时长档位（中间动作约 1800 秒、工作段 3600-7200 秒、末段=剩余时长）已迁到
	// prompt 分解规则（TacticalRules 规则 7/8 + tacticalCoreRules 精简版），此处
	// 只留执行语义，避免 10 个工具重复一份长描述。
	// social_chat 不追加（对话挂起直到结束，duration 会打断对话）；
	// 瞬时工具 + move_to 不追加（见 noDurationTool）。
	if name != "social_chat" && !noDurationTool(name) {
		props["duration"] = map[string]any{
			"type":        "number",
			"description": "持续时长（秒）。到点后系统打断当前段并进入队列下一段；末段设为时段剩余时长。",
		}
		required = append(required, "duration")
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
		params := map[string]any{
			"semantic_group": pa.Params["semantic_group"],
			"interaction":    pa.Params["interaction"],
			"auto_queue":     true,
		}
		// zone 是 UE 明确支持的参数（capability_registry 声明），透传给 UE。
		// 否则"去中央广场长椅"等指定区域的日程会被落下到 NPC 所在 zone 的
		// 设施（如主生产车间的长椅），指定区域的意图落空。
		if z, ok := pa.Params["zone"].(string); ok && z != "" {
			params["zone"] = z
		}
		return protocol.CmdInteractSmartObject, params, nil
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
			// 复制 params 避免调用方误改原 map；剔除 duration——它是 MCP 侧
			// 控制字段（worker 按 game_time 定时打断消费），不透传 UE。
			out := make(map[string]any, len(pa.Params))
			for k, v := range pa.Params {
				if k == "duration" {
					continue
				}
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
