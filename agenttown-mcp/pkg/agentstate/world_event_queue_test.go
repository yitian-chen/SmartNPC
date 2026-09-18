package agentstate

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// worldEvent builds a minimal non-force world event for queue tests.
func worldEvent(id, eventType string) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:    id,
		Category:   protocol.CategoryWorld,
		EventType:  eventType,
		Force:      false,
		Severity:   3,
		GameTime:   "D12 10:47:03",
		OccurredAt: 1719456402000,
		Data:       json.RawMessage(`{"description":"测试事件"}`),
	}
}

// TestEnqueueWorldEvent_FIFOAndDrainAll verifies arrival-order FIFO and the
// §4.4 drain semantics: ALL events come out at once and the queue empties.
func TestEnqueueWorldEvent_FIFOAndDrainAll(t *testing.T) {
	s := New()
	for _, id := range []string{"evt_1", "evt_2", "evt_3"} {
		if r := s.EnqueueWorldEvent(worldEvent(id, "malfunction")); r != EnqueueOK {
			t.Fatalf("enqueue %s: got %v, want EnqueueOK", id, r)
		}
	}
	if got := s.WorldEventQueueLen(); got != 3 {
		t.Fatalf("queue len = %d, want 3", got)
	}
	events := s.DrainWorldEvents()
	if len(events) != 3 {
		t.Fatalf("drain returned %d events, want 3", len(events))
	}
	for i, want := range []string{"evt_1", "evt_2", "evt_3"} {
		if events[i].EventID != want {
			t.Fatalf("drain[%d].EventID = %q, want %q (arrival order)", i, events[i].EventID, want)
		}
	}
	// Drain empties the queue: a second drain returns nothing.
	if again := s.DrainWorldEvents(); len(again) != 0 {
		t.Fatalf("second drain should be empty, got %d", len(again))
	}
	if got := s.WorldEventQueueLen(); got != 0 {
		t.Fatalf("queue len after drain = %d, want 0", got)
	}
}

// TestEnqueueWorldEvent_DuplicateDropped verifies the event_id dedup that
// guards against reconnect seq-replay redelivery — including AFTER the
// original was already drained and consumed.
func TestEnqueueWorldEvent_DuplicateDropped(t *testing.T) {
	s := New()
	if r := s.EnqueueWorldEvent(worldEvent("evt_dup", "malfunction")); r != EnqueueOK {
		t.Fatalf("first enqueue: got %v, want EnqueueOK", r)
	}
	if r := s.EnqueueWorldEvent(worldEvent("evt_dup", "malfunction")); r != EnqueueDuplicate {
		t.Fatalf("replayed enqueue: got %v, want EnqueueDuplicate", r)
	}
	if got := s.WorldEventQueueLen(); got != 1 {
		t.Fatalf("queue len after duplicate = %d, want 1", got)
	}
	// Consume it, then replay the same id: the seen-set (preserved across
	// the drain) must still refuse re-enqueue.
	s.DrainWorldEvents()
	if r := s.EnqueueWorldEvent(worldEvent("evt_dup", "malfunction")); r != EnqueueDuplicate {
		t.Fatalf("replay after drain: got %v, want EnqueueDuplicate", r)
	}
	if got := s.WorldEventQueueLen(); got != 0 {
		t.Fatalf("queue len after post-drain replay = %d, want 0", got)
	}
}

// TestEnqueueWorldEvent_OverflowEvictsOldest verifies the bounded-queue
// policy: at capacity, enqueueing a new event drops the OLDEST pending
// event and reports EnqueueEvictedOldest.
func TestEnqueueWorldEvent_OverflowEvictsOldest(t *testing.T) {
	s := New()
	ids := make([]string, worldEventQueueCap)
	for i := range ids {
		ids[i] = fmt.Sprintf("evt_%03d", i)
		if r := s.EnqueueWorldEvent(worldEvent(ids[i], "malfunction")); r != EnqueueOK {
			t.Fatalf("fill[%d]: got %v, want EnqueueOK", i, r)
		}
	}
	if r := s.EnqueueWorldEvent(worldEvent("evt_newest", "malfunction")); r != EnqueueEvictedOldest {
		t.Fatalf("overflow enqueue: got %v, want EnqueueEvictedOldest", r)
	}
	if got := s.WorldEventQueueLen(); got != worldEventQueueCap {
		t.Fatalf("queue len after overflow = %d, want %d", got, worldEventQueueCap)
	}
	events := s.DrainWorldEvents()
	if events[0].EventID != ids[1] {
		t.Fatalf("head after eviction = %q, want %q (oldest %q evicted)", events[0].EventID, ids[1], ids[0])
	}
	if events[len(events)-1].EventID != "evt_newest" {
		t.Fatalf("newest event should be at tail, got %q", events[len(events)-1].EventID)
	}
}

// TestMarkWorldEventSeen_BoundedWindow verifies the dedup set is a bounded
// recent-id window: after enough distinct ids, the oldest is forgotten and
// becomes fresh again (prevents unbounded growth over long simulations).
func TestMarkWorldEventSeen_BoundedWindow(t *testing.T) {
	s := New()
	if !s.MarkWorldEventSeen("evt_old") {
		t.Fatalf("first sight should be fresh")
	}
	if s.MarkWorldEventSeen("evt_old") {
		t.Fatalf("second sight should be duplicate")
	}
	// Displace evt_old with enough distinct ids to roll the window past it.
	for i := 0; i < worldEventSeenCap; i++ {
		s.MarkWorldEventSeen(fmt.Sprintf("evt_pad_%03d", i))
	}
	if !s.MarkWorldEventSeen("evt_old") {
		t.Fatalf("evt_old should be fresh again after the seen window rolled past it")
	}
}

// TestEnqueueWorldEvent_EmptyEventIDNoDedup verifies degenerate events
// without an id never collapse into one: no dedup key means no dropping.
func TestEnqueueWorldEvent_EmptyEventIDNoDedup(t *testing.T) {
	s := New()
	for i := 0; i < 3; i++ {
		if r := s.EnqueueWorldEvent(worldEvent("", "malfunction")); r != EnqueueOK {
			t.Fatalf("empty-id enqueue[%d]: got %v, want EnqueueOK", i, r)
		}
	}
	if got := s.WorldEventQueueLen(); got != 3 {
		t.Fatalf("queue len = %d, want 3 (id-less events must not be deduped)", got)
	}
}

// TestStop_ClearsWorldEventQueueAndSeen verifies the only reset point:
// Stop empties the queue AND the seen-set, so a post-reconnect replay of
// an event that was enqueued-but-never-drained can be re-delivered.
func TestStop_ClearsWorldEventQueueAndSeen(t *testing.T) {
	s := New()
	s.EnqueueWorldEvent(worldEvent("evt_gone", "malfunction"))
	s.Stop()
	if got := s.WorldEventQueueLen(); got != 0 {
		t.Fatalf("queue len after Stop = %d, want 0", got)
	}
	if r := s.EnqueueWorldEvent(worldEvent("evt_gone", "malfunction")); r != EnqueueOK {
		t.Fatalf("re-enqueue after Stop: got %v, want EnqueueOK (seen-set reset)", r)
	}
}

// TestClearForSlotSwitch_KeepsWorldEventQueue pins the deliberate design
// decision as an executable spec: slot switch / replan must NOT clear the
// world-event queue. Both hooks run before the interrupted action's
// completion — the very safe point (§4.4) where the queued events must be
// delivered to the tactical layer. Clearing there would lose them forever
// (the seen-set blocks replay).
func TestClearForSlotSwitch_KeepsWorldEventQueue(t *testing.T) {
	s := New()
	s.EnqueueWorldEvent(worldEvent("evt_keep", "malfunction"))
	s.ClearForSlotSwitch()
	if got := s.WorldEventQueueLen(); got != 1 {
		t.Fatalf("queue len after ClearForSlotSwitch = %d, want 1 (events are slot-agnostic)", got)
	}
	// ClearForReplan delegates to ClearForSlotSwitch — same guarantee.
	s.ClearForReplan()
	if got := s.WorldEventQueueLen(); got != 1 {
		t.Fatalf("queue len after ClearForReplan = %d, want 1", got)
	}
	events := s.DrainWorldEvents()
	if len(events) != 1 || events[0].EventID != "evt_keep" {
		t.Fatalf("event lost across slot switch / replan: %+v", events)
	}
}

// TestEnqueueWorldEvent_ConcurrentDuplicate hammers the dedup from many
// goroutines with the same event_id: exactly one enqueue may win.
func TestEnqueueWorldEvent_ConcurrentDuplicate(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.EnqueueWorldEvent(worldEvent("evt_race", "malfunction"))
		}()
	}
	wg.Wait()
	if got := s.WorldEventQueueLen(); got != 1 {
		t.Fatalf("concurrent same-id enqueues must yield exactly 1 queued event, got %d", got)
	}
}

// ─── P4-10（§6.1 状态栏补全）测试 ──────────────────────────────

// TestRecordActionInterrupted_CapturesUnfinishedSlot verifies the capture:
// unfinished task + last end (interrupted + specific why). No-op when idle.
func TestRecordActionInterrupted_CapturesUnfinishedSlot(t *testing.T) {
	s := New()
	s.RecordActionStarted("act-1", "InteractSmartObject", map[string]any{"semantic_group": "workbench"}, SourceTactical, "")
	s.RecordActionInterrupted("InteractSmartObject(workbench)，已执行约 47 分钟", "被玩家攻击强制打断")
	snap := s.Snapshot()
	if snap.UnfinishedTask != "InteractSmartObject(workbench)，已执行约 47 分钟" {
		t.Fatalf("unfinished = %q", snap.UnfinishedTask)
	}
	if snap.LastEndResult != "interrupted" || snap.LastEndWhy != "被玩家攻击强制打断" {
		t.Fatalf("lastEnd = %q/%q", snap.LastEndResult, snap.LastEndWhy)
	}

	// 无在途时 no-op（真实时序：stop 点捕捉后立即 ClearInFlightKeepQueue，
	// 更晚的打断点看到的是空在途，不得覆盖更具体的记录）。
	s.ClearInFlightKeepQueue()
	s.RecordActionInterrupted("后来的笼统描述", "另一个笼统原因")
	snap = s.Snapshot()
	if snap.LastEndWhy != "被玩家攻击强制打断" {
		t.Fatalf("idle capture must not overwrite the more specific one, got %q", snap.LastEndWhy)
	}
}

// TestRecordActionCompletion_LastEndBookkeeping verifies the ledger rules:
// a stop-time-prefaced interrupted completion keeps the specific why; other
// results overwrite; a fresh interrupted (no stop capture) records its own.
func TestRecordActionCompletion_LastEndBookkeeping(t *testing.T) {
	s := New()
	s.RecordActionStarted("act-1", "WorkShift", nil, SourceTactical, "")
	s.RecordActionInterrupted("WorkShift", "被紧急事件强制打断")
	// 迟到的 interrupted completion：只确认，不覆盖 why。
	s.RecordActionCompletion("act-1", "interrupted", "")
	snap := s.Snapshot()
	if snap.LastEndResult != "interrupted" || snap.LastEndWhy != "被紧急事件强制打断" {
		t.Fatalf("prefaced completion must keep the specific why, got %q/%q", snap.LastEndResult, snap.LastEndWhy)
	}

	// 正常完成：覆盖。
	s.RecordActionStarted("act-2", "WorkShift", nil, SourceTactical, "")
	s.RecordActionCompletion("act-2", "success", "")
	snap = s.Snapshot()
	if snap.LastEndResult != "success" || snap.LastEndWhy != "" {
		t.Fatalf("success must overwrite, got %q/%q", snap.LastEndResult, snap.LastEndWhy)
	}

	// 未经 stop 点的 interrupted（UE 自身原因）：按实际记录。
	s.RecordActionStarted("act-3", "MoveTo", nil, SourceTactical, "")
	s.RecordActionCompletion("act-3", "failed", "unreachable")
	snap = s.Snapshot()
	if snap.LastEndResult != "failed" || snap.LastEndWhy != "unreachable" {
		t.Fatalf("failed must record its reason, got %q/%q", snap.LastEndResult, snap.LastEndWhy)
	}
}

// TestUnfinishedTask_ClearedOnNextAction verifies the consumption semantics:
// a new action start clears the slot (the decision that produced this
// action already consumed the fact), and Stop clears everything.
func TestUnfinishedTask_ClearedOnNextAction(t *testing.T) {
	s := New()
	s.RecordActionStarted("act-1", "WorkShift", nil, SourceTactical, "")
	s.RecordActionInterrupted("WorkShift", "时段切换（计划内打断）")
	if s.Snapshot().UnfinishedTask == "" {
		t.Fatalf("unfinished should be captured")
	}
	s.RecordActionStarted("act-2", "Speak", nil, SourceTactical, "")
	if got := s.Snapshot().UnfinishedTask; got != "" {
		t.Fatalf("next action start must clear the slot, got %q", got)
	}
	// lastEnd 保留（跨动作可见：上次为什么结束）。
	if s.Snapshot().LastEndResult != "interrupted" {
		t.Fatalf("lastEnd must survive action start, got %q", s.Snapshot().LastEndResult)
	}

	s.RecordActionInterrupted("x", "y")
	s.Stop()
	snap := s.Snapshot()
	if snap.UnfinishedTask != "" || snap.LastEndResult != "" {
		t.Fatalf("Stop must clear P4-10 fields, got %q/%q", snap.UnfinishedTask, snap.LastEndResult)
	}
}

// TestCurrentActionStartGame records the game-time anchor for elapsed
// computation when perception exists, and stays 0 without perception.
func TestCurrentActionStartGame(t *testing.T) {
	s := New()
	// 无感知：起点为 0（已执行时长省略）。
	s.RecordActionStarted("act-1", "WorkShift", nil, SourceTactical, "")
	if got := s.Snapshot().CurrentActionStartGame; got != 0 {
		t.Fatalf("no perception → start game 0, got %v", got)
	}
	// 有感知：起点 = 权威游戏秒。
	perc, _ := json.Marshal(protocol.PerceptionPayload{
		Environment: protocol.Environment{GameTimeSec: 989223, TimeOfDaySec: 38823, DayCount: 11, TimeScale: 90},
	})
	if _, err := s.SetPerception(perc); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	s.RecordActionStarted("act-2", "WorkShift", nil, SourceTactical, "")
	if got := s.Snapshot().CurrentActionStartGame; got != 989223 {
		t.Fatalf("start game = %v, want 989223", got)
	}
}
