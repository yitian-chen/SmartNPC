// Package main — reactive layer.
//
// 反应层用本地 Ollama 模型（qwen2.5:7b）低延迟处理突发事件，决策是否
// 打断战术层在途 action。与战略/战术层独立，无会话链累积，每轮独立 prompt。
//
// 决策输出四种反应：continue / observe / interrupt / act。
// 详见 docs/AgentTown_Reactive_Layer.md。

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// ReactionKind 枚举反应层决策类型。
type ReactionKind string

const (
	ReactionContinue  ReactionKind = "continue"  // 不打断，让当前行动继续
	ReactionObserve   ReactionKind = "observe"   // 不打断，记录事件供战术层参考
	ReactionInterrupt ReactionKind = "interrupt" // 打断当前 action（发 stop_action）
	ReactionAct       ReactionKind = "act"       // 打断并立即执行新 action
)

// ReactiveDecision 是反应层期望从 Ollama 拿到的 JSON 决策。
type ReactiveDecision struct {
	Reaction ReactionKind    `json:"reaction"`
	Reason   string          `json:"reason"`
	Action   *ReactionAction `json:"action,omitempty"`
}

// ReactionAction 是 reaction=act 时要立即下发的动作。
type ReactionAction struct {
	Cmd    string                 `json:"cmd"`
	Params map[string]interface{} `json:"params"`
}

// ReactiveTrigger 标识触发原因，用于去抖和日志。
type ReactiveTrigger string

const (
	TriggerZoneChange    ReactiveTrigger = "zone_change"     // NPC 进入新区域
	TriggerNewObject     ReactiveTrigger = "new_object"      // nearby_objects 出现新物体
	TriggerEventNotify   ReactiveTrigger = "event_notify"    // 收到 event_notification
	TriggerPhysicalAlert ReactiveTrigger = "physical_alert"  // 物理状态突破警戒带
)

// ReactiveInput 聚合反应层决策所需的输入状态。由 main.go 从 agentContext
// 提取后传入，避免 reactive.go 直接依赖 agentContext（便于单元测试）。
type ReactiveInput struct {
	AgentID      string
	TimeOfDay    string          // "HH:MM" 游戏时间
	Zone         string          // 当前区域 id
	Energy       float64         // 0-100
	Fatigue      float64         // 0-100
	Health       float64         // 0-100
	CurrentAction string         // 当前在途 action 描述（空=无在途）
	ElapsedSec   int             // 当前 action 已执行秒数
	Trigger      ReactiveTrigger
	TriggerDetail string         // 触发原因详情（如 "energy 18→15 跌破警戒带 20"）
}

// reactivePromptTemplate 是反应层 prompt 模板。用 fmt.Sprintf 填充。
// 中文 prompt（qwen2.5 中文表现好），严格约束 JSON 输出。
const reactivePromptTemplate = `你是 NPC 老陈的反应决策模块。当前情况需要你判断是否打断当前行动。

【当前状态】
时段：%s
位置：%s
物理：体力=%.0f/100, 疲劳=%.0f/100, 健康=%.0f/100
在途动作：%s（已执行 %d 秒）

【触发原因】
%s

【可选反应】
- continue：不打断，让当前行动继续
- observe：不打断，记录这个事件供后续参考
- interrupt：打断当前行动（会发送 stop_action）
- act：打断当前行动并立即执行一个新动作

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"reaction": "continue|observe|interrupt|act", "reason": "简短理由", "action": {"cmd": "...", "params": {...}}}

action 字段仅在 reaction=act 时填写，cmd 可选：move_to / speak / emote / wait / interact。
不要输出 JSON 以外的任何内容。`

// buildReactivePrompt 构造反应层 prompt。纯函数，便于测试。
func buildReactivePrompt(in ReactiveInput) string {
	currentAction := in.CurrentAction
	if currentAction == "" {
		currentAction = "无在途动作"
	}
	detail := in.TriggerDetail
	if detail == "" {
		detail = string(in.Trigger)
	}
	return fmt.Sprintf(reactivePromptTemplate,
		in.TimeOfDay, in.Zone,
		in.Energy, in.Fatigue, in.Health,
		currentAction, in.ElapsedSec,
		detail,
	)
}

// parseReactiveDecision 解析 Ollama 输出为 ReactiveDecision。
//
// 容错策略（与文档 P0.3 一致）：
//   - JSON 解析失败 → 视为 continue（最保守）
//   - reaction 字段不在枚举内 → 视为 continue
//   - act 但 action 为空或 cmd 非法 → 降级为 interrupt
//   - action.params 不完整 → 保留原样，由调用方在发 action 时兜底
func parseReactiveDecision(raw string) ReactiveDecision {
	// Ollama 有时会输出 ```json ... ``` 包裹的 JSON，提取出来。
	cleaned := stripCodeFence(raw)
	dec := ReactiveDecision{Reaction: ReactionContinue} // 默认 continue
	if err := json.Unmarshal([]byte(cleaned), &dec); err != nil {
		// 解析失败 → 默认 continue
		return ReactiveDecision{Reaction: ReactionContinue, Reason: "parse_failed: " + truncate(err.Error(), 80)}
	}
	// 枚举校验
	switch dec.Reaction {
	case ReactionContinue, ReactionObserve, ReactionInterrupt, ReactionAct:
		// ok
	default:
		return ReactiveDecision{Reaction: ReactionContinue, Reason: "unknown_reaction: " + string(dec.Reaction)}
	}
	// act 但 action 缺失/非法 → 降级 interrupt
	if dec.Reaction == ReactionAct {
		if dec.Action == nil || !isValidReactionCmd(dec.Action.Cmd) {
			dec.Reaction = ReactionInterrupt
			dec.Reason = "act_downgrade: " + dec.Reason
			dec.Action = nil
		}
	}
	return dec
}

// stripCodeFence 去除 Ollama 输出可能包裹的 ```json ... ``` 围栏。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 去掉首行 ```json 或 ```
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[idx+1:]
	}
	// 去掉末尾 ```
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// isValidReactionCmd 校验 reaction action 的 cmd 是否在允许列表内。
// 仅原子短动作（不含复合动作——复合动作应由战术层规划）。
func isValidReactionCmd(cmd string) bool {
	switch cmd {
	case "move_to", "speak", "emote", "wait", "interact":
		return true
	}
	return false
}

// truncate 截断字符串到 maxLen，超出加省略号。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// shouldTriggerReactive 判断感知/状态变化是否"显著"到需要调用反应层。
// 与决策 3 一致：只在显著变化时调用，避免每个 perception_update 都打 Ollama。
//
// 返回 (trigger, detail) — 若不显著则 trigger 为空字符串。
func shouldTriggerReactive(
	prevZone, curZone string,
	prevObjectIDs, curObjectIDs []string,
	prevPhysical, curPhysical *protocol.PhysicalState,
) (ReactiveTrigger, string) {
	// 1. zone 变化
	if prevZone != curZone && curZone != "" {
		return TriggerZoneChange, fmt.Sprintf("zone %s→%s", prevZone, curZone)
	}
	// 2. 新物体出现
	newObjs := diffStrings(curObjectIDs, prevObjectIDs)
	if len(newObjs) > 0 {
		return TriggerNewObject, fmt.Sprintf("new objects: %s", strings.Join(newObjs, ","))
	}
	// 3. 物理状态突破警戒带（从正常跨入警戒带的那一刻）
	if prevPhysical != nil && curPhysical != nil {
		if !belowThreshold(prevPhysical.Energy, 20) && belowThreshold(curPhysical.Energy, 20) {
			return TriggerPhysicalAlert, fmt.Sprintf("energy %.0f→%.0f 跌破警戒带 20", prevPhysical.Energy, curPhysical.Energy)
		}
		if !belowThreshold(prevPhysical.Health, 30) && belowThreshold(curPhysical.Health, 30) {
			return TriggerPhysicalAlert, fmt.Sprintf("health %.0f→%.0f 跌破警戒带 30", prevPhysical.Health, curPhysical.Health)
		}
		if !aboveThreshold(prevPhysical.Fatigue, 80) && aboveThreshold(curPhysical.Fatigue, 80) {
			return TriggerPhysicalAlert, fmt.Sprintf("fatigue %.0f→%.0f 突破警戒带 80", prevPhysical.Fatigue, curPhysical.Fatigue)
		}
	}
	return "", ""
}

// belowThreshold 返回 v < threshold（严格小于）。
func belowThreshold(v, threshold float64) bool { return v < threshold }

// aboveThreshold 返回 v > threshold（严格大于）。
func aboveThreshold(v, threshold float64) bool { return v > threshold }

// diffStrings 返回 in 中存在但 notIn 中不存在的字符串（in - notIn）。
func diffStrings(in, notIn []string) []string {
	set := make(map[string]struct{}, len(notIn))
	for _, s := range notIn {
		set[s] = struct{}{}
	}
	var out []string
	for _, s := range in {
		if _, ok := set[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// extractObjectIDs 从 perception 的 NearbyObjects 提取 object id 列表。
// 反应层用它对比前后两次感知，检测"新物体出现"触发。
// 空 ID 跳过（防御性：mock_ue 偶发输出无 id 的对象）。
func extractObjectIDs(p protocol.PerceptionPayload) []string {
	ids := make([]string, 0, len(p.NearbyObjects))
	for _, obj := range p.NearbyObjects {
		if obj.ID != "" {
			ids = append(ids, obj.ID)
		}
	}
	return ids
}

// dedupeKey 构造去抖用的唯一键（agentID + trigger + detail）。
// 相同 key 在 dedupeWindow 内不重复触发。
func dedupeKey(agentID string, trigger ReactiveTrigger, detail string) string {
	return agentID + "|" + string(trigger) + "|" + detail
}

// reactiveDedupeWindow 是相同触发原因的去抖窗口。
// 60 秒——与文档决策 3 一致。
const reactiveDedupeWindow = 60 * time.Second
