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
