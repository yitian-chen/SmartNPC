package agentstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
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

// ─── Stage 3: write-through persistence tests ──────────────────────────
//
// fakeStore is a minimal Store recording Save calls. Defined here (not in
// pkg/storage) because it's test-only and agentstate-specific.

type fakeStore struct {
	mu        sync.Mutex
	saved     map[string]storage.ScheduleState
	saveCalls int
	loadErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: make(map[string]storage.ScheduleState)}
}

func (f *fakeStore) LoadScheduleState(_ context.Context, agentID string) (storage.ScheduleState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return storage.ScheduleState{}, f.loadErr
	}
	if st, ok := f.saved[agentID]; ok {
		return st, nil
	}
	return storage.ScheduleState{}, storage.ErrNotFound
}

func (f *fakeStore) SaveScheduleState(_ context.Context, agentID string, s storage.ScheduleState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	f.saved[agentID] = s
	return nil
}

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) snapshot(agentID string) (storage.ScheduleState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.saved[agentID]
	return st, ok
}

func (f *fakeStore) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saveCalls
}

// bindStore wires a fresh fakeStore to an AgentState for write-through tests.
func bindStore(t *testing.T, a *AgentState, agentID string) *fakeStore {
	t.Helper()
	fs := newFakeStore()
	a.SetIdentity(agentID, fs)
	return fs
}

// TestSetDailyPlan_Persists verifies SetDailyPlan triggers a write-through
// saving both the plan and the day.
func TestSetDailyPlan_Persists(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.SetDailyPlan("06:00-22:00 工作计划", 3)
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1", got)
	}
	snap, ok := fs.snapshot("H-01")
	if !ok {
		t.Fatal("no snapshot saved")
	}
	if snap.DailyPlan != "06:00-22:00 工作计划" || snap.CurrentDay != 3 {
		t.Errorf("snapshot: got %+v, want plan+day=3", snap)
	}
}

// TestSetCurrentPlanIndex_Persists verifies index changes are persisted.
func TestSetCurrentPlanIndex_Persists(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.SetCurrentPlanIndex(4)
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentPlanIndex != 4 {
		t.Errorf("CurrentPlanIndex: got %d, want 4", snap.CurrentPlanIndex)
	}
}

// TestCommitTacticalRefill_PersistsNewSlot verifies a new-slot commit
// (isRedecompose=false) writes slot+index.
func TestCommitTacticalRefill_PersistsNewSlot(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.CommitTacticalRefill("08:00-12:00", 1, false)
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentSlot != "08:00-12:00" || snap.CurrentPlanIndex != 1 {
		t.Errorf("snapshot: got %+v, want slot=08:00-12:00 idx=1", snap)
	}
}

// TestCommitTacticalRefill_RedecomposeDoesNotPersist verifies the
// redecompose branch (same slot, just bumping counter) does NOT trigger
// a save — redecomposeCount is transient.
func TestCommitTacticalRefill_RedecomposeDoesNotPersist(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.CommitTacticalRefill("", 0, true)
	if got := fs.saveCount(); got != 0 {
		t.Fatalf("redecompose save calls: got %d, want 0 (transient counter)", got)
	}
}

// TestCommitReplan_Persists verifies replan writes the new slot+index.
func TestCommitReplan_Persists(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.CommitReplan([]PlannedAction{{Action: "wait"}}, "13:00-17:00", 2)
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentSlot != "13:00-17:00" || snap.CurrentPlanIndex != 2 {
		t.Errorf("snapshot: got %+v, want slot=13:00-17:00 idx=2", snap)
	}
}

// TestClearForSlotSwitch_PersistsClearedSlot verifies clearing the slot
// persists the empty currentSlot so a restart doesn't resume a stale slot.
func TestClearForSlotSwitch_PersistsClearedSlot(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.RefillQueue([]PlannedAction{{Action: "work"}}, "08:00-12:00") // 1 save
	a.ClearForSlotSwitch()                                            // 2nd save
	if got := fs.saveCount(); got != 2 {
		t.Fatalf("save calls: got %d, want 2", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentSlot != "" {
		t.Errorf("after clear, CurrentSlot: got %q, want empty", snap.CurrentSlot)
	}
}

// TestStop_PersistsClearedSlot verifies Stop persists the cleared slot
// (agent going offline should not leave a stale slot in the DB).
func TestStop_PersistsClearedSlot(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.RefillQueue(nil, "08:00-12:00") // 1 save
	a.Stop()                          // 2nd save
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentSlot != "" {
		t.Errorf("after Stop, CurrentSlot: got %q, want empty", snap.CurrentSlot)
	}
}

// TestRefillQueue_Persists verifies RefillQueue writes the slot.
func TestRefillQueue_Persists(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.RefillQueue([]PlannedAction{{Action: "work_at_workbench"}}, "07:00-11:00")
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentSlot != "07:00-11:00" {
		t.Errorf("CurrentSlot: got %q, want 07:00-11:00", snap.CurrentSlot)
	}
}

// TestDetectDayRollover_Persists verifies day advancement triggers a save.
func TestDetectDayRollover_Persists(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	// Seed currentDay=1 via SetDailyPlan (1 save), then advance to day 2.
	a.SetDailyPlan("plan", 1) // save 1
	setPerceptionForTest(t, a, "06:00", 2)
	rollover, _, newDay := a.DetectDayRollover()
	if !rollover || newDay != 2 {
		t.Fatalf("rollover: got %v day=%d, want true day=2", rollover, newDay)
	}
	if got := fs.saveCount(); got != 2 {
		t.Fatalf("save calls: got %d, want 2 (SetDailyPlan + rollover)", got)
	}
	snap, _ := fs.snapshot("H-01")
	if snap.CurrentDay != 2 {
		t.Errorf("CurrentDay: got %d, want 2", snap.CurrentDay)
	}
}

// TestDetectDayRollover_NoSaveWhenUnchanged verifies no save fires when
// the day hasn't advanced (idempotent guard).
func TestDetectDayRollover_NoSaveWhenUnchanged(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	a.SetDailyPlan("plan", 3) // save 1
	setPerceptionForTest(t, a, "10:00", 3) // same day
	a.DetectDayRollover()                  // no change → no save
	if got := fs.saveCount(); got != 1 {
		t.Fatalf("save calls: got %d, want 1 (no save on unchanged day)", got)
	}
}

// TestLoadPersistent_Hydrates verifies LoadPersistent restores the four
// fields from the store.
func TestLoadPersistent_Hydrates(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	// Seed the store directly.
	fs.saved["H-01"] = storage.ScheduleState{
		DailyPlan:        "cached plan",
		CurrentDay:       5,
		CurrentPlanIndex: 3,
		CurrentSlot:      "14:00-18:00",
	}
	if err := a.LoadPersistent(context.Background()); err != nil {
		t.Fatalf("LoadPersistent: %v", err)
	}
	snap := a.SnapshotPersistent()
	if snap.DailyPlan != "cached plan" || snap.CurrentDay != 5 ||
		snap.CurrentPlanIndex != 3 || snap.CurrentSlot != "14:00-18:00" {
		t.Errorf("after load: got %+v, want seeded values", snap)
	}
}

// TestLoadPersistent_NotFoundKeepsDefaults verifies ErrNotFound is treated
// as cold-start (defaults preserved, no error returned).
func TestLoadPersistent_NotFoundKeepsDefaults(t *testing.T) {
	a := New()
	bindStore(t, a, "H-01") // empty store → Load returns ErrNotFound
	if err := a.LoadPersistent(context.Background()); err != nil {
		t.Fatalf("LoadPersistent on empty: got err=%v, want nil (cold start)", err)
	}
	snap := a.SnapshotPersistent()
	if snap.CurrentDay != -1 {
		t.Errorf("CurrentDay: got %d, want -1 (default unplanned)", snap.CurrentDay)
	}
	if snap.DailyPlan != "" {
		t.Errorf("DailyPlan: got %q, want empty", snap.DailyPlan)
	}
}

// TestLoadPersistent_StoreErrorPropagates verifies non-NotFound errors
// are returned so the caller can log them.
func TestLoadPersistent_StoreErrorPropagates(t *testing.T) {
	a := New()
	fs := bindStore(t, a, "H-01")
	fs.loadErr = errors.New("connection refused")
	if err := a.LoadPersistent(context.Background()); err == nil {
		t.Fatal("LoadPersistent: got nil err, want connection error")
	}
}

// TestWriteThrough_NilStoreNoOp verifies that when store is nil (the
// default — no SetIdentity call), setters don't panic and in-memory state
// still updates. This is the in-memory mode.
func TestWriteThrough_NilStoreNoOp(t *testing.T) {
	a := New() // no SetIdentity → store is nil
	a.SetDailyPlan("plan", 1)
	a.SetCurrentPlanIndex(2)
	a.RefillQueue(nil, "08:00-12:00")
	snap := a.SnapshotPersistent()
	if snap.DailyPlan != "plan" || snap.CurrentDay != 1 ||
		snap.CurrentPlanIndex != 2 || snap.CurrentSlot != "08:00-12:00" {
		t.Errorf("in-memory state: got %+v, want all fields set", snap)
	}
}
