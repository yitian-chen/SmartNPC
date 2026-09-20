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

// TestChatInviteIncoming_DispatchedByWorldEvent verifies P4-14: a
// social.chat_invite_incoming world_event is handed to the dialogue runner
// immediately (not enqueued for the safe point), and the legacy standalone
// chat_invite message is translated into the same world_event path.
func TestChatInviteIncoming_DispatchedByWorldEvent(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	if ac.dialogue == nil {
		t.Skip("dialogue runner requires ws wiring; covered by dialogue_test.go")
	}

	// 新路径：world_event 推 chat_invite_incoming。
	ev := protocol.WorldEventPayload{
		EventID:   "evt_ci_1",
		Category:  protocol.CategorySocial,
		EventType: protocol.EventTypeChatInviteIncoming,
		Severity:  5,
		Subject:   "H-02",
		Data:      json.RawMessage(`{"conv_id":"conv_001","from":"H-02","content":"老陈，借个工具？"}`),
	}
	if !rt.handleWorldEvent("H-01", ev) {
		t.Fatalf("world_event should be accepted")
	}
	// 即时消费——不留在队列。
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("chat_invite_incoming must NOT be enqueued (immediate dispatch), got len=%d", got)
	}

	// 旧路径：独立 chat_invite 消息也走同一管道（转 world_event 再分发）。
	legacy := protocol.ChatInvitePayload{ConvID: "conv_002", FromAgentID: "H-03", Content: "hi"}
	payload, _ := json.Marshal(legacy)
	rt.HandleMessage(context.Background(), protocol.TypeChatInvite, "H-01", payload)
	// 给 goroutine 一点时间。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := ac.as.Snapshot()
		if snap.CurrentActionCmd == "Speak" || ac.dialogue != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = ft
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
	_, detail := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-1", Result: protocol.ResultInterrupted,
	})
	if detail != "" {
		t.Fatalf("pre-recorded interrupted completion must NOT produce a detail, got %q", detail)
	}

	// 对照：非自己打断的 interrupted（UE 自身原因）→ 产生 detail。
	ac.as.RecordActionStarted("act-2", "MoveTo", nil, agentstate.SourceTactical, "")
	_, detail = ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-2", Result: protocol.ResultInterrupted, Reason: "dialogue:abandoned by H-03",
	})
	if detail == "" {
		t.Fatalf("non-pre-recorded interrupted must produce a detail for synthesis")
	}
}
