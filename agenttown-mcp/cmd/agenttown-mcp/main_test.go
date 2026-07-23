package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

func perceptionJSON(timeOfDay, zone, location, weather string, audible []protocol.AudibleEvent) json.RawMessage {
	z, l := zone, location
	payload := protocol.PerceptionPayload{
		Location:      protocol.Location{CurrentZone: &z, CurrentLocation: &l},
		VisibleAgents: []protocol.VisibleAgent{},
		NearbyObjects: []protocol.NearbyObject{{
			ID: "workbench_01", State: "idle", AvailableActions: []string{"assemble", "inspect"},
		}},
		AudibleEvents: audible,
		Environment:   protocol.Environment{TimeOfDay: timeOfDay, Weather: weather},
	}
	raw, _ := json.Marshal(payload)
	return raw
}

func perceptionWithScanID(raw json.RawMessage, scanID string) json.RawMessage {
	var payload protocol.PerceptionPayload
	_ = json.Unmarshal(raw, &payload)
	payload.ScanID = scanID
	out, _ := json.Marshal(payload)
	return out
}

func TestAgentContext_PerceptionGateAndLatestWins(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	first := perceptionJSON("06:30", "main_workshop", "", "clear", nil)
	reasons, _, err := ac.observePerception(first)
	if err != nil || !containsReason(reasons, reasonFirstPerception) {
		t.Fatalf("first perception reasons=%v err=%v", reasons, err)
	}

	// Pure time change updates the latest snapshot but does not trigger.
	timeOnly := perceptionJSON("07:00", "main_workshop", "", "clear", nil)
	reasons, replaced, err := ac.observePerception(timeOnly)
	if err != nil || len(reasons) != 0 {
		t.Fatalf("time-only perception triggered: reasons=%v err=%v", reasons, err)
	}
	if !replaced {
		t.Fatal("latest time-only snapshot did not replace queued first snapshot")
	}
	work := ac.takeDecision()
	if work == nil || string(work.perception) != string(timeOnly) {
		t.Fatalf("worker did not receive latest snapshot: %#v", work)
	}
}

func TestAgentContext_ImportantChangesTrigger(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision()

	audible := []protocol.AudibleEvent{{Type: "scenario", Source: "director", Content: "传送带异常"}}
	reasons, _, err := ac.observePerception(perceptionJSON("07:00", "central_plaza", "plaza", "rain", audible))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{reasonZoneChanged, reasonLocationChanged, reasonAudibleEvent, reasonWeatherChanged} {
		if !containsReason(reasons, want) {
			t.Errorf("missing reason %q in %v", want, reasons)
		}
	}

	// Repeating the exact scan result must not recursively trigger.
	_ = ac.takeDecision()
	reasons, _, _ = ac.observePerception(perceptionJSON("07:00", "central_plaza", "plaza", "rain", audible))
	if len(reasons) != 0 {
		t.Fatalf("identical scan retriggered decision: %v", reasons)
	}
}

func TestAgentContext_MatchingScanResponseForcesExactlyOneDecision(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	base := perceptionJSON("07:00", "main_workshop", "", "clear", nil)
	_, _, _ = ac.observePerception(base)
	_ = ac.takeDecision()

	_, epoch, ok := ac.beginDecision()
	if !ok {
		t.Fatal("could not start decision")
	}
	if err := ac.armScan(epoch, "scan_1"); err != nil {
		t.Fatal(err)
	}

	// Scan response arrives while the scan-initiating decision is still active.
	// The current turn must stay active so that subsequent tool_calls from the
	// same LLM response (e.g. scan_area → move_to) are accepted with the
	// original epoch. The scan-followup is queued for the next turn.
	reasons, _, err := ac.observePerception(perceptionWithScanID(base, "scan_1"))
	if err != nil || !containsReason(reasons, reasonScanResponse) {
		t.Fatalf("scan response reasons=%v err=%v", reasons, err)
	}
	if err := ac.validateDecision(epoch); err != nil {
		t.Fatalf("current epoch was invalidated after scan response, blocking same-turn tool calls: %v", err)
	}
	work := ac.takeDecision()
	if work == nil || !work.scanFollowup {
		t.Fatalf("scan response did not create scan follow-up: %#v", work)
	}

	reasons, _, err = ac.observePerception(perceptionWithScanID(base, "scan_1"))
	if err != nil || len(reasons) != 0 || ac.takeDecision() != nil {
		t.Fatalf("duplicate scan response retriggered: reasons=%v err=%v", reasons, err)
	}
}

func TestAgentContext_UnmatchedScanDoesNotConsumePendingToken(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	base := perceptionJSON("07:00", "main_workshop", "", "clear", nil)
	_, _, _ = ac.observePerception(base)
	_ = ac.takeDecision()
	_, epoch, _ := ac.beginDecision()
	if err := ac.armScan(epoch, "scan_1"); err != nil {
		t.Fatal(err)
	}

	reasons, _, _ := ac.observePerception(perceptionWithScanID(base, "scan_other"))
	if len(reasons) != 0 {
		t.Fatalf("unmatched scan triggered: %v", reasons)
	}
	ac.mu.Lock()
	pending := ac.pendingScanID
	ac.mu.Unlock()
	if pending != "scan_1" {
		t.Fatalf("unmatched scan consumed token: %q", pending)
	}
}

func TestAgentContext_PendingScanDoesNotBlockSameTurnToolCalls(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, epoch, ok := ac.beginDecision()
	if !ok {
		t.Fatal("could not start decision")
	}
	if err := ac.armScan(epoch, "scan_1"); err != nil {
		t.Fatal(err)
	}
	// While the scan response has not arrived yet, the LLM may issue another
	// tool_call in the same response. It must still be accepted with the
	// current epoch — rejecting it caused Hermes' circuit breaker to trip
	// after 3 stale rejections.
	if err := ac.validateDecision(epoch); err != nil {
		t.Fatalf("post-scan tool call was rejected: %v", err)
	}
}

func TestAgentContext_ScanFollowupRejectsRecursiveScan(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, epoch, _ := ac.beginDecisionWithScan(true)
	if err := ac.armScan(epoch, "scan_recursive"); err == nil || !strings.Contains(err.Error(), "scan-response") {
		t.Fatalf("recursive scan was not rejected: %v", err)
	}
}

func TestAgentContext_StateAndCompletionTriggers(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision()

	// Task start (nil→non-nil) must NOT trigger a decision — the agent
	// already knows from the tool result. This prevents self-interruption
	// where the NPC sees "任务开始" and decides to stop/move instead of
	// waiting for the action to complete.
	reasons := ac.updateState(protocol.StateReportPayload{
		PhysicalState:       protocol.PhysicalState{Energy: 100, Fatigue: 0, Health: 100},
		CurrentTaskProgress: &protocol.CurrentTaskProgress{ActionID: "act_1", Progress: 0.1},
	})
	if len(reasons) != 0 {
		t.Fatalf("task start should not trigger reasons, got=%v", reasons)
	}
	// currentTask is still updated even without a decision trigger.
	if ac.currentTask == nil || ac.currentTask.ActionID != "act_1" {
		t.Fatalf("currentTask not updated: %#v", ac.currentTask)
	}

	// Progress-only update is cache-only.
	reasons = ac.updateState(protocol.StateReportPayload{
		PhysicalState:       protocol.PhysicalState{Energy: 99, Fatigue: 1, Health: 100},
		CurrentTaskProgress: &protocol.CurrentTaskProgress{ActionID: "act_1", Progress: 0.8},
	})
	if len(reasons) != 0 {
		t.Fatalf("progress-only update triggered: %v", reasons)
	}

	if !ac.recordActionCompletion(protocol.ActionCompletedPayload{ActionID: "act_1", Result: protocol.ResultSuccess, Progress: 1}) {
		t.Fatal("completion did not queue a decision from latest perception")
	}
	work := ac.takeDecision()
	if work == nil || len(work.reasons) == 0 || len(work.extras) == 0 {
		t.Fatalf("completion work incomplete: %#v", work)
	}
}

func TestAgentContext_PhysicalThresholdCrossing(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.updateState(protocol.StateReportPayload{PhysicalState: protocol.PhysicalState{Energy: 30, Fatigue: 20, JointWear: 10, Health: 100}})
	reasons := ac.updateState(protocol.StateReportPayload{PhysicalState: protocol.PhysicalState{Energy: 24, Fatigue: 20, JointWear: 10, Health: 100}})
	if len(reasons) != 1 || reasons[0] != "物理状态进入警戒带:energy<=25" {
		t.Fatalf("threshold reasons=%v", reasons)
	}
}

func TestLocalSummary_ContainsOnlyAuthoritativeState(t *testing.T) {
	physical := &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 3, Health: 100}
	task := &protocol.CurrentTaskProgress{ActionID: "act_1", Progress: 0.5}
	summary := buildLocalSummary(
		perceptionJSON("08:00", "main_workshop", "workbench_01", "clear", nil),
		physical,
		task,
		[]localActionSummary{{ActionID: "act_0", Result: "success", Progress: 1}},
		[]string{"传送带异常"},
		"", // dailyPlan — 测试不关心
	)
	for _, want := range []string{"08:00", "main_workshop", "act_0", "传送带异常"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}
	if strings.Contains(summary, "narrative") || strings.Contains(summary, "assistant") {
		t.Fatalf("summary contains non-authoritative narrative field: %s", summary)
	}
}

func TestAgentContext_DecisionEpochLifecycle(t *testing.T) {
	ac, _ := newAgentContext(context.Background(), 7)
	agentEpoch, decisionEpoch, ok := ac.beginDecision()
	if !ok || agentEpoch != 7 || decisionEpoch != 1 {
		t.Fatalf("beginDecision=(%d,%d,%v)", agentEpoch, decisionEpoch, ok)
	}
	if err := ac.validateDecision(decisionEpoch); err != nil {
		t.Fatalf("current decision rejected: %v", err)
	}
	ac.endDecision(decisionEpoch)
	if err := ac.validateDecision(decisionEpoch); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("inactive decision was not stale: %v", err)
	}
	_, nextEpoch, _ := ac.beginDecision()
	if nextEpoch != 2 {
		t.Fatalf("next epoch=%d, want 2", nextEpoch)
	}
	if err := ac.validateDecision(1); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("old epoch was not rejected: %v", err)
	}
}

func TestAgentContext_StopMakesAgentOffline(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, epoch, _ := ac.beginDecision()
	ac.stop()
	if err := ac.validateDecision(epoch); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("stopped agent validation=%v, want offline", err)
	}
}

func TestAgentContext_StopClearsPendingDecision(t *testing.T) {
	ac, ctx := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	ac.stop()
	if ctx.Err() == nil {
		t.Fatal("worker context was not canceled")
	}
	if got := ac.takeDecision(); got != nil {
		t.Fatalf("pending decision survived stop: %#v", got)
	}
}

// ─── 战术层队列抑制与 completion 路由 ──────────────────────────

// setQueueForTest 在测试中直接设置队列（绕过 mu 的 tacticalRefill 流程）。
func setQueueForTest(ac *agentContext, actions []plannedAction) {
	ac.mu.Lock()
	ac.actionQueue = actions
	ac.mu.Unlock()
}

func TestObservePerception_SuppressedDuringTacticalQueue(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// 先建立基线感知并消费掉
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision()

	// 设置队列非空（模拟战术执行中）
	setQueueForTest(ac, []plannedAction{{Action: "wait", Params: map[string]any{"duration_sec": 30}}})

	// 发送一个会触发决策的感知（区域变化 + 可听事件）
	audible := []protocol.AudibleEvent{{Type: "scenario", Source: "director", Content: "传送带异常"}}
	reasons, _, err := ac.observePerception(perceptionJSON("07:00", "central_plaza", "plaza", "rain", audible))
	if err != nil {
		t.Fatal(err)
	}
	if reasons != nil {
		t.Fatalf("perception should be suppressed during tactical queue, got reasons=%v", reasons)
	}
	// 不应有 pending 决策
	if work := ac.takeDecision(); work != nil {
		t.Fatalf("decision should not be queued during tactical execution: %#v", work)
	}
	// 但 latestPerception 应已更新（供 refill 用）
	ac.mu.Lock()
	raw := ac.latestPerception
	ac.mu.Unlock()
	if len(raw) == 0 {
		t.Fatal("latestPerception should still update during tactical execution")
	}
}

func TestObservePerception_TriggersWhenQueueEmpty(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision()

	// 队列为空，正常触发
	audible := []protocol.AudibleEvent{{Type: "scenario", Source: "director", Content: "传送带异常"}}
	reasons, _, err := ac.observePerception(perceptionJSON("07:00", "central_plaza", "plaza", "rain", audible))
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) == 0 {
		t.Fatal("perception should trigger when queue is empty")
	}
	if work := ac.takeDecision(); work == nil {
		t.Fatal("decision should be queued when queue is empty")
	}
}

func TestObservePerception_SuppressedWhenTacticalActionInFlight(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision()

	// 队列已空，但有战术层在途 action（最后一个 action 已 pop 但 UE 仍在执行 composite）
	ac.mu.Lock()
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	audible := []protocol.AudibleEvent{{Type: "scenario", Source: "director", Content: "传送带异常"}}
	reasons, _, err := ac.observePerception(perceptionJSON("07:00", "central_plaza", "plaza", "rain", audible))
	if err != nil {
		t.Fatal(err)
	}
	if reasons != nil {
		t.Fatalf("perception should be suppressed while tactical action in-flight, got reasons=%v", reasons)
	}
	if work := ac.takeDecision(); work != nil {
		t.Fatalf("decision should not be queued while tactical action in-flight: %#v", work)
	}
}

func TestUpdateState_SuppressedDuringTacticalQueue(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// 建立 currentTask
	ac.updateState(protocol.StateReportPayload{
		PhysicalState:       protocol.PhysicalState{Energy: 100, Fatigue: 0, Health: 100},
		CurrentTaskProgress: &protocol.CurrentTaskProgress{ActionID: "act_1", Progress: 0.5},
	})

	// 设置队列非空
	setQueueForTest(ac, []plannedAction{{Action: "wait", Params: map[string]any{"duration_sec": 30}}})

	// 任务结束（non-nil → nil）正常应触发，但队列非空时应抑制
	reasons := ac.updateState(protocol.StateReportPayload{
		PhysicalState:       protocol.PhysicalState{Energy: 90, Fatigue: 10, Health: 100},
		CurrentTaskProgress: nil,
	})
	if len(reasons) != 0 {
		t.Fatalf("task end should be suppressed during tactical queue, got reasons=%v", reasons)
	}
	// currentTask 仍应更新
	ac.mu.Lock()
	task := ac.currentTask
	ac.mu.Unlock()
	if task != nil {
		t.Fatalf("currentTask should be cleared, got %#v", task)
	}
}

func TestRecordActionCompletion_TacticalSourceSignalsOnly(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))
	_ = ac.takeDecision() // 消费首次感知决策

	// 设置为战术来源的在途 action
	ac.mu.Lock()
	ac.currentActionID = "act_t1"
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	// 排空 wake 通道
	select {
	case <-ac.wake:
	default:
	}

	queued := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act_t1", Result: protocol.ResultSuccess, Progress: 1,
	})
	if !queued {
		t.Fatal("tactical completion should return true (handled)")
	}
	// 不应入队 Hermes 决策
	if work := ac.takeDecision(); work != nil {
		t.Fatalf("tactical completion should not queue Hermes decision: %#v", work)
	}
	// 应 signal worker（wake 通道有值）
	select {
	case <-ac.wake:
		// good
	default:
		t.Fatal("tactical completion should signal worker via wake channel")
	}
	// currentActionSrc 应已清空
	ac.mu.Lock()
	src := ac.currentActionSrc
	ac.mu.Unlock()
	if src != "" {
		t.Fatalf("currentActionSrc should be cleared, got %q", src)
	}
}

func TestRecordActionCompletion_HermesSourceQueuesDecision(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))

	// 设置为 Hermes 来源的在途 action
	ac.mu.Lock()
	ac.currentActionID = "act_h1"
	ac.currentActionSrc = sourceHermes
	ac.mu.Unlock()

	queued := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act_h1", Result: protocol.ResultSuccess, Progress: 1,
	})
	if !queued {
		t.Fatal("hermes completion should queue a decision")
	}
	// 应入队 Hermes 决策
	work := ac.takeDecision()
	if work == nil {
		t.Fatal("hermes completion should queue Hermes decision")
	}
	if !containsReason(work.reasons, "动作完成:act_h1") {
		t.Errorf("work reasons missing completion: %v", work.reasons)
	}
}

func TestRecordEventNotification_ClearsQueue(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, _, _ = ac.observePerception(perceptionJSON("06:30", "main_workshop", "", "clear", nil))

	// 设置队列非空
	setQueueForTest(ac, []plannedAction{
		{Action: "move_to", Params: map[string]any{"target": "main_workshop"}},
		{Action: "wait", Params: map[string]any{"duration_sec": 30}},
	})
	ac.mu.Lock()
	ac.currentSlot = "08:00-12:00"
	ac.redecomposeCount = 1
	ac.mu.Unlock()

	queued := ac.recordEventNotification(protocol.EventNotificationPayload{
		EventID:         "evt_001",
		PerceptionLevel: "audible",
		Event:           map[string]any{"type": "alert"},
	})
	if !queued {
		t.Fatal("event notification should queue a decision")
	}
	// 队列应被清空
	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	slot := ac.currentSlot
	count := ac.redecomposeCount
	ac.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("queue should be cleared, got %d items", queueLen)
	}
	if slot != "" {
		t.Errorf("currentSlot should be cleared, got %q", slot)
	}
	if count != 0 {
		t.Errorf("redecomposeCount should be reset, got %d", count)
	}
}

func TestClearQueueAndStopInFlight_NoInFlightNoStop(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ws := wsserver.New(wsserver.Options{})
	setQueueForTest(ac, []plannedAction{{Action: "wait", Params: map[string]any{"duration_sec": 30}}})

	// 无在途 action
	ac.clearQueueAndStopInFlight("H-01", ws, slog.Default())

	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	ac.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("queue should be cleared, got %d items", queueLen)
	}
}

func TestClearQueueAndStopInFlight_NoCrashWithDisconnectedWS(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ws := wsserver.New(wsserver.Options{}) // 未连接
	setQueueForTest(ac, []plannedAction{{Action: "wait", Params: map[string]any{"duration_sec": 30}}})

	// 有在途战术 action 但 UE 未连接——不应崩溃，stop_action 被跳过
	ac.mu.Lock()
	ac.currentActionID = "act_t1"
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	ac.clearQueueAndStopInFlight("H-01", ws, slog.Default())

	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	ac.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("queue should be cleared even with disconnected WS, got %d", queueLen)
	}
}

func TestPopAndSendQueueAction_RefillOnBusyRejection(t *testing.T) {
	// 模拟 UE 拒绝（busy with composite）：SendAction 在未连接 ws 上一定失败。
	// 当 currentActionSrc == sourceTactical（有在途 composite）时，被拒 action
	// 应回填到队首，而不是 signal → 整队消耗光。
	ac, _ := newAgentContext(context.Background())
	ws := wsserver.New(wsserver.Options{}) // 未连接 → SendAction 失败
	logger := slog.Default()
	kb := loadTestKB(t)

	// 队列 3 个 action
	setQueueForTest(ac, []plannedAction{
		{Action: "wait", Params: map[string]any{"duration_sec": 30}},
		{Action: "wait", Params: map[string]any{"duration_sec": 60}},
		{Action: "wait", Params: map[string]any{"duration_sec": 90}},
	})

	// 有在途战术 action（最后一个已 pop 但未完成）
	ac.mu.Lock()
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	ac.popAndSendQueueAction(context.Background(), "H-01", ws, kb, logger)

	// 回填后队列仍为 3，且队首仍是第一个 action
	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	firstAction := ""
	if queueLen > 0 {
		firstAction = ac.actionQueue[0].Action
	}
	ac.mu.Unlock()
	if queueLen != 3 {
		t.Fatalf("queue should be refilled to 3 after busy rejection, got %d", queueLen)
	}
	if firstAction != "wait" {
		t.Fatalf("first action should be 'wait' after refill, got %q", firstAction)
	}
}

func TestRecordActionStarted_SetsSource(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	ac.recordActionStarted("act_1", "MoveTo", map[string]any{"target": "main_workshop"}, 1, sourceTactical)
	ac.mu.Lock()
	src := ac.currentActionSrc
	id := ac.currentActionID
	ac.mu.Unlock()
	if src != sourceTactical {
		t.Fatalf("currentActionSrc=%q, want tactical", src)
	}
	if id != "act_1" {
		t.Fatalf("currentActionID=%q, want act_1", id)
	}

	ac.recordActionStarted("act_2", "Wait", map[string]any{"duration_sec": 30}, 2, sourceHermes)
	ac.mu.Lock()
	src = ac.currentActionSrc
	id = ac.currentActionID
	ac.mu.Unlock()
	if src != sourceHermes {
		t.Fatalf("currentActionSrc=%q, want hermes", src)
	}
	if id != "act_2" {
		t.Fatalf("currentActionID=%q, want act_2", id)
	}
}
