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
	ReactionReplan    ReactionKind = "replan"    // 触发战术层重新规划整个时段
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
	TriggerActionDone    ReactiveTrigger = "action_done"     // action_completed，自然评估点
	TriggerPeriodic      ReactiveTrigger = "periodic"        // 周期性触发：每 N 次感知强制评估
)

// 物理警戒带阈值。原 P0 设 energy<20 / health<30 / fatigue>80，但单日仿真
// energy 最低只到约 80、fatigue 最高约 48，警戒带从未触发。放宽到预警级别，
// 让 1-2 小时仿真就能自然触发物理类反应。
const (
	energyAlertThreshold  = 40.0 // energy 跌破此值触发"低电量预警"
	healthAlertThreshold  = 50.0 // health 跌破此值触发"健康预警"
	fatigueAlertThreshold = 60.0 // fatigue 突破此值触发"疲劳预警"
)

// periodicTriggerInterval 是周期性触发的感知次数间隔。
// mock_ue 每 15s 推一次 perception（normal 模式 60s，behavior 模式 15s），
// 设 4 次即每 60s（normal）或 60s（behavior）强制评估一次。
// 本地 Ollama 调用成本可承受，但太频繁会产出大量 continue 决策浪费算力。
const periodicTriggerInterval = 4

// ReactiveInput 聚合反应层决策所需的输入状态。由 main.go 从 agentContext
// 提取后传入，避免 reactive.go 直接依赖 agentContext（便于单元测试）。
type ReactiveInput struct {
	AgentID       string
	TimeOfDay     string // "HH:MM" 游戏时间
	Zone          string // 当前区域 id
	Energy        float64
	Fatigue       float64
	Health        float64
	CurrentAction string // 当前在途 action 的可读描述（如 "work_assemble(target=workbench_01)"），空=无在途
	ElapsedSec    int    // 当前 action 已执行秒数
	ActionSrc     string // 在途 action 来源：tactical / hermes / 空
	CurrentSlot   string // 当前战术时段 "HH:MM-HH:MM"，空=未分解
	DailyPlan     string // 战略层每日计划摘要（格式化字符串），空=未生成
	Trigger       ReactiveTrigger
	TriggerDetail string // 触发原因详情
}

// reactivePromptTemplate 是反应层 prompt 模板。用 fmt.Sprintf 填充。
// 中文 prompt（qwen2.5 中文表现好），严格约束 JSON 输出。
const reactivePromptTemplate = `你是 NPC 老陈的反应决策模块。当前情况需要你判断是否打断当前行动。

【当前状态】
游戏时间：%s
位置：%s
物理：体力=%.0f/100, 疲劳=%.0f/100, 健康=%.0f/100

【在途动作】
%s
来源：%s（战术层规划的动作为深思熟虑的结果，非必要不打断）

【战术层上下文】
当前时段：%s
每日计划摘要：
%s

【触发原因】
%s

【可选反应】
- continue：不打断，让当前行动继续
- observe：不打断，记录这个事件供后续参考
- interrupt：打断当前行动（会发送 stop_action）
- act：打断当前行动并立即执行一个新动作
- replan：当前时段的整个战术规划已不合理（如时段目标与实际冲突、物理状态无法支撑剩余 action），请求战术层基于当前状态重新分解本时段 goal

判断要点：
- 战术层规划的动作通常是合理的，除非有明确理由，否则 continue
- 仅在物理状态告警、事件突发、或当前动作明显不合理时才 interrupt/act
- replan 是"重大"决策：当你认为整个 action 队列都应作废、重新规划时使用，而非单个 action 不合适（单个不合适用 interrupt/act）。30 分钟内至多触发 1 次 replan，请慎重

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"reaction": "continue|observe|interrupt|act|replan", "reason": "简短理由", "action": {"cmd": "...", "params": {...}}}

action 字段仅在 reaction=act 时填写，cmd 可选：move_to / speak / emote / wait / interact。
reaction=replan 时不要填 action 字段。
不要输出 JSON 以外的任何内容。`

// buildReactivePrompt 构造反应层 prompt。纯函数，便于测试。
func buildReactivePrompt(in ReactiveInput) string {
	currentAction := in.CurrentAction
	if currentAction == "" {
		currentAction = "无在途动作"
	}
	elapsed := in.ElapsedSec
	if elapsed < 0 {
		elapsed = 0
	}
	actionSrc := in.ActionSrc
	if actionSrc == "" {
		actionSrc = "无"
	}
	slot := in.CurrentSlot
	if slot == "" {
		slot = "未分解"
	}
	plan := in.DailyPlan
	if plan == "" {
		plan = "（未生成）"
	}
	detail := in.TriggerDetail
	if detail == "" {
		detail = string(in.Trigger)
	}
	return fmt.Sprintf(reactivePromptTemplate,
		in.TimeOfDay, in.Zone,
		in.Energy, in.Fatigue, in.Health,
		currentAction,
		actionSrc,
		slot,
		plan,
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
	case ReactionContinue, ReactionObserve, ReactionInterrupt, ReactionAct, ReactionReplan:
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
// 本地模型成本可承受，但仍需避免无意义的高频调用（大部分决策会是 continue）。
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
		if !belowThreshold(prevPhysical.Energy, energyAlertThreshold) && belowThreshold(curPhysical.Energy, energyAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("energy %.0f→%.0f 跌破警戒带 %.0f", prevPhysical.Energy, curPhysical.Energy, energyAlertThreshold)
		}
		if !belowThreshold(prevPhysical.Health, healthAlertThreshold) && belowThreshold(curPhysical.Health, healthAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("health %.0f→%.0f 跌破警戒带 %.0f", prevPhysical.Health, curPhysical.Health, healthAlertThreshold)
		}
		if !aboveThreshold(prevPhysical.Fatigue, fatigueAlertThreshold) && aboveThreshold(curPhysical.Fatigue, fatigueAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("fatigue %.0f→%.0f 突破警戒带 %.0f", prevPhysical.Fatigue, curPhysical.Fatigue, fatigueAlertThreshold)
		}
	}
	return "", ""
}

// belowThreshold 返回 v < threshold（严格小于）。
func belowThreshold(v, threshold float64) bool { return v < threshold }

// aboveThreshold 返回 v > threshold（严格大于）。
func aboveThreshold(v, threshold float64) bool { return v > threshold }

// shouldTriggerPeriodic 判断是否到了周期性强制触发的时机。
//
// 本地 Ollama 模型调用成本可承受，但大部分 perception 无显著变化时决策会是
// continue，频繁调用浪费算力。每 periodicTriggerInterval 次感知强制触发一次，
// 让反应层有机会评估"是否应该换活动"、"已经工作太久该休息了"等长周期决策。
//
// perceptionCount 是累计感知次数（从 1 开始），返回 (TriggerPeriodic, detail) 或 ("", "")。
// 调用方负责传入累计计数；此函数是纯函数，便于测试。
func shouldTriggerPeriodic(perceptionCount int) (ReactiveTrigger, string) {
	if perceptionCount <= 0 {
		return "", ""
	}
	if perceptionCount%periodicTriggerInterval == 0 {
		return TriggerPeriodic, fmt.Sprintf("周期性评估（第 %d 次感知，每 %d 次触发）", perceptionCount, periodicTriggerInterval)
	}
	return "", ""
}

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
// 事件类触发（zone/objects/physical/event）用 60s——与文档决策 3 一致。
// 周期性触发（periodic）用 45s——比事件类短，确保 NPC 在静止场景中也能
// 定期评估；但不会与事件类触发互相干扰（trigger 类型不同，dedupe key 不同）。
const reactiveDedupeWindow = 60 * time.Second

// reactivePeriodicDedupeWindow 是周期性触发的去抖窗口。
// 比 60s 短，确保感知频率较低（如 normal 模式 60s 一次）时也能定期触发；
// 但要有一定去抖，避免感知频率高（如 behavior 模式 15s 一次）时过度调用。
const reactivePeriodicDedupeWindow = 45 * time.Second

// replanDedupeWindow 限制 reaction=replan 的触发频率：wall-clock 30 分钟内至多 1 次。
// 不做倍率换算——"至多一次"约束的是反应层决策频率（wall-clock 时间轴上的 Ollama
// 调用、goroutine 调度），与游戏时间倍率无关。150x 下 30 wall-clock 分钟 ≈ 75 游戏小时，
// 远超单日仿真（16 游戏小时 ≈ 6.4 wall-clock 分钟），确保 replan 是罕见重大事件。
// 该去抖在 execute() 的 replan 分支内检查（不在 trigger() 第一层），按 agent 全局，
// 不按 trigger/detail——replan 是 agent 级决策，不是单个触发的事件。
const replanDedupeWindow = 30 * time.Minute
