package agentstate

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// setPerceptionForTest builds a perception payload with the given game time
// (HH:MM) and day_count, then stores it on the AgentState.
func setPerceptionForTest(t *testing.T, a *AgentState, hhmm string, dayCount int) {
	t.Helper()
	var h, m int
	if n, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil || n != 2 {
		t.Fatalf("invalid hhmm %q: %v", hhmm, err)
	}
	totalSec := h*3600 + m*60
	raw := []byte(fmt.Sprintf(
		`{"environment":{"game_time_sec":%d,"time_of_day_sec":%d,"day_count":%d,"time_scale":60}}`,
		dayCount*86400+totalSec, totalSec, dayCount))
	if _, err := a.SetPerception(raw); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
}

func TestNew_Defaults(t *testing.T) {
	a := New()
	if a.CurrentDay() != -1 {
		t.Errorf("currentDay = %d, want -1", a.CurrentDay())
	}
	if a.LatestDayCount() != -1 {
		t.Errorf("LatestDayCount = %d, want -1 (no perception yet)", a.LatestDayCount())
	}
	if a.LatestTimeOfDay() != "" {
		t.Errorf("LatestTimeOfDay = %q, want empty", a.LatestTimeOfDay())
	}
	if a.HasQueueNext() {
		t.Error("HasQueueNext = true, want false on new state")
	}
	if a.HasInFlightAction() {
		t.Error("HasInFlightAction = true, want false on new state")
	}
}

func TestSetPerception_UpdatesZoneAndCount(t *testing.T) {
	a := New()
	raw := []byte(`{
		"environment":{"game_time_sec":25200,"time_of_day_sec":25200,"day_count":0,"time_scale":60},
		"location":{"current_zone":"workshop"},
		"nearby_objects":[{"id":"workbench_01"}]
	}`)
	upd, err := a.SetPerception(raw)
	if err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	if upd.CurZone != "workshop" {
		t.Errorf("CurZone = %q, want workshop", upd.CurZone)
	}
	if upd.PerceptionCount != 1 {
		t.Errorf("PerceptionCount = %d, want 1", upd.PerceptionCount)
	}
	if a.LatestZone() != "workshop" {
		t.Errorf("LatestZone = %q, want workshop", a.LatestZone())
	}
	if a.LatestTimeOfDay() != "07:00" {
		t.Errorf("LatestTimeOfDay = %q, want 07:00", a.LatestTimeOfDay())
	}
}

func TestSetPerception_InvalidJSON(t *testing.T) {
	a := New()
	if _, err := a.SetPerception(json.RawMessage("not json")); err == nil {
		t.Error("SetPerception expected error for invalid JSON")
	}
}

func TestRefillQueue_PopPeek(t *testing.T) {
	a := New()
	actions := []PlannedAction{
		{Action: "move_to_location", Params: map[string]any{"target": "workbench_01"}},
		{Action: "work_at_workbench", Params: map[string]any{"duration_sec": 3600}},
	}
	a.RefillQueue(actions, "07:00-11:00")

	if !a.HasQueueNext() {
		t.Fatal("HasQueueNext = false after RefillQueue")
	}
	if a.QueueLen() != 2 {
		t.Errorf("QueueLen = %d, want 2", a.QueueLen())
	}

	// Peek should not remove.
	if peek, ok := a.PeekAction(); !ok || peek.Action != "move_to_location" {
		t.Errorf("PeekAction = %+v ok=%v, want move_to_location", peek, ok)
	}
	if a.QueueLen() != 2 {
		t.Errorf("QueueLen after Peek = %d, want 2", a.QueueLen())
	}

	// Pop FIFO.
	pop, ok := a.PopAction()
	if !ok || pop.Action != "move_to_location" {
		t.Errorf("PopAction = %+v ok=%v, want move_to_location", pop, ok)
	}
	if a.QueueLen() != 1 {
		t.Errorf("QueueLen after Pop = %d, want 1", a.QueueLen())
	}

	pop2, ok := a.PopAction()
	if !ok || pop2.Action != "work_at_workbench" {
		t.Errorf("second PopAction = %+v, want work_at_workbench", pop2)
	}
	if a.HasQueueNext() {
		t.Error("HasQueueNext = true after draining queue")
	}
	if _, ok := a.PopAction(); ok {
		t.Error("PopAction on empty queue should return ok=false")
	}
}

func TestSnapshotSchedule(t *testing.T) {
	a := New()
	a.SetDailyPlan("07:00-11:00 装配\n11:00-12:00 午餐", 0)
	a.RefillQueue([]PlannedAction{{Action: "work_at_workbench"}}, "07:00-11:00")
	a.SetCurrentPlanIndex(1)

	plan, slot, idx := a.SnapshotSchedule()
	if plan != "07:00-11:00 装配\n11:00-12:00 午餐" {
		t.Errorf("plan = %q", plan)
	}
	if slot != "07:00-11:00" {
		t.Errorf("slot = %q, want 07:00-11:00", slot)
	}
	if idx != 1 {
		t.Errorf("idx = %d, want 1", idx)
	}
}

func TestDetectDayRollover_FirstSyncNoRollover(t *testing.T) {
	a := New()
	setPerceptionForTest(t, a, "06:00", 0)
	rollover, prev, newDay := a.DetectDayRollover()
	if rollover {
		t.Error("first sync should not trigger rollover")
	}
	if prev != -1 {
		t.Errorf("prev = %d, want -1", prev)
	}
	if newDay != 0 {
		t.Errorf("newDay = %d, want 0", newDay)
	}
	if a.CurrentDay() != 0 {
		t.Errorf("CurrentDay = %d, want 0 after first sync", a.CurrentDay())
	}
}

func TestDetectDayRollover_SameDayNoRollover(t *testing.T) {
	a := New()
	setPerceptionForTest(t, a, "06:00", 0)
	a.DetectDayRollover() // first sync
	setPerceptionForTest(t, a, "12:00", 0)
	rollover, _, _ := a.DetectDayRollover()
	if rollover {
		t.Error("same day should not trigger rollover")
	}
}

func TestDetectDayRollover_DayIncrementTriggersRollover(t *testing.T) {
	a := New()
	setPerceptionForTest(t, a, "06:00", 0)
	a.DetectDayRollover() // first sync, currentDay=0
	setPerceptionForTest(t, a, "06:00", 1)
	rollover, prev, newDay := a.DetectDayRollover()
	if !rollover {
		t.Error("day increment should trigger rollover")
	}
	if prev != 0 {
		t.Errorf("prev = %d, want 0", prev)
	}
	if newDay != 1 {
		t.Errorf("newDay = %d, want 1", newDay)
	}
}

func TestDetectDayRollover_NoPerceptionNoRollover(t *testing.T) {
	a := New()
	rollover, prev, newDay := a.DetectDayRollover()
	if rollover {
		t.Error("no perception should not trigger rollover")
	}
	if prev != -1 || newDay != -1 {
		t.Errorf("prev=%d newDay=%d, want -1/-1", prev, newDay)
	}
}

func TestRecordActionStarted_Completion(t *testing.T) {
	a := New()
	a.RecordActionStarted("act-1", "WorkAtWorkbench", map[string]any{"duration_sec": 60}, SourceTactical)

	if !a.HasInFlightAction() {
		t.Error("HasInFlightAction = false after RecordActionStarted")
	}
	snap := a.Snapshot()
	if snap.CurrentActionID != "act-1" || snap.CurrentActionCmd != "WorkAtWorkbench" || snap.CurrentActionSrc != SourceTactical {
		t.Errorf("snapshot = %+v", snap)
	}

	res := a.RecordActionCompletion("act-1")
	if !res.WasInFlight {
		t.Error("WasInFlight = false, want true")
	}
	if res.Src != SourceTactical {
		t.Errorf("Src = %q, want tactical", res.Src)
	}
	if a.HasInFlightAction() {
		t.Error("HasInFlightAction = true after completion")
	}
}

func TestRecordActionCompletion_PendingStopMatch(t *testing.T) {
	a := New()
	a.SetPendingStopActionID("act-old")
	res := a.RecordActionCompletion("act-old")
	if !res.WasPendingStop {
		t.Error("WasPendingStop = false, want true")
	}
	if a.PendingStopActionID() != "" {
		t.Errorf("PendingStopActionID = %q, want cleared", a.PendingStopActionID())
	}
}

func TestRecordActionCompletion_SelfStopMatch(t *testing.T) {
	a := New()
	a.SetSelfStopInProgress("act-stop")
	res := a.RecordActionCompletion("act-stop")
	if !res.WasSelfStop {
		t.Error("WasSelfStop = false, want true")
	}
	if a.SelfStopInProgress() != "" {
		t.Errorf("SelfStopInProgress = %q, want cleared", a.SelfStopInProgress())
	}
}

func TestClearForSlotSwitch(t *testing.T) {
	a := New()
	a.RefillQueue([]PlannedAction{{Action: "work"}}, "07:00-11:00")
	a.RecordActionStarted("act-1", "WorkAtWorkbench", nil, SourceTactical)

	info := a.ClearForSlotSwitch()
	if info.ActionID != "act-1" {
		t.Errorf("info.ActionID = %q, want act-1", info.ActionID)
	}
	if info.ActionCmd != "WorkAtWorkbench" {
		t.Errorf("info.ActionCmd = %q", info.ActionCmd)
	}
	if info.QueueLen != 1 {
		t.Errorf("info.QueueLen = %d, want 1", info.QueueLen)
	}
	if a.HasQueueNext() {
		t.Error("HasQueueNext = true after ClearForSlotSwitch")
	}
	if a.HasInFlightAction() {
		t.Error("HasInFlightAction = true after ClearForSlotSwitch")
	}
	_, slot, _ := a.SnapshotSchedule()
	if slot != "" {
		t.Errorf("slot = %q, want empty after clear", slot)
	}
}

func TestStop_ClearsTransient(t *testing.T) {
	a := New()
	a.SetOnline(true)
	a.RefillQueue([]PlannedAction{{Action: "work"}}, "07:00-11:00")
	a.RecordActionStarted("act-1", "MoveTo", nil, SourceTactical)
	a.Stop()

	snap := a.Snapshot()
	if snap.Online {
		t.Error("Online = true after Stop, want false")
	}
	if snap.CurrentActionID != "" {
		t.Error("CurrentActionID not cleared by Stop")
	}
	if len(snap.ActionQueue) != 0 {
		t.Error("ActionQueue not cleared by Stop")
	}
	if snap.CurrentSlot != "" {
		t.Error("CurrentSlot not cleared by Stop")
	}
}

func TestSetPhysicalState_ReturnsPrev(t *testing.T) {
	a := New()
	prev1 := protocol.PhysicalState{Energy: 100}
	a.SetPhysicalState(&prev1, nil)
	prev2 := protocol.PhysicalState{Energy: 80}
	returned := a.SetPhysicalState(&prev2, nil)
	if returned == nil || returned.Energy != 100 {
		t.Errorf("returned prev = %+v, want Energy=100", returned)
	}
}

func TestSetReplanHint(t *testing.T) {
	a := New()
	a.SetReplanHint("zone changed to workshop")
	snap := a.Snapshot()
	if snap.ReplanHint != "zone changed to workshop" {
		t.Errorf("ReplanHint = %q", snap.ReplanHint)
	}
}

func TestSnapshot_DeepCopy(t *testing.T) {
	a := New()
	original := []PlannedAction{{Action: "work", Params: map[string]any{"x": 1}}}
	a.RefillQueue(original, "07:00-11:00")

	snap := a.Snapshot()
	// Mutate snapshot, source must not change.
	snap.ActionQueue[0].Action = "mutated"
	snap.ActionQueue[0].Params["x"] = 999

	got := a.Snapshot()
	if got.ActionQueue[0].Action != "work" {
		t.Errorf("source queue mutated via snapshot: %q", got.ActionQueue[0].Action)
	}
	if got.ActionQueue[0].Params["x"] != 1 {
		t.Errorf("source params mutated via snapshot: %v", got.ActionQueue[0].Params["x"])
	}
}

func TestReactiveDedupe(t *testing.T) {
	a := New()
	key := "H-01|zone_change|workshop"
	if _, ok := a.LastReactiveAt(key); ok {
		t.Error("LastReactiveAt should be absent initially")
	}
	now := time.Now()
	a.SetLastReactiveAt(key, now)
	got, ok := a.LastReactiveAt(key)
	if !ok {
		t.Error("LastReactiveAt should be present after Set")
	}
	if !got.Equal(now) {
		t.Errorf("LastReactiveAt = %v, want %v", got, now)
	}
}

func TestDedupeReactive(t *testing.T) {
	a := New()
	key := "H-01|zone_change|workshop"
	window := 60 * time.Second
	// First call: no prior record → proceeds, records now.
	t0 := time.Now()
	if !a.DedupeReactive(key, t0, window) {
		t.Fatal("first DedupeReactive should proceed (no prior record)")
	}
	// Within window: should skip and leave timestamp unchanged.
	t1 := t0.Add(30 * time.Second)
	if a.DedupeReactive(key, t1, window) {
		t.Fatal("second DedupeReactive within window should skip")
	}
	got, _ := a.LastReactiveAt(key)
	if !got.Equal(t0) {
		t.Errorf("timestamp mutated during skip: got %v, want %v", got, t0)
	}
	// Outside window: should proceed and update timestamp.
	t2 := t0.Add(61 * time.Second)
	if !a.DedupeReactive(key, t2, window) {
		t.Fatal("third DedupeReactive outside window should proceed")
	}
	got, _ = a.LastReactiveAt(key)
	if !got.Equal(t2) {
		t.Errorf("timestamp not updated: got %v, want %v", got, t2)
	}
	// Different key: independent dedupe.
	if !a.DedupeReactive("H-01|periodic|", t0, window) {
		t.Fatal("DedupeReactive with new key should proceed")
	}
}

// TestConcurrentAccess verifies the AgentState mutex protects against
// data races under concurrent reads/writes. Run with -race.
func TestConcurrentAccess(t *testing.T) {
	a := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			a.RefillQueue([]PlannedAction{{Action: "w"}}, "07:00-11:00")
		}(i)
		go func(i int) {
			defer wg.Done()
			a.PopAction()
		}(i)
		go func() {
			defer wg.Done()
			a.Snapshot()
		}()
	}
	wg.Wait()
}
