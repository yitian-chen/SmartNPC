package agentstate

import "github.com/AgentTown/agenttown-mcp/contract/protocol"

// world_event queue (事件驱动设计 §4.4，docs/AgentTown_WorldEvent_Protocol.md).
//
// Per-agent bounded FIFO of non-force world events awaiting the next safe
// point (an action completion), where they are drained ALL AT ONCE and
// handed to the tactical layer together with the strategic plan and the
// real-time state (§5.1 三输入).
//
// Deliberately NOT cleared by ClearForSlotSwitch / ClearForReplan: events
// are slot-agnostic world facts（"小柯借工具"不会因为过了 12:00 就作废）,
// and both clear hooks run BEFORE the interrupted action's completion
// arrives — the very safe point where §4.4 mandates delivering the queued
// events. Clearing there would destroy them permanently: the seen-set
// keeps a replay from ever re-enqueueing. Only Stop (agent offline) resets
// the queue and the seen-set — after a reconnect, UE only replays messages
// the agent never received, so nothing received-and-consumed can come back.

const (
	// worldEventQueueCap bounds the pending-event queue. On overflow the
	// OLDEST event is dropped: events are decision inputs, not a ledger —
	// a stale event the tactical layer never consumed is worth less than
	// the fresh one displacing it.
	worldEventQueueCap = 64
	// worldEventSeenCap bounds the dedup set. Replayed events arrive
	// shortly after their original, so a small recent-id window suffices;
	// a FIFO evicts the oldest remembered id when the window is full.
	worldEventSeenCap = 256
)

// EnqueueWorldEventResult reports the outcome of EnqueueWorldEvent so the
// caller (runtime dispatch) can log precisely without re-inspecting state.
type EnqueueWorldEventResult int

const (
	// EnqueueOK: the event was appended to the queue.
	EnqueueOK EnqueueWorldEventResult = iota
	// EnqueueDuplicate: the event_id was already seen — the same event
	// redelivered by reconnect seq replay — and nothing was enqueued.
	EnqueueDuplicate
	// EnqueueEvictedOldest: the event was enqueued, but the queue was
	// already at capacity so the OLDEST pending event was dropped.
	EnqueueEvictedOldest
)

// markWorldEventSeenLocked records eventID as handled and reports whether
// it is fresh. An empty event_id is always reported fresh (no dedup
// possible) and never recorded — collapsing all id-less events into one
// would silently drop real events.
func (a *AgentState) markWorldEventSeenLocked(eventID string) bool {
	if eventID == "" {
		return true
	}
	if a.worldEventSeen == nil {
		a.worldEventSeen = make(map[string]struct{})
	}
	if _, dup := a.worldEventSeen[eventID]; dup {
		return false
	}
	a.worldEventSeen[eventID] = struct{}{}
	a.worldEventSeenFIFO = append(a.worldEventSeenFIFO, eventID)
	if len(a.worldEventSeenFIFO) > worldEventSeenCap {
		evict := a.worldEventSeenFIFO[0]
		a.worldEventSeenFIFO = a.worldEventSeenFIFO[1:]
		delete(a.worldEventSeen, evict)
	}
	return true
}

// MarkWorldEventSeen records eventID as handled and reports whether it is
// fresh; false means the id was already seen (reconnect seq-replay of the
// same event) and the caller should drop it. Consumers that handle an
// event without queueing it — the force path interrupts immediately
// (§4.2) — still call this so a later replay cannot re-trigger.
func (a *AgentState) MarkWorldEventSeen(eventID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.markWorldEventSeenLocked(eventID)
}

// EnqueueWorldEvent appends a non-force world event to the per-agent
// queue. Duplicate event_ids are dropped (EnqueueDuplicate). When the
// queue is at capacity the oldest pending event is evicted
// (EnqueueEvictedOldest).
func (a *AgentState) EnqueueWorldEvent(ev protocol.WorldEventPayload) EnqueueWorldEventResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.markWorldEventSeenLocked(ev.EventID) {
		return EnqueueDuplicate
	}
	evicted := false
	if len(a.worldEventQueue) >= worldEventQueueCap {
		a.worldEventQueue = a.worldEventQueue[1:]
		evicted = true
	}
	a.worldEventQueue = append(a.worldEventQueue, ev)
	if evicted {
		return EnqueueEvictedOldest
	}
	return EnqueueOK
}

// DrainWorldEvents atomically returns ALL queued events in arrival order
// and empties the queue (§4.4: 一次性 drain 全部交给战术层，不是 pop 一个).
// Returns an empty slice when nothing is pending. The dedup seen-set is
// preserved across the drain, so a replay of an already-consumed event
// cannot re-enqueue it.
func (a *AgentState) DrainWorldEvents() []protocol.WorldEventPayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := a.worldEventQueue
	a.worldEventQueue = nil
	return events
}

// WorldEventQueueLen reports how many events are pending (debug panels /
// observability).
func (a *AgentState) WorldEventQueueLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.worldEventQueue)
}

// RemoveQueuedWorldEvent removes one pending event by id (the router's
// interrupt branch pulls an event out of the queue because it is being
// handled NOW, not at the next safe point). Reports whether the event was
// found and removed. The dedup seen-set is untouched: a replay of this
// event must still be dropped (it was received and is being handled).
func (a *AgentState) RemoveQueuedWorldEvent(eventID string) bool {
	if eventID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, ev := range a.worldEventQueue {
		if ev.EventID == eventID {
			a.worldEventQueue = append(a.worldEventQueue[:i], a.worldEventQueue[i+1:]...)
			return true
		}
	}
	return false
}
