package main

// combat-detach（战斗让位）测试：detach=true 的攻击事件 = UE 战斗 AI 接管
// 身体，agent 完全静默让位（不重规划、不下发），直到 combat_exit 或 TTL
// 兜底归还。覆盖：入口（停动作+业务复位+置位、幂等、timer/pending 清理）、
// 让位期间抑制面（worker 守卫语义、路由双守卫、chat_invite 拒绝、
// executor 门禁、/debug/schedule 409）、归还（combat_exit 确定性接收、
// force 变体、TTL 到期与冷启动 inert、非让位回归）、跨断线存续、
// /debug/event 与 /debug/tactical 的 detach 支持。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
)

// detachTestEvent mirrors forceTestEvent with the detach flag set — UE's
// combat-takeover attack event.
func detachTestEvent(id string) protocol.WorldEventPayload {
	ev := forceTestEvent(id)
	ev.Detach = true
	return ev
}

// combatExitTestEvent is the protocol handback signal (non-force).
func combatExitTestEvent(id string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   id,
		Category:  protocol.CategoryPlayerInteraction,
		EventType: protocol.EventTypeCombatExit,
		Force:     false,
		Severity:  6,
		Subject:   "H-01",
		GameTime:  "D12 12:01:30",
		Data:      json.RawMessage(`{"attacker":"player_1","outcome":"escaped"}`),
	}
}

// yieldViaDetach drives the agent into combat yield through the full dispatch
// entry.
func yieldViaDetach(t *testing.T, rt *Runtime, id string) {
	t.Helper()
	dispatchTestEvent(t, rt, "H-01", detachTestEvent(id))
	waitFor(t, time.Second, func() bool { return rt.lookupAgent("H-01").combatYieldActive() })
}

// TestHandleWorldEvent_DetachYieldsControl verifies the yield entry: the
// in-flight stop is the handover assist (synchronous), business state resets
// (queue/slot/in-flight), and — the whole point — NO replan, NO hint, NO
// reaction window follows.
func TestHandleWorldEvent_DetachYieldsControl(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift,
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	dispatchTestEvent(t, rt, "H-01", detachTestEvent("evt_d1"))

	// 交接辅助 stop 同步发出；在途追踪清空（ClearForReplan 已跑）。
	stops := stoppedActions(ft)
	if len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("detach must stop the in-flight action as handover assist, got %v", stops)
	}
	if ac.as.CurrentActionID() != "" {
		t.Fatalf("in-flight tracking must be cleared")
	}
	if !ac.combatYieldActive() {
		t.Fatalf("agent must be combat-yielded after detach")
	}
	// 业务复位：队列与 slot 清空。
	if got := ac.as.QueueLen(); got != 0 {
		t.Fatalf("queue must be dropped at yield entry, got len=%d", got)
	}
	// 不重规划、不 hint、不 arm 反应窗口——与 force 路径的本质区别。
	if !replanIdle(ac) {
		t.Fatalf("detach must NOT trigger a replan")
	}
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("detach must NOT inject a replan hint, got %q", hint)
	}
	if active, _, _ := ac.reactionSnapshot(); active {
		t.Fatalf("detach must NOT arm a reaction window")
	}
	// 等待任何迟到 goroutine 后复查（tacticalHc=nil 时若有 replan 会走
	// abandon 并注入 hint——hint 持续为空即证明从未启动）。
	time.Sleep(50 * time.Millisecond)
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("late replan side effect detected, hint=%q", hint)
	}
	if !replanIdle(ac) {
		t.Fatalf("late replan detected")
	}
}

// TestDetachEntry_IdempotentOnRepeatAttacks verifies repeat attacks while
// already yielded only refresh the TTL anchor and the echoed event — no
// re-stop, no state churn.
func TestDetachEntry_IdempotentOnRepeatAttacks(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)

	yieldViaDetach(t, rt, "evt_d1")
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("no in-flight action, expected zero stops, got %v", stops)
	}
	_, since1, last1 := ac.combatYieldSnapshot()

	// 游戏时钟推进 10 分钟后第二次攻击（新 event_id）——TTL 锚点应随之刷新。
	if _, err := ac.as.SetPerception(mustMarshalBarPerception(t, 11, 38823+600, 11*86400+38823+600)); err != nil {
		t.Fatalf("advance perception: %v", err)
	}
	dispatchTestEvent(t, rt, "H-01", detachTestEvent("evt_d2"))
	waitFor(t, time.Second, func() bool {
		_, since2, last2 := ac.combatYieldSnapshot()
		return since2 != since1 || last2.EventID != last1.EventID
	})
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("repeat detach must not re-stop, got %v", stops)
	}
	_, since2, last2 := ac.combatYieldSnapshot()
	if last2.EventID != "evt_d2" {
		t.Fatalf("lastEvent must refresh to the newest attack, got %q", last2.EventID)
	}
	if since2 <= since1 {
		t.Fatalf("TTL anchor must refresh, since %v -> %v", since1, since2)
	}
}

// TestDetachEntry_CancelsPendingActionTimeout verifies the in-flight action's
// timeout timer is cancelled at yield entry — a late callback would fire a
// stray stop / burn the clearedAction stash.
func TestDetachEntry_CancelsPendingActionTimeout(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.armActionTimeout("act-1", nil, ft, "H-01", rt.lookupAgent)
	ac.coordMu.Lock()
	_, armed := ac.pendingActionTimeouts["act-1"]
	ac.coordMu.Unlock()
	if !armed {
		t.Fatalf("timeout timer must be armed first")
	}

	dispatchTestEvent(t, rt, "H-01", detachTestEvent("evt_d1"))

	ac.coordMu.Lock()
	_, stillArmed := ac.pendingActionTimeouts["act-1"]
	ac.coordMu.Unlock()
	if stillArmed {
		t.Fatalf("yield entry must cancel the in-flight timeout timer")
	}
}

// TestDetachEntry_ClearsStaleSlotSwitchPending verifies a slot switch that
// was already pending when the detach arrived is dissolved at yield entry —
// otherwise processSlotSwitch would wipe the freshly committed post-combat
// queue after the reclaim replan.
func TestDetachEntry_ClearsStaleSlotSwitchPending(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	ac.as.SetSlotSwitchPending()
	if res := ac.as.EnqueueWorldEvent(nonForceTestEvent("evt_keep")); res != agentstate.EnqueueOK {
		t.Fatalf("enqueue: %v", res)
	}

	dispatchTestEvent(t, rt, "H-01", detachTestEvent("evt_d1"))

	if ac.as.SlotSwitchPending() {
		t.Fatalf("stale slotSwitchPending must be cleared at yield entry")
	}
	// 事件队列保留（战斗前上下文活到归还后的 drain）。
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("world-event queue must survive the yield entry, got len=%d", got)
	}
}

// TestDetachEntry_ExitEventWithDetachDoesNotYield verifies a combat_exit
// mislabeled with detach=true does not ENTER a yield — it is the handback
// signal, not a takeover.
func TestDetachEntry_ExitEventWithDetachDoesNotYield(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	ev := combatExitTestEvent("evt_x1")
	ev.Force = true
	ev.Detach = true

	dispatchTestEvent(t, rt, "H-01", ev)

	if ac.combatYieldActive() {
		t.Fatalf("combat_exit+detach must not enter a yield")
	}
}

// TestHandleWorldEvent_NonForceDuringYieldEnqueuesWithoutRouting verifies the
// yield keeps enqueueing non-force events (context for the reclaim drain)
// but skips the router judgment entirely — an interrupt would fight UE's
// combat AI for the body.
func TestHandleWorldEvent_NonForceDuringYieldEnqueuesWithoutRouting(t *testing.T) {
	rt, _, ac, j, _ := newRouterTestRuntime(t, notInterruptJudge())
	yieldViaDetach(t, rt, "evt_d1")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_q1"))

	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("non-force event must still enqueue during yield, got len=%d", got)
	}
	// route() 的让位守卫：给异步 goroutine 一点时间后断言判决从未发生。
	time.Sleep(100 * time.Millisecond)
	if got := len(j.statesOf()); got != 0 {
		t.Fatalf("router must not judge during yield, got %d verdict calls", got)
	}
}

// TestHandleWorldEvent_NonDetachForceDuringYield verifies a plain force
// event arriving mid-yield is parked in the queue (echo bypasses the burned
// dedup id) instead of stop/replan.
func TestHandleWorldEvent_NonDetachForceDuringYield(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	yieldViaDetach(t, rt, "evt_d1")

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_f_force"))

	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("force during yield must not stop anything, got %v", stops)
	}
	if !replanIdle(ac) {
		t.Fatalf("force during yield must not trigger a replan")
	}
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("force during yield must not inject a hint, got %q", hint)
	}
	// 事件经 echo 入队（MarkSeen 已烧掉正常入队通道），归还后 drain 可见。
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("force event must be parked via echo, got len=%d", got)
	}
	if q := ac.as.WorldEventQueueSnapshot(); len(q) != 1 || q[0].EventID != "evt_f_force" {
		t.Fatalf("parked event mismatch: %+v", q)
	}
}

// TestRouterInterrupt_GatedDuringYield verifies the routerInterrupt guard
// covers the in-flight race: a verdict launched before the yield (or from
// the synthesizer launch site) returns after the yield began and must be
// dropped — event stays queued, no stop, no replan.
func TestRouterInterrupt_GatedDuringYield(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
	if res := ac.as.EnqueueWorldEvent(nonForceTestEvent("evt_race")); res != agentstate.EnqueueOK {
		t.Fatalf("enqueue: %v", res)
	}
	yieldViaDetach(t, rt, "evt_d1")

	// 让位生效后裁决才返回——直接调用模拟这个时序。
	rt.routerInterrupt("H-01", nonForceTestEvent("evt_race"), prompt.RouterDecision{
		Interrupt: true, Motive: prompt.RouterMotiveUrgent, Severity: 9, Reason: "race",
	})

	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("late router verdict must not stop during yield, got %v", stops)
	}
	if !replanIdle(ac) {
		t.Fatalf("late router verdict must not replan during yield")
	}
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("late router verdict must not inject a hint, got %q", hint)
	}
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("event must stay queued (not removed by interrupt), got len=%d", got)
	}
}

// TestChatInvite_DuringYieldRejected verifies a chat invite during the yield
// is politely declined (rsp=false so UE frees the inviting peer) instead of
// being forwarded to the dialogue runner.
func TestChatInvite_DuringYieldRejected(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.dialogue = &dialogueRunner{ac: ac, ws: ft, logger: testLogger()}
	yieldViaDetach(t, rt, "evt_d1")

	invite := protocol.WorldEventPayload{
		EventID:   "evt_inv",
		Category:  protocol.CategorySocial,
		EventType: protocol.EventTypeChatInviteIncoming,
		Severity:  5,
		GameTime:  "D12 12:00:30",
		Data:      json.RawMessage(`{"conv_id":"conv_1","from":"H-02","content":"借个工具？"}`),
	}
	dispatchTestEvent(t, rt, "H-01", invite)

	ft.mu.Lock()
	envelopes := append([]string(nil), ft.envelopes...)
	ft.mu.Unlock()
	declined := false
	for _, e := range envelopes {
		if strings.Contains(e, "chat_invite_rsp") && strings.Contains(e, `"accept":false`) {
			declined = true
		}
	}
	if !declined {
		t.Fatalf("invite during yield must be declined via chat_invite_rsp(false), got %v", envelopes)
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("declined invite must not enqueue, got len=%d", got)
	}
}

// TestGuardedExecutor_SendActionRejectedDuringYield verifies the executor
// chokepoint rejects action commands while yielded (defense in depth).
func TestGuardedExecutor_SendActionRejectedDuringYield(t *testing.T) {
	rt, ft, _ := newWorldEventTestRuntime(t)
	yieldViaDetach(t, rt, "evt_d1")

	g := &guardedExecutor{ws: ft, lookup: rt.lookupAgent}
	_, err := g.SendAction(context.Background(), "H-01", 0, protocol.CmdSpeak, map[string]any{"content": "hi"})
	if err == nil || !strings.Contains(err.Error(), "combat-detach") {
		t.Fatalf("SendAction during yield must be rejected with combat-detach error, got %v", err)
	}
}

// TestDebugSchedule_409DuringYield verifies the schedule injection endpoint
// refuses while yielded (the body belongs to UE's combat AI).
func TestDebugSchedule_409DuringYield(t *testing.T) {
	rt, ft, _ := newWorldEventTestRuntime(t)
	yieldViaDetach(t, rt, "evt_d1")

	req, rec := newDebugScheduleRecorder(t, `{"agent_id":"H-01","schedule":"车间装配作业"}`)
	handleDebugSchedule(context.Background(), testLogger(), ft, nil, rt.lookupAgent, nil, rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp debugScheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(resp.Error, "combat-detach") {
		t.Fatalf("error should mention combat-detach, got %q", resp.Error)
	}
}

// TestCombatExit_ReturnsControl verifies the handback: combat_exit while
// yielded deterministically returns control (no router judgment — the
// verdictRecorder stays empty) and the post-combat replan sees both the
// 【战斗结束】 hint and the chronological attack→exit pair.
func TestCombatExit_ReturnsControl(t *testing.T) {
	rt, ft, ac, j, verdicts := newRouterTestRuntime(t, notInterruptJudge())
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac.tacticalHc = fake
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.as.RecordActionStarted("act-1", "WorkShift", nil, agentstate.SourceTactical, "")

	// 让位（stop=act-1 交接辅助）→ combat_exit 归还。
	yieldViaDetach(t, rt, "evt_atk")
	dispatchTestEvent(t, rt, "H-01", combatExitTestEvent("evt_exit"))

	if ac.combatYieldActive() {
		// 归还路径已同步清标志（reclaimFromCombatYield 在接收路径跑）。
		t.Fatalf("combat_exit must end the yield")
	}
	// 归还 replan 成功（fake 返回 tool_calls）：攻击回声被 drain 进 prompt，
	// exit 事件成功后回声入队（修复 B 同款）。等待条件锚定"exit 回声出现
	// 且 replan 空闲"——reclaim 同步回声的攻击事件会让 queue==1 在 replan
	// 启动前就成立，不能作为完成信号。
	waitFor(t, 2*time.Second, func() bool {
		if !replanIdle(ac) {
			return false
		}
		for _, e := range ac.as.WorldEventQueueSnapshot() {
			if e.EventID == "evt_exit" {
				return true
			}
		}
		return false
	})
	user := lastUserPromptOf(t, fake.capturedMsgs)
	for _, want := range []string{
		"【战斗结束】",                      // hint 渲染（战后恢复指引）
		"与玩家 player_1 的战斗结束",              // exit 事件（hint 携带）
		"被玩家 player_1 攻击",                 // 攻击回声（【发生的事件】）
		"恢复自主控制",                        // 战后恢复指引文案
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("post-combat replan prompt missing %q:\n%s", want, user)
		}
	}
	if q := ac.as.WorldEventQueueSnapshot(); len(q) != 1 || q[0].EventID != "evt_exit" {
		t.Fatalf("post-combat queue must hold exactly the exit echo, got %+v", q)
	}
	// 归还不走路由器：判决记录为空（route 守卫 + handleWorldEvent 直连分支）。
	if got := len(j.statesOf()); got != 0 {
		t.Fatalf("combat_exit reclaim must bypass the router, got %d verdict calls", got)
	}
	if verdicts.len() != 0 {
		t.Fatalf("no interrupt verdicts expected, got %d", verdicts.len())
	}
	// 交接辅助 stop 恰一次（进入让位时）。
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("expected exactly the handover stop, got %v", stops)
	}
}

// TestCombatExit_ForceVariantAlsoReturns verifies a force-labeled combat_exit
// also reclaims (the reclaim branch sits before the force split).
func TestCombatExit_ForceVariantAlsoReturns(t *testing.T) {
	rt, _, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
	ac.tacticalHc = &fakeLoopLLM{resp: speakToolCallResp()}
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	yieldViaDetach(t, rt, "evt_atk")
	exit := combatExitTestEvent("evt_exit_f")
	exit.Force = true
	dispatchTestEvent(t, rt, "H-01", exit)

	if ac.combatYieldActive() {
		t.Fatalf("force combat_exit must also end the yield")
	}
	// 同 TestCombatExit_ReturnsControl：锚定 exit 回声（replan 成功的产物）。
	waitFor(t, 2*time.Second, func() bool {
		if !replanIdle(ac) {
			return false
		}
		for _, e := range ac.as.WorldEventQueueSnapshot() {
			if e.EventID == "evt_exit_f" {
				return true
			}
		}
		return false
	})
}

// TestCombatYieldTTL_ExpiryReturnsControl verifies the TTL safety net: no
// combat_exit within combatYieldTTLGameSec of the last detach → the agent
// reclaims control with the expiry hint.
func TestCombatYieldTTL_ExpiryReturnsControl(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
	ac.tacticalHc = &fakeLoopLLM{resp: speakToolCallResp()}
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	yieldViaDetach(t, rt, "evt_atk")

	// 把 TTL 起点拨回到期之前（测试直接操作 coordMu 字段）。
	ac.coordMu.Lock()
	ac.combatYieldSinceGameSec = ac.as.LatestGameTimeSec() - combatYieldTTLGameSec - 1
	ac.coordMu.Unlock()

	kb := *rt.kbPtr
	if !ac.checkCombatYieldExpiry(context.Background(), "H-01", ft, kb, nil, testLogger()) {
		t.Fatalf("expired yield must report a reclaim")
	}
	if ac.combatYieldActive() {
		t.Fatalf("TTL reclaim must end the yield")
	}
	// TTL 变体 hint：兜底文案 + 攻击事件回声（lastEvent 由 replan echo 或
	// 手动回声入队——失败路径也不丢）。tacticalHc=fake → replan 成功。
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac) })
	hint := ac.as.ReplanHint()
	if !strings.Contains(hint, "【战斗结束】") || !strings.Contains(hint, "兜底") {
		// CommitReplan 成功会清 hint——若已清空则从 prompt 断言。
		if hint != "" {
			t.Fatalf("TTL hint should mention 兜底, got %q", hint)
		}
	}
}

// TestCombatYieldTTL_NoPerceptionInert verifies the cold-start convention: a
// yield begun before the first perception stores since=0 and stays inert —
// the first perception LATCHES the anchor instead of instantly expiring it.
func TestCombatYieldTTL_NoPerceptionInert(t *testing.T) {
	// newRouterTestRuntime 内部 seedPerception，这里要无感知冷启动——用
	// newWorldEventTestRuntime（LatestGameTimeSec()=0）。
	rt, ft, ac := newWorldEventTestRuntime(t)
	// 无 seedPerception：LatestGameTimeSec()=0。
	dispatchTestEvent(t, rt, "H-01", detachTestEvent("evt_cold"))
	if !ac.combatYieldActive() {
		t.Fatalf("yield should begin even without perception")
	}
	_, since, _ := ac.combatYieldSnapshot()
	if since != 0 {
		t.Fatalf("cold-start yield must store since=0, got %v", since)
	}

	kb := *rt.kbPtr
	if ac.checkCombatYieldExpiry(context.Background(), "H-01", ft, kb, nil, testLogger()) {
		t.Fatalf("cold-start yield must stay inert (no instant expiry)")
	}
	if !ac.combatYieldActive() {
		t.Fatalf("cold-start yield must survive the inert check")
	}

	// 首条感知到达：起点锁存为当前时刻（不按 since=0 差值误判）。
	seedPerception(t, ac)
	if ac.checkCombatYieldExpiry(context.Background(), "H-01", ft, kb, nil, testLogger()) {
		t.Fatalf("freshly latched anchor must not be expired")
	}
	_, since2, _ := ac.combatYieldSnapshot()
	if since2 <= 0 {
		t.Fatalf("anchor must latch to the first perception time, got %v", since2)
	}
	if !ac.combatYieldActive() {
		t.Fatalf("yield must persist until TTL actually elapses")
	}
}

// TestCombatExit_WhileNotYieldingFallsThrough verifies the regression: a
// combat_exit without a prior yield keeps its existing enqueue+route path.
func TestCombatExit_WhileNotYieldingFallsThrough(t *testing.T) {
	rt, _, ac, j, _ := newRouterTestRuntime(t, notInterruptJudge())
	seedPerception(t, ac)

	dispatchTestEvent(t, rt, "H-01", combatExitTestEvent("evt_exit_plain"))

	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("non-yielded combat_exit must enqueue, got len=%d", got)
	}
	waitFor(t, time.Second, func() bool { return len(j.statesOf()) == 1 })
}

// TestCombatYield_SurvivesStopAndReconnect verifies the yield state survives
// agent stop()/reconnect — UE never re-sends detach after a reconnect, so
// clearing here would let MCP steal the body back mid-combat on every WS blip.
func TestCombatYield_SurvivesStopAndReconnect(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	yieldViaDetach(t, rt, "evt_d1")

	ac.stop()

	if !ac.combatYieldActive() {
		t.Fatalf("combat yield must survive agent stop()/reconnect (bounded by TTL)")
	}
}

// ─── 协议容错（event_type 短名 + category 缺失）─────────────────

// TestIsCombatStartEvent_AliasesAndMissingCategory pins the tolerant combat
// predicates: UE's short names and missing category both match; unrelated
// types and conflicting categories do not.
func TestIsCombatStartEvent_AliasesAndMissingCategory(t *testing.T) {
	cases := []struct {
		name string
		ev   protocol.WorldEventPayload
		want bool
	}{
		{"canonical", protocol.WorldEventPayload{Category: protocol.CategoryPlayerInteraction, EventType: protocol.EventTypePlayerAttacked}, true},
		{"short alias no category", protocol.WorldEventPayload{EventType: "attacked"}, true},
		{"short alias with category", protocol.WorldEventPayload{Category: protocol.CategoryPlayerInteraction, EventType: "attacked"}, true},
		{"targeted alias", protocol.WorldEventPayload{EventType: "targeted"}, true},
		{"canonical targeted", protocol.WorldEventPayload{Category: protocol.CategoryPlayerInteraction, EventType: protocol.EventTypePlayerTargeted}, true},
		{"unrelated type", protocol.WorldEventPayload{Category: protocol.CategoryWorld, EventType: protocol.EventTypeMalfunction}, false},
		{"conflicting category", protocol.WorldEventPayload{Category: protocol.CategoryWorld, EventType: "attacked"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := prompt.IsCombatStartEvent(tc.ev); got != tc.want {
				t.Fatalf("IsCombatStartEvent(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}
}

// TestIsCombatExitEvent_Alias pins the exit predicate tolerance.
func TestIsCombatExitEvent_Alias(t *testing.T) {
	if !prompt.IsCombatExitEvent(protocol.WorldEventPayload{EventType: protocol.EventTypeCombatExit}) {
		t.Fatalf("combat_exit without category must match")
	}
	if !prompt.IsCombatExitEvent(protocol.WorldEventPayload{Category: protocol.CategoryPlayerInteraction, EventType: protocol.EventTypeCombatExit}) {
		t.Fatalf("canonical combat_exit must match")
	}
	if prompt.IsCombatExitEvent(protocol.WorldEventPayload{EventType: protocol.EventTypePlayerInteract}) {
		t.Fatalf("player_interact is not a combat exit")
	}
}

// TestDetach_SituationRegistersWithShortNames verifies the end-to-end fix
// for the 2026-09-22 field bug: UE's bare "attacked" (no category) now
// registers the persistent combat situation (it never did before).
func TestDetach_SituationRegistersWithShortNames(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)

	ev := protocol.WorldEventPayload{
		EventID:   "evt_short",
		EventType: "attacked", // UE 实测形态：短名、无 category
		Force:     true,
		Detach:    true,
		Severity:  10,
		Subject:   "H-01",
		GameTime:  "D12 12:00:00",
		Data:      json.RawMessage(`{"attacker":"player_1","damage":10}`),
	}
	dispatchTestEvent(t, rt, "H-01", ev)

	snap := ac.as.Snapshot()
	found := false
	for _, s := range snap.ActiveSituations {
		if s.Kind == situationKindCombat {
			found = true
		}
	}
	if !found {
		t.Fatalf("bare attacked must register the combat situation: %+v", snap.ActiveSituations)
	}
}

// ─── /debug/event 与 /debug/tactical 的 detach 支持 ───────────────

// TestHandleDebugEvent_DetachEchoAndNote verifies the endpoint round-trips
// the detach flag, reports the yield note, and tolerates the UE-malformed
// combat event (short event_type, missing category) for e2e tolerance tests.
func TestHandleDebugEvent_DetachEchoAndNote(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	code, resp := postDebugEvent(t, rt, `{
		"agent_id": "H-01",
		"event": {"event_type": "attacked", "force": true, "detach": true, "severity": 10,
			"data": {"attacker": "player_1", "damage": 20, "damage_type": "physical"}}
	}`)
	if code != http.StatusOK || !resp.OK || !resp.Detach {
		t.Fatalf("code=%d resp=%+v, want 200/ok/detach", code, resp)
	}
	// 空 category + 短名 → 服务端按别名表补齐 player_interaction。
	if resp.Category != protocol.CategoryPlayerInteraction {
		t.Fatalf("category should be inferred from the combat alias table, got %q", resp.Category)
	}
	if !strings.Contains(resp.Note, "战斗接管") || !strings.Contains(resp.Note, "让位") {
		t.Fatalf("note should explain the yield, got %q", resp.Note)
	}
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("detach injection must stop the in-flight action, got %v", stops)
	}
	waitFor(t, time.Second, func() bool { return ac.combatYieldActive() })
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("detach injection must not inject a hint, got %q", hint)
	}
}

// TestDebugTactical_DetachedField verifies /debug/tactical exposes the yield
// state for the console badge.
func TestDebugTactical_DetachedField(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	yieldViaDetach(t, rt, "evt_d1")

	req := httptest.NewRequest(http.MethodGet, "/debug/tactical", nil)
	rec := httptest.NewRecorder()
	handleDebugTactical(rec, req, rt.lookupAgent, func() []string { return []string{"H-01"} }, testLogger())

	var entries []debugTacticalEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if len(entries) != 1 || !entries[0].Detached {
		t.Fatalf("entry must report detached=true, got %+v", entries)
	}
}
