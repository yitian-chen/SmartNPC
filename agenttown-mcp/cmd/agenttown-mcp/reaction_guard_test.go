package main

// P2-6 反应护栏测试：截止时间（护栏①）+ severity 严格递增（护栏②）。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
)

// reactionTestEvent builds a non-force event with a controllable id.
func reactionTestEvent(id string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   id,
		Category:  protocol.CategoryWorld,
		EventType: protocol.EventTypeMalfunction,
		Severity:  7,
		Subject:   "K-03",
		GameTime:  "D12 10:47:03",
		Data:      json.RawMessage(`{"target":"K-03","description":"K-03 关节锁死"}`),
	}
}

// dispatchRouterInterrupt drives the real routerInterrupt with a scripted
// verdict (bypassing the LLM call) — the guardrail wiring lives in
// routerInterrupt itself.
func dispatchRouterInterrupt(t *testing.T, rt *Runtime, agentID string, ev protocol.WorldEventPayload, severity int) {
	t.Helper()
	rt.routerInterrupt(agentID, ev, prompt.RouterDecision{Interrupt: true, Severity: severity, Reason: "测试裁决"})
}

// TestReactionGuard_HigherSeverityRequired verifies guardrail ②: while a
// reaction (severity 8) is active, a router interrupt with severity ≤ 8 is
// gated (event stays queued, nothing stopped), and severity 9 proceeds and
// re-arms the window with the higher bar. The window is armed via
// beginReaction directly so the gated phase has no in-flight replan
// goroutine (deterministic; the interrupt-arms-window wiring is asserted
// on the proceeding branch below).
func TestReactionGuard_HigherSeverityRequired(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	// 直接武装 severity 8 的反应窗口（等价于一次 severity 8 打断后的护栏状态）。
	ac.beginReaction(8, ac.as.LatestGameTimeSec(), "")

	// severity 8（不严格更高）被护栏拦截——事件留在队列，不 stop，不 replan。
	ac.as.EnqueueWorldEvent(reactionTestEvent("evt_g2"))
	dispatchRouterInterrupt(t, rt, "H-01", reactionTestEvent("evt_g2"), 8)
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("gated event must stay queued, got len=%d", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("gated interrupt must not stop anything, got %v", stops)
	}
	if !replanIdle(ac) {
		t.Fatalf("gated interrupt must not trigger a replan")
	}

	// severity 9（严格更高）放行：打断 + 重置窗口 bar 为 9。
	ac.as.RecordActionStarted("act-2", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	dispatchRouterInterrupt(t, rt, "H-01", reactionTestEvent("evt_g3"), 9)
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-2" {
		t.Fatalf("strictly-higher interrupt must proceed, got %v", stops)
	}
	if _, sev, _ := ac.reactionSnapshot(); sev != 9 {
		t.Fatalf("window must re-arm with the higher severity, got %d", sev)
	}
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac) })
}

// TestReactionGuard_ForceBypassesLadder verifies §4.2: force events ignore
// the severity ladder entirely — even a severity-2 force event cuts an
// active severity-9 reaction.
func TestReactionGuard_ForceBypassesLadder(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.beginReaction(9, ac.as.LatestGameTimeSec(), "")

	// severity 2 的 force 事件（如调试命令）照样打断。
	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_g4"))
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force must bypass the severity ladder, got %v", stops)
	}
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac) })
}

// TestReactionGuard_DeadlineCutsBackToSchedule verifies guardrail ①: at
// deadline expiry the still-armed reaction is cut — in-flight stopped,
// queue dropped, hint tells the next refill to return to the schedule, and
// the guard is lifted.
func TestReactionGuard_DeadlineCutsBackToSchedule(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac) // D12 10:47:03 → gameSec 989223
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	// 直接武装一个"马上到期"的反应窗口（绕过 beginReaction 的 30 分钟
	// 常量，验证 checkReactionDeadline 的行为本身）。
	ac.beginReaction(8, ac.as.LatestGameTimeSec()-reactionDeadlineGameSec-1, "") // deadline 已过

	ac.checkReactionDeadline("H-01", ft, testLogger())

	if active, _, _ := ac.reactionSnapshot(); active {
		t.Fatalf("deadline expiry must lift the guard")
	}
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("expired reaction must be stopped, got %v", stops)
	}
	if got := ac.as.QueueLen(); got != 0 {
		t.Fatalf("expired reaction queue must be dropped, got len=%d", got)
	}
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【反应截止】") {
		t.Fatalf("hint must tell the next refill to return to the schedule, got %q", hint)
	}
	// worker 被唤醒（自然 refill 回到日程）。
	select {
	case <-ac.wake:
	default:
		t.Fatalf("deadline cut must signal the worker")
	}
}

// TestReactionGuard_DeadlineNotYetReached verifies the check is inert
// before the deadline and for unarmed agents.
func TestReactionGuard_DeadlineNotYetReached(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	// 未到期：什么都不动。
	ac.beginReaction(8, ac.as.LatestGameTimeSec(), "")
	ac.checkReactionDeadline("H-01", ft, testLogger())
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("pre-deadline check must be inert, got %v", stops)
	}
	if active, _, _ := ac.reactionSnapshot(); !active {
		t.Fatalf("guard must stay armed before the deadline")
	}
	if ac.as.CurrentActionID() != "act-1" {
		t.Fatalf("in-flight action must be untouched before the deadline")
	}

	// 未武装：no-op。
	ac.clearReaction()
	ac.checkReactionDeadline("H-01", ft, testLogger())
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("unarmed check must be a no-op, got %v", stops)
	}
}

// TestReactionGuard_ScheduleRefillClears verifies the natural end: a
// worker schedule refill (tacticalRefill) lifts the reaction window — the
// NPC has returned to schedule-driven planning.
func TestReactionGuard_ScheduleRefillClears(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.beginReaction(8, ac.as.LatestGameTimeSec(), "")

	// tacticalHc 失败 → refill 返回 false，但 clearReaction 在 LLM 调用前
	// 已执行（ShouldSkip 守卫之后）。
	ac.tacticalHc = newFailedVenusClient()
	_ = ac.tacticalRefill(context.Background(), "H-01", ft, nil, nil, testLogger())

	if active, _, _ := ac.reactionSnapshot(); active {
		t.Fatalf("schedule refill must clear the reaction window")
	}
}

// TestReactionGuard_StopClears verifies agent shutdown lifts the guard.
func TestReactionGuard_StopClears(t *testing.T) {
	_, _, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.beginReaction(8, 1000, "")
	ac.stop()
	if active, _, _ := ac.reactionSnapshot(); active {
		t.Fatalf("agent stop must clear the reaction window")
	}
}

// TestReactionGuard_BeginClampsSeverity verifies the severity bar clamps
// to 0..10 regardless of source (force events trust UE's field).
func TestReactionGuard_BeginClampsSeverity(t *testing.T) {
	_, _, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.beginReaction(99, 1000, "")
	if _, sev, _ := ac.reactionSnapshot(); sev != 10 {
		t.Fatalf("severity 99 must clamp to 10, got %d", sev)
	}
	ac.beginReaction(-5, 1000, "")
	if _, sev, _ := ac.reactionSnapshot(); sev != 0 {
		t.Fatalf("severity -5 must clamp to 0, got %d", sev)
	}
}

// TestReactionGuard_NoPerceptionDeadlineInert verifies beginReaction with
// no perception (nowGameSec<=0) leaves the deadline check inert until the
// first perception arrives.
func TestReactionGuard_NoPerceptionDeadlineInert(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.beginReaction(8, 0, "") // 无感知
	ac.checkReactionDeadline("H-01", ft, testLogger())
	if active, _, _ := ac.reactionSnapshot(); !active {
		t.Fatalf("no-perception window stays armed (inert), got cleared")
	}
}

// ensure sync import is used even if future edits drop the field usage.
var _ sync.Mutex
