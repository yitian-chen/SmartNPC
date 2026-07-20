package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
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
	ac.endDecision(epoch)

	reasons, _, err := ac.observePerception(perceptionWithScanID(base, "scan_1"))
	if err != nil || !containsReason(reasons, reasonScanResponse) {
		t.Fatalf("scan response reasons=%v err=%v", reasons, err)
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
	ac.endDecision(epoch)

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

func TestAgentContext_PendingScanInvalidatesRemainingToolTurn(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	_, epoch, ok := ac.beginDecision()
	if !ok {
		t.Fatal("could not start decision")
	}
	if err := ac.armScan(epoch, "scan_1"); err != nil {
		t.Fatal(err)
	}
	if err := ac.validateDecision(epoch); err == nil || !strings.Contains(err.Error(), "scan response pending") {
		t.Fatalf("post-scan tool call was not rejected: %v", err)
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

	reasons := ac.updateState(protocol.StateReportPayload{
		PhysicalState:       protocol.PhysicalState{Energy: 100, Fatigue: 0, Health: 100},
		CurrentTaskProgress: &protocol.CurrentTaskProgress{ActionID: "act_1", Progress: 0.1},
	})
	if len(reasons) != 1 || reasons[0] != "任务开始:act_1" {
		t.Fatalf("task start reasons=%v", reasons)
	}
	_ = ac.takeDecision()

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
