// Package prompt — reactive layer prompt builder and trigger helpers.
//
// All functions here are pure: no side effects, no I/O, no mutexes. The
// reactive runner (cmd/agenttown-mcp/reactive_runner.go) calls these to
// build prompts, parse Ollama output, and decide when to trigger. Decision
// execution (stop_action, tactical replan) stays in the main package.

package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// ReactionKind enumerates reactive layer decision types.
type ReactionKind string

const (
	ReactionContinue ReactionKind = "continue" // do not interrupt; let current action proceed
	ReactionObserve  ReactionKind = "observe"  // do not interrupt; record event for tactical layer
	ReactionReplan   ReactionKind = "replan"   // trigger tactical layer to replan the whole slot
)

// ReactiveDecision is the JSON decision expected from Ollama.
type ReactiveDecision struct {
	Reaction ReactionKind `json:"reaction"`
	Reason   string       `json:"reason"`
}

// Physical alert thresholds.
// fatigue threshold raised from 60 to 80 (paired with mock_ue fatigue rate
// reduction) so fatigue alerts fire naturally mid-afternoon (~14:00) rather
// than at 11:00. energy/health kept at early-warning levels so 1-2 hour
// simulations also trigger naturally.
const (
	EnergyAlertThreshold  = 40.0 // energy below this triggers "low battery alert"
	HealthAlertThreshold  = 50.0 // health below this triggers "health alert"
	FatigueAlertThreshold = 80.0 // fatigue above this triggers "fatigue alert"
)

// PeriodicTriggerInterval is the perception count interval for periodic
// forced triggers. mock_ue pushes perception every 15s (normal 60s, behavior
// 15s), so 4 intervals = every 60s. Local Ollama cost is tolerable, but too
// frequent produces many continue decisions wasting compute.
const PeriodicTriggerInterval = 4

// ReactiveDedupeWindow is the dedupe window for event-type triggers
// (zone/objects/physical/event). Matches doc decision 3.
const ReactiveDedupeWindow = 60 * time.Second

// ReactivePeriodicDedupeWindow is the dedupe window for periodic triggers.
// Shorter than 60s to ensure periodic evaluation fires even at lower
// perception frequencies (normal mode 60s); but still debounced to avoid
// excessive calls at high frequencies (behavior mode 15s).
const ReactivePeriodicDedupeWindow = 45 * time.Second

// ReplanDedupeGameMinutes limits reaction=replan frequency: at most 1 per
// game hour. Uses game time (not wall-clock) because simulation rate can
// reach 600x, where a wall-clock window would be amplified to tens of game
// hours, blocking legitimate replans for an entire day. 1 game hour ensures
// physical alerts in different game slots can each trigger a valid replan.
// This dedupe is checked inside execute() (not trigger()'s first layer),
// keyed per-agent globally, not per trigger/detail — replan is an agent-level
// decision, not a single trigger event.
const ReplanDedupeGameMinutes = 60

// ReactivePromptTemplate is the reactive layer prompt template.
// Filled via fmt.Sprintf. Chinese prompt (qwen2.5 performs better in Chinese),
// strict JSON output constraint.
//
// agentName is injected by the caller to avoid hardcoding "老陈" or other
// specific NPC names — the reactive layer should serve any agent.
// agentRole is injected by the caller from KB (AgentRole); empty string
// degrades the segment to "（无角色信息）" — keeps the placeholder so the LLM
// knows the field exists but is currently unavailable.
// physicalLine is injected by the caller: empty when UE hasn't implemented
// physical state (prompt omits physical segment); non-empty looks like
// "物理：体力=X/100, 疲劳=Y/100, 健康=Z/100".
// physicalRuleLine is injected by the caller: physical alert judgment points
// when physical state is available; empty skips.
const ReactivePromptTemplate = `你是 NPC %s 的反应决策模块。当前情况需要你判断是否打断当前行动进行重规划。

【你的角色】
%s

【当前状态】
游戏时间：%s
位置：%s
%s
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
- replan：当前时段的整个战术规划已不合理（如物理状态无法支撑剩余 action、感知到新的 object / agent 希望改变原来的计划转而与之互动），请求战术层基于当前状态重新分解本时段 goal

判断要点：
- 战术层规划的动作通常是合理的，除非有明确理由，否则 continue
%s- replan 是"重大"决策：当你认为整个 action 队列都应作废、重新规划时使用。

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"reaction": "continue|observe|replan", "reason": "简短理由"}

不要输出 JSON 以外的任何内容。`

// BuildReactive constructs the reactive layer prompt. Pure function.
func BuildReactive(in ReactiveInput) string {
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
	agentName := in.AgentName
	if agentName == "" {
		agentName = in.AgentID
	}
	agentRole := in.AgentRole
	if agentRole == "" {
		agentRole = "（无角色信息）"
	}
	// Physical segment: when PhysicalAvailable=false, prompt omits physical
	// segment and the "physical alert → replan" judgment point (avoid LLM
	// misjudging an empty physical segment).
	physicalLine := ""
	physicalRuleLine := ""
	if in.PhysicalAvailable {
		physicalLine = fmt.Sprintf("物理：体力=%.0f/100, 疲劳=%.0f/100, 健康=%.0f/100\n", in.Energy, in.Fatigue, in.Health)
		physicalRuleLine = "- 物理状态告警时（体力<40、疲劳>80、健康<50）原则上需要输出 replan 让 NPC 休息/充电、不可输出 continue/observe\n"
	}
	return fmt.Sprintf(ReactivePromptTemplate,
		agentName,
		agentRole,
		in.TimeOfDay, in.Zone,
		physicalLine,
		currentAction,
		actionSrc,
		slot,
		plan,
		detail,
		physicalRuleLine,
	)
}

// ParseReactiveDecision parses Ollama output into ReactiveDecision.
//
// Fault tolerance (matches doc P0.3):
//   - JSON parse failure → continue (most conservative)
//   - reaction field not in enum → continue
//
// The reactive layer only supports continue/observe/replan; historically
// existing interrupt/act are removed, and if the model still outputs those
// values they degrade to continue.
func ParseReactiveDecision(raw string) ReactiveDecision {
	cleaned := StripCodeFence(raw)
	dec := ReactiveDecision{Reaction: ReactionContinue} // default continue
	if err := json.Unmarshal([]byte(cleaned), &dec); err != nil {
		// Parse failure → default continue
		return ReactiveDecision{Reaction: ReactionContinue, Reason: "parse_failed: " + truncate(err.Error(), 80)}
	}
	// Enum validation
	switch dec.Reaction {
	case ReactionContinue, ReactionObserve, ReactionReplan:
		// ok
	default:
		return ReactiveDecision{Reaction: ReactionContinue, Reason: "unknown_reaction: " + string(dec.Reaction)}
	}
	return dec
}

// StripCodeFence removes ```json ... ``` fences wrapping Ollama output.
func StripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Strip leading ```json or ```
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[idx+1:]
	}
	// Strip trailing ```
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// truncate truncates s to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// ShouldTriggerReactive determines if perception/state changes are
// "significant" enough to invoke the reactive layer. Matches decision 3:
// only invoke on significant changes to avoid hitting Ollama on every
// perception_update. Local model cost is tolerable but meaningless high-frequency
// calls (most decisions would be continue) should still be avoided.
//
// Returns (trigger, detail) — trigger is empty string if not significant.
func ShouldTriggerReactive(
	prevZone, curZone string,
	prevObjectIDs, curObjectIDs []string,
	prevPhysical, curPhysical *protocol.PhysicalState,
) (ReactiveTrigger, string) {
	// 1. zone change
	if prevZone != curZone && curZone != "" {
		return TriggerZoneChange, fmt.Sprintf("zone %s→%s", prevZone, curZone)
	}
	// 2. new object appeared
	newObjs := DiffStrings(curObjectIDs, prevObjectIDs)
	if len(newObjs) > 0 {
		return TriggerNewObject, fmt.Sprintf("new objects: %s", strings.Join(newObjs, ","))
	}
	// 3. physical state crossed alert threshold (the moment it crosses from normal into alert)
	//    When UE hasn't implemented physical state, prev/cur are all 0; IsZero guard skips to avoid false triggers.
	if prevPhysical != nil && curPhysical != nil &&
		!prevPhysical.IsZero() && !curPhysical.IsZero() {
		if !belowThreshold(prevPhysical.Energy, EnergyAlertThreshold) && belowThreshold(curPhysical.Energy, EnergyAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("energy %.0f→%.0f 跌破警戒带 %.0f", prevPhysical.Energy, curPhysical.Energy, EnergyAlertThreshold)
		}
		if !belowThreshold(prevPhysical.Health, HealthAlertThreshold) && belowThreshold(curPhysical.Health, HealthAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("health %.0f→%.0f 跌破警戒带 %.0f", prevPhysical.Health, curPhysical.Health, HealthAlertThreshold)
		}
		if !aboveThreshold(prevPhysical.Fatigue, FatigueAlertThreshold) && aboveThreshold(curPhysical.Fatigue, FatigueAlertThreshold) {
			return TriggerPhysicalAlert, fmt.Sprintf("fatigue %.0f→%.0f 突破警戒带 %.0f", prevPhysical.Fatigue, curPhysical.Fatigue, FatigueAlertThreshold)
		}
	}
	return "", ""
}

// belowThreshold returns v < threshold (strictly less than).
func belowThreshold(v, threshold float64) bool { return v < threshold }

// aboveThreshold returns v > threshold (strictly greater than).
func aboveThreshold(v, threshold float64) bool { return v > threshold }

// ShouldTriggerPeriodic checks if it's time for a periodic forced trigger.
//
// Local Ollama model cost is tolerable, but most perceptions with no
// significant change would yield continue, so frequent calls waste compute.
// Every PeriodicTriggerInterval perceptions, force a trigger to give the
// reactive layer a chance to evaluate "should I switch activities",
// "been working too long, should rest", etc.
//
// perceptionCount is the cumulative perception count (from 1). Returns
// (TriggerPeriodic, detail) or ("", ""). The caller is responsible for
// passing the cumulative count; this function is pure for testability.
func ShouldTriggerPeriodic(perceptionCount int) (ReactiveTrigger, string) {
	if perceptionCount <= 0 {
		return "", ""
	}
	if perceptionCount%PeriodicTriggerInterval == 0 {
		return TriggerPeriodic, fmt.Sprintf("周期性评估（第 %d 次感知，每 %d 次触发）", perceptionCount, PeriodicTriggerInterval)
	}
	return "", ""
}

// DiffStrings returns elements in `in` that are not in `notIn` (in - notIn).
func DiffStrings(in, notIn []string) []string {
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

// ExtractObjectIDs extracts object ids from perception's NearbyObjects.
// The reactive layer uses it to compare successive perceptions and detect
// "new object appeared" triggers. Empty IDs are skipped (defensive: mock_ue
// occasionally outputs objects without ids).
func ExtractObjectIDs(p protocol.PerceptionPayload) []string {
	ids := make([]string, 0, len(p.NearbyObjects))
	for _, obj := range p.NearbyObjects {
		if obj.ID != "" {
			ids = append(ids, obj.ID)
		}
	}
	return ids
}

// DedupeKey constructs a unique key for dedupe (agentID + trigger + detail).
// The same key within a dedupe window is not re-triggered.
func DedupeKey(agentID string, trigger ReactiveTrigger, detail string) string {
	return agentID + "|" + string(trigger) + "|" + detail
}

// UpgradeIfPhysicalAlert is a code-level fallback: when physical state is in
// alert (fatigue>80 / energy<40 / health<50) but the LLM still outputs
// continue/observe, force-upgrade to replan.
//
// Motivation: in testing qwen2.5:7b still outputs observe ("物理状态尚可")
// when fatigue crosses the threshold; prompt constraints alone are unreliable.
// The code-level guarantee ensures the agent actually stops and replans on
// physical alerts. The upgraded replan invokes the tactical layer to
// re-decompose the current slot goal (see execute), guiding the LLM to plan
// rest/charging.
//
// Note: only upgrades continue/observe; replan decisions pass through.
// trigger=physical_alert usually already yields replan from the LLM; this
// function mainly covers periodic/zone_change triggers where physical state
// is already in alert but the LLM ignored it.
func UpgradeIfPhysicalAlert(input ReactiveInput, dec ReactiveDecision) ReactiveDecision {
	if dec.Reaction != ReactionContinue && dec.Reaction != ReactionObserve {
		return dec
	}
	// When UE hasn't implemented physical state, PhysicalAvailable=false; skip upgrade to avoid misjudging all-0 as alert.
	if !input.PhysicalAvailable {
		return dec
	}
	alert := ""
	if input.Fatigue > FatigueAlertThreshold {
		alert = fmt.Sprintf("疲劳=%.0f超过%.0f", input.Fatigue, FatigueAlertThreshold)
	} else if input.Energy < EnergyAlertThreshold {
		alert = fmt.Sprintf("体力=%.0f低于%.0f", input.Energy, EnergyAlertThreshold)
	} else if input.Health < HealthAlertThreshold {
		alert = fmt.Sprintf("健康=%.0f低于%.0f", input.Health, HealthAlertThreshold)
	} else {
		return dec
	}
	origReason := dec.Reason
	origReaction := dec.Reaction // capture before mutation to avoid reason mis-display
	dec.Reaction = ReactionReplan
	dec.Reason = "物理状态告警自动升级(" + alert + ")；原决策=" + string(origReaction) + "/" + origReason
	return dec
}

// GameTimeDeltaMinutes computes the "HH:MM" game time delta (cur - prev) in
// minutes. Used for replan dedupe window checks (by game time, not wall-clock).
// Empty or unparseable args return 0 (treated as "no dedupe info, allow trigger").
// Handles day wrap: when cur < prev, adds 24h (1440 minutes) — single-day
// simulations (06:00-22:00) generally won't hit this.
func GameTimeDeltaMinutes(prev, cur string) int {
	p := ParsePlanMinute(prev)
	c := ParsePlanMinute(cur)
	if p < 0 || c < 0 {
		return 0
	}
	delta := c - p
	if delta < 0 {
		delta += 1440 // day wrap
	}
	return delta
}
