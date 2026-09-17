package main

import (
	"context"
	"encoding/json"
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
