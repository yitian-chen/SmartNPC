package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// newWorldEventTestRuntime builds a minimal Runtime wired to a fake
// transport with one registered agent (H-01). tacticalHc stays nil so the
// force replan deterministically takes the failure path (abandon).
func newWorldEventTestRuntime(t *testing.T) (*Runtime, *fakeTransport, *agentContext) {
	t.Helper()
	ft := &fakeTransport{connected: true}
	ac, _ := newAgentContext(context.Background())
	agents := map[string]*agentContext{"H-01": ac}
	kb := &worldkb.KB{}
	return &Runtime{
		logger:          testLogger(),
		ws:              ft,
		lookupAgent:     func(id string) *agentContext { return agents[id] },
		agents:          agents,
		agentsMu:        &sync.Mutex{},
		autoPlanEnabled: true,
		ctx:             context.Background(),
		kbPtr:           &kb,
	}, ft, ac
}

// dispatchTestEvent marshals ev and feeds it through the full
// Runtime.HandleMessage entry (exercises the case wiring, not just the
// handler).
func dispatchTestEvent(t *testing.T, rt *Runtime, agentID string, ev protocol.WorldEventPayload) {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	rt.HandleMessage(context.Background(), protocol.TypeWorldEvent, agentID, payload)
}

func forceTestEvent(id string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   id,
		Category:  protocol.CategoryPlayerInteraction,
		EventType: protocol.EventTypePlayerAttacked,
		Force:     true,
		Severity:  10,
		Subject:   "H-01",
		GameTime:  "D12 12:00:00",
		Location:  "central_plaza",
		Data:      json.RawMessage(`{"attacker":"player_1","damage":20,"damage_type":"physical"}`),
	}
}

func nonForceTestEvent(id string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   id,
		Category:  protocol.CategoryWorld,
		EventType: protocol.EventTypeMalfunction,
		Force:     false,
		Severity:  7,
		Subject:   "K-03",
		GameTime:  "D12 10:47:03",
		Data:      json.RawMessage(`{"target":"K-03","description":"K-03 关节锁死"}`),
	}
}

// replanIdle reports whether no replan goroutine holds the replan slot.
func replanIdle(ac *agentContext) bool {
	ac.coordMu.Lock()
	defer ac.coordMu.Unlock()
	return !ac.replanInProgress
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// stoppedActions returns a copy of the recorded stop_action ids.
func stoppedActions(ft *fakeTransport) []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]string(nil), ft.stopActionIDs...)
}

// TestHandleWorldEvent_NonForceEnqueuesOnly verifies the non-force branch:
// enqueue for the next safe point, zero stop, zero replan, zero hint.
func TestHandleWorldEvent_NonForceEnqueuesOnly(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_1"))

	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("queue len = %d, want 1", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("non-force event must not stop anything, got %v", stops)
	}
	if !replanIdle(ac) {
		t.Fatalf("non-force event must not trigger a replan")
	}
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("non-force event must not set the replan hint, got %q", hint)
	}
}

// TestHandleWorldEvent_ForceInterruptsInFlightImmediately verifies the
// §4.2 hard guarantee: the stop fires synchronously in the receive path
// (asserted before any goroutine could have run), in-flight tracking is
// handed to the delayed completion, and the async replan failure path drops
// the stale queue and injects the force hint.
func TestHandleWorldEvent_ForceInterruptsInFlightImmediately(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift,
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_f1"))

	// 硬保证部分是同步的：stop 已发 + 在途追踪已清，此刻 replan goroutine
	// 尚未跑完也必须成立。
	stops := stoppedActions(ft)
	if len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force must stop the in-flight action synchronously, got %v", stops)
	}
	if ac.as.CurrentActionID() != "" {
		t.Fatalf("in-flight tracking must be cleared after the stop")
	}

	// replan（tacticalHc=nil → 失败路径）最终清空旧队列、注入 hint。
	waitFor(t, 2*time.Second, func() bool {
		return ac.as.QueueLen() == 0 && replanIdle(ac)
	})
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【强制打断】") || !strings.Contains(hint, "被玩家 player_1 攻击") {
		t.Fatalf("force hint not injected: %q", hint)
	}
}

// TestHandleWorldEvent_ForceDuplicateDropped verifies the event_id dedup on
// the force path: a seq-replay of the same event must not re-interrupt.
func TestHandleWorldEvent_ForceDuplicateDropped(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	// 旧队列给 goroutine 一个可观测的完成信号（abandon 清队列）——只等
	// replanIdle 会在 goroutine 启动前就返回，遗留 goroutine 跨测试存活。
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	ev := forceTestEvent("evt_f2")
	dispatchTestEvent(t, rt, "H-01", ev)
	dispatchTestEvent(t, rt, "H-01", ev) // replay

	// 第二次被 seen-set 同步拦截；第一次的失败路径不会追发 stop
	// （ClearInFlightKeepQueue 后 CurrentActionID 为空）。
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("replayed force event must be dropped by event_id dedup, got %d stops: %v", len(stops), stops)
	}
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
}

// TestHandleWorldEvent_ForceWithoutInFlightNoStop verifies the no-in-flight
// case: nothing to stop, the replan still runs (hint set synchronously).
func TestHandleWorldEvent_ForceWithoutInFlightNoStop(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_f3"))

	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("no stop expected without in-flight action, got %v", stops)
	}
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【强制打断】") {
		t.Fatalf("hint expected even without in-flight action: %q", hint)
	}
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
}

// socialSpeechTestEvent builds a force speech_heard event mirroring UE's
// NPC-to-NPC speech dispatch (category=social, force=true).
func socialSpeechTestEvent(id string, category string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   id,
		Category:  category,
		EventType: protocol.EventTypeSpeechHeard,
		Force:     true,
		Severity:  5,
		Subject:   "H-02",
		GameTime:  "D12 12:00:00",
		Location:  "logistics_hub",
		Data:      json.RawMessage(`{"speaker":"H-04","content":"老王，歇会儿不？长凳上坐坐。"}`),
	}
}

// TestForceEventHint_SocialVsEmergency verifies the force-path hint flavor:
// social speech events (UE's speech_heard always carries force:true) render
// as 【社交回应】 (invitation-decision guidance), while combat/threat events
// keep the 【强制打断】→【紧急事件】 treatment. 2026-10-15 sim: a chat line
// rendered as an emergency pushed the invitee back to work and the inviter
// answered an acceptance with a monologue.
func TestForceEventHint_SocialVsEmergency(t *testing.T) {
	if hint := forceEventHint(socialSpeechTestEvent("evt_s1", protocol.CategorySocial)); !strings.Contains(hint, "【社交回应】") {
		t.Fatalf("social speech event must use the social hint, got %q", hint)
	}
	// speech_heard 的 event_type 容错：category 缺失时同样按社交处理
	//（与 combat 短名别名同款容忍）。
	if hint := forceEventHint(socialSpeechTestEvent("evt_s2", "")); !strings.Contains(hint, "【社交回应】") {
		t.Fatalf("category-less speech_heard must use the social hint, got %q", hint)
	}
	if hint := forceEventHint(forceTestEvent("evt_f9")); !strings.Contains(hint, "【强制打断】") {
		t.Fatalf("combat force event must keep the emergency hint, got %q", hint)
	}
}

// TestHandleWorldEvent_ForceSocialUsesSocialHint drives the full force path
// with a speech_heard event: the hint carries 【社交回应】 (not 【强制打断】)
// and the interrupted-action bookkeeping labels it a social interruption.
func TestHandleWorldEvent_ForceSocialUsesSocialHint(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift,
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")

	dispatchTestEvent(t, rt, "H-01", socialSpeechTestEvent("evt_s3", protocol.CategorySocial))

	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force speech must still stop the in-flight action, got %v", stops)
	}
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【社交回应】") || strings.Contains(hint, "【强制打断】") {
		t.Fatalf("force speech hint must be social-flavored, got %q", hint)
	}
	if _, why := interruptedRecord(ac); !strings.Contains(why, "被社交对话打断") {
		t.Fatalf("interrupted bookkeeping must label it social, got %q", why)
	}
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
}

// interruptedRecord returns the latest unfinished-task snapshot's end
// bookkeeping (P4-10): unfinished task description + why it ended.
func interruptedRecord(ac *agentContext) (string, string) {
	snap := ac.as.Snapshot()
	return snap.UnfinishedTask, snap.LastEndWhy
}

// TestHandleWorldEvent_UnregisteredAgentDropped verifies an event for an
// unknown agent is dropped with a warning instead of panicking.
func TestHandleWorldEvent_UnregisteredAgentDropped(t *testing.T) {
	rt, ft, _ := newWorldEventTestRuntime(t)
	rt.lookupAgent = func(string) *agentContext { return nil }

	dispatchTestEvent(t, rt, "H-99", nonForceTestEvent("evt_9"))

	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("unregistered agent must not be stopped, got %v", stops)
	}
}

// TestHandleWorldEvent_ManualModeDropped verifies manual mode (--auto-plan
// =false) drops world_events entirely, consistent with the reactive-layer
// trigger gating.
func TestHandleWorldEvent_ManualModeDropped(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	rt.autoPlanEnabled = false
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_f4"))
	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_5"))

	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("manual mode must not enqueue, got %d", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("manual mode must not stop in-flight actions, got %v", stops)
	}
	if hint := ac.as.ReplanHint(); hint != "" {
		t.Fatalf("manual mode must not set the replan hint, got %q", hint)
	}
}

// TestHandleWorldEvent_ParseFailureDropped verifies a malformed payload is
// dropped at the dispatch boundary.
func TestHandleWorldEvent_ParseFailureDropped(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)

	rt.HandleMessage(context.Background(), protocol.TypeWorldEvent, "H-01", json.RawMessage(`{not json`))

	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("malformed event must be dropped, got %d queued", got)
	}
}

// TestForceInterruptReplan_WaitsForInFlightReplanTimesOut verifies the
// contended-slot path: with another replan holding the slot beyond the
// wait limit, the force goroutine gives up — but the interruption has
// already fired synchronously, the hint stays set, and the worker is
// signaled so its natural refill still carries the event context.
func TestForceInterruptReplan_WaitsForInFlightReplanTimesOut(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	origLimit := forceReplanWaitLimit
	forceReplanWaitLimit = 150 * time.Millisecond
	t.Cleanup(func() { forceReplanWaitLimit = origLimit })

	ac.coordMu.Lock()
	ac.replanInProgress = true // simulate a /debug/schedule replan holding the slot
	ac.coordMu.Unlock()
	t.Cleanup(func() {
		ac.coordMu.Lock()
		ac.replanInProgress = false
		ac.coordMu.Unlock()
	})

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_f6"))

	// 硬保证不受 slot 竞争影响：stop 同步已发。
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force stop must fire regardless of replan contention, got %v", stops)
	}
	// 等待超时路径必须 signal worker（自然 refill 兜底）。
	select {
	case <-ac.wake:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout path must signal the worker")
	}
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【强制打断】") {
		t.Fatalf("hint must survive the timeout path: %q", hint)
	}
}

// ─── /debug/event 注入端点测试（P4-13）────────────────────────

// postDebugEvent POSTs a JSON body through handleDebugEvent and decodes the
// response. inject is the runtime's real dispatch entry, so a passing test
// exercises the same path a UE message would take.
func postDebugEvent(t *testing.T, rt *Runtime, body string) (int, debugEventResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleDebugEvent(testLogger(), rt.lookupAgent, rt.handleWorldEvent, rec, req)
	var resp debugEventResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return rec.Code, resp
}

// seedPerception installs a perception at D12 10:47:03 so game-time
// autofill has an authoritative source.
func seedPerception(t *testing.T, ac *agentContext) {
	t.Helper()
	if _, err := ac.as.SetPerception(mustMarshalBarPerception(t, 11, 38823, 11*86400+38823)); err != nil {
		t.Fatalf("seed perception: %v", err)
	}
}

// TestHandleDebugEvent_NonForceAutofillsAndEnqueues verifies the endpoint
// feeds a complete event through the real dispatch: server-side autofill of
// event_id (evt_debug_*，与 UE id 空间隔离) / game_time (当前游戏时间) /
// occurred_at / data，then enqueues for the safe-point drain.
func TestHandleDebugEvent_NonForceAutofillsAndEnqueues(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)

	code, resp := postDebugEvent(t, rt, `{
		"agent_id": "H-01",
		"event": {"category": "world", "event_type": "malfunction", "severity": 7}
	}`)
	if code != http.StatusOK || !resp.OK {
		t.Fatalf("code=%d resp=%+v, want 200/ok", code, resp)
	}
	if !strings.HasPrefix(resp.EventID, "evt_debug_") {
		t.Fatalf("event_id should be auto-generated with evt_debug_ prefix, got %q", resp.EventID)
	}
	if resp.GameTime != "D12 10:47:03" {
		t.Fatalf("game_time should autofill from the agent's perception, got %q", resp.GameTime)
	}
	if resp.QueueLen != 1 {
		t.Fatalf("queue_len = %d, want 1", resp.QueueLen)
	}
	// 入队事件的字段经 autofill 后完整（drain 检查 occurred_at/data）。
	events := ac.as.DrainWorldEvents()
	if len(events) != 1 {
		t.Fatalf("drained %d events, want 1", len(events))
	}
	if events[0].OccurredAt == 0 {
		t.Fatalf("occurred_at should autofill to now")
	}
	if string(events[0].Data) != "{}" {
		t.Fatalf("data should default to empty object, got %s", events[0].Data)
	}
	if events[0].EventID != resp.EventID {
		t.Fatalf("enqueued id %q != response id %q", events[0].EventID, resp.EventID)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("non-force injection must not stop anything, got %v", stops)
	}
}

// TestHandleDebugEvent_ForceStopsInFlight verifies a force injection runs
// the full hard-guarantee path: synchronous stop + async replan failure
// fallback (queue dropped, hint injected).
func TestHandleDebugEvent_ForceStopsInFlight(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	code, resp := postDebugEvent(t, rt, `{
		"agent_id": "H-01",
		"event": {"category": "player_interaction", "event_type": "player_attacked",
			"force": true, "severity": 10,
			"data": {"attacker": "player_1", "damage": 20, "damage_type": "physical"}}
	}`)
	if code != http.StatusOK || !resp.OK || !resp.Force {
		t.Fatalf("code=%d resp=%+v, want 200/ok/force", code, resp)
	}
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force injection must stop the in-flight action synchronously, got %v", stops)
	}
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【强制打断】") {
		t.Fatalf("force hint not injected: %q", hint)
	}
}

// TestHandleDebugEvent_RequestValidation covers the error paths: method,
// JSON, agent_id, unknown agent, category, event_type.
func TestHandleDebugEvent_RequestValidation(t *testing.T) {
	rt, _, _ := newWorldEventTestRuntime(t)

	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"method", httptest.NewRequest(http.MethodGet, "/debug/event", nil), http.StatusMethodNotAllowed},
		{"bad json", httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(`{nope`)), http.StatusBadRequest},
		{"missing agent_id", httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(`{"event":{"category":"world","event_type":"malfunction"}}`)), http.StatusBadRequest},
		{"unknown agent", httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(`{"agent_id":"H-99","event":{"category":"world","event_type":"malfunction"}}`)), http.StatusBadRequest},
		{"missing category", httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(`{"agent_id":"H-01","event":{"event_type":"malfunction"}}`)), http.StatusBadRequest},
		{"missing event_type", httptest.NewRequest(http.MethodPost, "/debug/event", strings.NewReader(`{"agent_id":"H-01","event":{"category":"world"}}`)), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleDebugEvent(testLogger(), rt.lookupAgent, rt.handleWorldEvent, rec, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestHandleDebugEvent_ManualModeReportsDropped verifies manual mode is
// reported honestly: injection succeeds, dispatch drops it by policy.
func TestHandleDebugEvent_ManualModeReportsDropped(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	rt.autoPlanEnabled = false

	code, resp := postDebugEvent(t, rt, `{
		"agent_id": "H-01",
		"event": {"category": "world", "event_type": "malfunction"}
	}`)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (policy drop is not a client error)", code)
	}
	if resp.OK {
		t.Fatalf("manual mode should report ok=false, got %+v", resp)
	}
	if !strings.Contains(resp.Note, "丢弃") {
		t.Fatalf("note should explain the drop, got %q", resp.Note)
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("manual mode must not enqueue, got %d", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("manual mode must not stop anything, got %v", stops)
	}
}

// TestNoSynthesisForSelfInterruptedCompletion pins the 2026-09-20 fix:
// a completion{interrupted} that was pre-recorded by our own stop (force/
// router/social interrupt) must NOT produce a detail string — it's an
// expected follow-up, not an anomaly. The bug: social interrupt → stop →
// UE returns interrupted → synthesized as action_failed → router judges
// urgent → interrupts the just-dispatched social response → infinite loop.
func TestNoSynthesisForSelfInterruptedCompletion(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	// 模拟路由打断：recordInterrupted 先行（设置 preface 标志）→ stop。
	ac.as.RecordActionStarted("act-1", "InteractSmartObject",
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")
	ac.as.RecordActionInterrupted("InteractSmartObject(workbench)", "被紧急事件打断")
	ac.as.ClearInFlightKeepQueue()

	// 被打断的动作回 interrupted completion——应返回空 detail（不合成事件）。
	_, detail, _ := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-1", Result: protocol.ResultInterrupted,
	}, nil, nil, nil)
	if detail != "" {
		t.Fatalf("pre-recorded interrupted completion must NOT produce a detail, got %q", detail)
	}

	// 对照：非自己打断的 interrupted（UE 自身原因）→ 产生 detail。
	ac.as.RecordActionStarted("act-2", "MoveTo", nil, agentstate.SourceTactical, "")
	_, detail, _ = ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-2", Result: protocol.ResultInterrupted, Reason: "dialogue:abandoned by H-03",
	}, nil, nil, nil)
	if detail == "" {
		t.Fatalf("non-pre-recorded interrupted must produce a detail for synthesis")
	}
}

// TestNoSynthesisForTimeToStopAndUntracked verifies the remaining two
// false-positive sources are fixed:
//  1. time_to_stop expiry: checkTimeToStop sets the preface flag → subsequent
//     interrupted completion produces no detail (planned segment switch).
//  2. Untracked action (cold-start sleep): WasInFlight=false + interrupted →
//     no detail (NPC born sleeping, first dispatch interrupts it naturally).
func TestNoSynthesisForTimeToStopAndUntracked(t *testing.T) {
	// ── 1. time_to_stop 到期 ──
	ac, _ := newAgentContext(context.Background())
	ac.as.RecordActionStarted("act-tts", "InteractSmartObject",
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")

	// 模拟 checkTimeToStop 的行为：recordScheduledEnd → ClearInFlightKeepQueue。
	ac.recordScheduledEnd()
	ac.as.ClearInFlightKeepQueue()

	_, detail, _ := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-tts", Result: protocol.ResultInterrupted,
	}, nil, nil, nil)
	if detail != "" {
		t.Fatalf("time_to_stop interrupted must NOT produce detail, got %q", detail)
	}

	// ── 2. 冷启动未追踪动作 ──
	ac2, _ := newAgentContext(context.Background())
	// 不调 recordActionStarted——模拟 start_asleep 从未被追踪。
	_, detail, _ = ac2.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "start_asleep_H-04", Result: protocol.ResultInterrupted,
	}, nil, nil, nil)
	if detail != "" {
		t.Fatalf("untracked interrupted must NOT produce detail, got %q", detail)
	}

	// 对照：UE 真正的 failed → 照常产生 detail。
	ac2.as.RecordActionStarted("act-real", "MoveTo", nil, agentstate.SourceTactical, "")
	_, detail, _ = ac2.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-real", Result: protocol.ResultFailed, Reason: "unreachable",
	}, nil, nil, nil)
	if detail == "" {
		t.Fatalf("genuine failed must still produce detail for synthesis")
	}
}

// TestReleaseFarewellSpeaker_OnDelivery verifies the ends_conversation
// release: dispatching a speech_heard to the addressee stops the SPEAKER's
// in-flight farewell speak_to (delivery confirmed = its mission complete),
// clears in-flight tracking, and wakes the speaker's worker — skipping the
// UE no_reply window. Negative: a speak_to without the flag, or a non-speak_to
// in-flight action, is left alone.
func TestReleaseFarewellSpeaker_OnDelivery(t *testing.T) {
	rt, ft, sp := newWorldEventTestRuntime(t)
	seedPerception(t, sp)
	// 注册接收方 H-02（fixture 默认只注册 H-01——说话人）。
	recv, _ := newAgentContext(context.Background())
	rt.agentsMu.Lock()
	rt.agents["H-02"] = recv
	rt.agentsMu.Unlock()

	mkParams := func(ends bool) map[string]any {
		p := map[string]any{"target_agent_id": "H-02", "content": "回头见，跑完各忙各的。", "interrupt": true}
		if ends {
			p["ends_conversation"] = true
		}
		return p
	}

	// 正例：带 ends_conversation 的在途 speak_to → speech_heard 送达即 stop。
	sp.as.RecordActionStarted("act-fw1", "SpeakTo", mkParams(true), agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", replySpeechTestEvent("evt_fw1", "H-01"))
	stops := stoppedActions(ft)
	if len(stops) != 1 || stops[0] != "act-fw1" {
		t.Fatalf("farewell speak_to must be stopped on delivery, got %v", stops)
	}
	if sp.as.CurrentActionID() != "" {
		t.Fatalf("speaker in-flight must be cleared, got %q", sp.as.CurrentActionID())
	}

	// 反例 1：不带标记的 speak_to（普通对话轮）→ 不 stop（要等对方回话打断）。
	sp.as.RecordActionStarted("act-fw2", "SpeakTo", mkParams(false), agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", replySpeechTestEvent("evt_fw2", "H-01"))
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("unflagged speak_to must NOT be stopped, got %v", stops)
	}

	// 反例 2：非 speak_to 在途动作（即使带同名参数）→ 不 stop。
	sp.as.ClearInFlightKeepQueue()
	sp.as.RecordActionStarted("act-fw3", "InteractSmartObject", map[string]any{
		"semantic_group": "treadmill", "ends_conversation": true}, agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", replySpeechTestEvent("evt_fw3", "H-01"))
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("non-speak_to in-flight must NOT be stopped, got %v", stops)
	}
}

// TestCorrectUnansweredHint_OnLateReply 验证迟到回复的 hint 修正（2026-11-03）：
// no_reply 终局注入的「未获应答」hint（"没有回应不等于答应"），在对方回话
// 终于到达（会话经 carryover 重开、出现 peer 轮次）后立即被重写为正常守约
// hint——"未获应答"的判断已过期（实测：邀请方按"对方不理我"走人后 0.04s
// 对方的同意才到）；非未获应答 hint 不动。
func TestCorrectUnansweredHint_OnLateReply(t *testing.T) {
	rt, _, ac := newWorldEventTestRuntime(t)
	// 复现 no_reply 终局后的状态：会话关闭（只有自己的邀请轮）+ 未获应答 hint。
	ac.recordConversationTurn("H-01", "H-05", "老张，走，去跑步机跑两步？", 100)
	peer, turns := ac.closeConversationByPeer("H-05")
	ac.as.SetReplanHint(conversationEndedHint("H-01", peer, turns, nil))
	if !strings.Contains(ac.as.ReplanHint(), "没有回应不等于答应") {
		t.Fatalf("precondition: unanswered hint, got %.120s", ac.as.ReplanHint())
	}

	// 迟到的同意到达：carryover 重开（出现 peer 轮次）→ 修正。
	ac.recordConversationTurn("H-05", "H-05", "好嘞，走！一起去跑。", 110)
	rt.correctUnansweredHint(ac, "H-01", "H-05", *rt.kbPtr)
	fixed := ac.as.ReplanHint()
	if strings.Contains(fixed, "没有回应不等于答应") {
		t.Fatalf("stale unanswered hint must be rewritten, got %.120s", fixed)
	}
	if !strings.Contains(fixed, "立即去执行该约定") {
		t.Fatalf("rewritten hint must be the follow-through variant, got %.120s", fixed)
	}

	// 非未获应答 hint 不受影响。
	ac.as.SetReplanHint("【强制打断】其他事件在途")
	rt.correctUnansweredHint(ac, "H-01", "H-05", *rt.kbPtr)
	if ac.as.ReplanHint() != "【强制打断】其他事件在途" {
		t.Fatalf("non-unanswered hint must be left alone, got %q", ac.as.ReplanHint())
	}
}

// farewellReplyTestEvent is replySpeechTestEvent with ends_conversation=true:
// the speaker's utterance closes the conversation and expects no reply.
func farewellReplyTestEvent(id, speaker string) protocol.WorldEventPayload {
	ev := replySpeechTestEvent(id, speaker)
	ev.Data = json.RawMessage(`{"speaker_id":"` + speaker + `","speaker_name":"老陈","addressee_id":"H-02","content":"好，就这么定了，下午见！","ends_conversation":true}`)
	return ev
}

// TestReleaseFarewellListener_OnFarewellReply verifies the receiver-side
// mirror of releaseFarewellSpeaker: a reply carrying ends_conversation=true
// arriving at an agent whose in-flight speak_to targets the speaker (it is
// standing there waiting for that very reply) releases the wait immediately —
// stop + in-flight cleared + conversation closed + follow-through hint
// injected + social reaction window lifted — instead of standing through UE's
// late speech:replied completion (one full no-reply window after the reply,
// 2026-11-02 sim). Negatives: a reply without the flag, a speak_to waiting on
// a third party, and a non-speak_to in-flight action are all left alone.
func TestReleaseFarewellListener_OnFarewellReply(t *testing.T) {
	rt, ft, _ := newWorldEventTestRuntime(t)
	// 注册接收方 H-02（fixture 默认只注册 H-01）。H-02 在途的 speak_to
	// 指向 H-01 —— 它就是那位站着等回话的邀请方。
	recv, _ := newAgentContext(context.Background())
	rt.agentsMu.Lock()
	rt.agents["H-02"] = recv
	rt.agentsMu.Unlock()

	// 会话已有自己的邀请轮（recordActionStarted 的 own-turn 挂钩产物）。
	recv.recordConversationTurn("H-02", "H-01", "老陈，下午四点广场生日派对记得来！", 38823)
	// 社交反应窗口 armed（对话开启）——释放时应随对话终局一并清除。
	recv.beginReaction(2, 38823, "社交对话", true)

	waitParams := func(target string) map[string]any {
		return map[string]any{
			"target_agent_id": target, "content": "老陈，下午四点广场生日派对记得来！",
			"ends_conversation": false, "interrupt": true,
		}
	}

	// 正例：等待 H-01 回话的 speak_to + 带收尾标记的回话到达 → 立即释放。
	recv.as.RecordActionStarted("act-wait1", "SpeakTo", waitParams("H-01"), agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", farewellReplyTestEvent("evt_fl1", "H-01"))
	stops := stoppedActions(ft)
	if len(stops) != 1 || stops[0] != "act-wait1" {
		t.Fatalf("waiting speak_to must be stopped on farewell reply, got %v", stops)
	}
	if recv.as.CurrentActionID() != "" {
		t.Fatalf("receiver in-flight must be cleared, got %q", recv.as.CurrentActionID())
	}
	if peer, turns := recv.conversationPeerState(); peer != "" || turns != 0 {
		t.Fatalf("conversation must be closed, got peer=%q turns=%d", peer, turns)
	}
	if hint := recv.as.ReplanHint(); !strings.Contains(hint, "【对话结束】") {
		t.Fatalf("follow-through hint must be injected, got %q", hint)
	}
	if active, _, _ := recv.reactionSnapshot(); active {
		t.Fatal("social reaction window must be lifted with the conversation end")
	}

	// 反例 1：不带收尾标记的普通回话 → 不释放（对话可能继续，等待合法）。
	recv.recordConversationTurn("H-02", "H-01", "那我先去忙了。", 39000)
	recv.as.RecordActionStarted("act-wait2", "SpeakTo", waitParams("H-01"), agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", replySpeechTestEvent("evt_fl2", "H-01"))
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("unflagged reply must NOT release the waiter, got %v", stops)
	}

	// 反例 2：在途 speak_to 等的是第三方（H-03）→ 说话人的收尾不释放它。
	recv.as.ClearInFlightKeepQueue()
	recv.as.RecordActionStarted("act-wait3", "SpeakTo", waitParams("H-03"), agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", farewellReplyTestEvent("evt_fl3", "H-01"))
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("third-party wait must NOT be released, got %v", stops)
	}

	// 反例 3：非 speak_to 在途动作 → 不释放。
	recv.as.ClearInFlightKeepQueue()
	recv.as.RecordActionStarted("act-wait4", "InteractSmartObject", map[string]any{
		"semantic_group": "treadmill"}, agentstate.SourceTactical, "")
	dispatchTestEvent(t, rt, "H-02", farewellReplyTestEvent("evt_fl4", "H-01"))
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("non-speak_to in-flight must NOT be released, got %v", stops)
	}
}
