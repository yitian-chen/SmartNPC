package agentstate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

// AgentState holds per-agent business state: persistent fields (survive
// restart, will be backed by MySQL in stage 3) and transient fields
// (in-process only). All fields are private; access goes through
// semantics-named methods. Coordination fields (wake channel, cancel,
// pending timers, replan-in-progress flag, debug override) live in the
// main package's agentContext, which holds a *AgentState pointer.
type AgentState struct {
	mu sync.Mutex

	// Identity + persistence handle. Set once via SetIdentity by the main
	// package's registerAgent, before LoadPersistent and before any
	// write-through can fire. When store == nil (tests / in-memory mode),
	// persistence calls are skipped — the struct behaves as pure in-memory.
	agentID string
	store   storage.Store

	// Persistent fields (cross-session, backed by MySQL when store != nil)
	dailyPlan        string
	currentDay       int // -1 = unplanned
	currentPlanIndex int
	currentSlot      string // "HH:MM-HH:MM" or "__debug__"-prefixed

	// Transient fields (in-process, not persisted)
	online              bool
	latestPhysical      *protocol.PhysicalState
	latestPerception    json.RawMessage
	currentTask         *protocol.CurrentTaskProgress
	currentActionID     string
	currentActionCmd    string
	currentActionParams map[string]any
	currentActionStart  time.Time
	currentActionSrc    ActionSource
	// Queue state (约定21): when an auto_queue=true action targets an
	// occupied Smart Object, UE queues the agent and notifies via
	// action_queued. These fields track the latest queue status so the
	// reactive layer prompt can mention "正在排队等待…". Cleared on
	// action completion, slot switch, replan, and agent offline.
	queuedActionID      string
	queuedGroup         string
	queuedPosition      *int
	queuedEstimatedWait *float64
	queuedAt            time.Time
	actionQueue         []PlannedAction
	redecomposeCount    int
	pendingStopActionID string
	selfStopInProgress  string
	// clearedAction stashes the most recently cleared in-flight action
	// (cmd/params/start/src) when ClearForSlotSwitch / ClearForReplan
	// drops in-flight tracking before the delayed stop_action's
	// action_completed(interrupted) arrives. RecordActionCompletion
	// consumes the stash on actionID match and restores WasInFlight=true,
	// so callers (recordActionHistory) still record the full action row
	// for long-composite actions interrupted by slot switch or replan
	// fallback. One-shot: matched → cleared. Overwritten by next clear.
	clearedAction *clearedActionInfo
	prevZone      string
	prevObjectIDs []string
	lastReactiveAt      map[string]time.Time
	perceptionCount     int
	replanHint          string
	lastReplanAt        time.Time
	lastReplanGameTime  string
}

// clearedActionInfo is the stash dropped by ClearForSlotSwitch/ClearForReplan
// and consumed by RecordActionCompletion when a delayed stop completion arrives.
type clearedActionInfo struct {
	ActionID string
	Cmd      string
	Params   map[string]any
	Start    time.Time
	Src      ActionSource
}

// New creates an AgentState with default zero values. currentDay starts
// at -1 (unplanned); the worker sets it after the first perception or
// after generateDailyPlan on startup.
func New() *AgentState {
	return &AgentState{
		currentDay:     -1,
		lastReactiveAt: make(map[string]time.Time),
	}
}

// SetIdentity binds the agent's identity and persistence store. Must be
// called once after New, before LoadPersistent and before any setter that
// triggers write-through. When store == nil, all persistence is skipped
// (in-memory mode for tests and quick-smoke runs without MySQL).
//
// Passing a non-nil store over a previously-nil one is allowed (used by
// registerAgent on first registration); the store is then used by all
// subsequent setters. Re-binding a non-nil store to another non-nil store
// is not supported — each AgentState is scoped to one agent lifecycle.
func (a *AgentState) SetIdentity(agentID string, store storage.Store) {
	a.mu.Lock()
	a.agentID = agentID
	a.store = store
	a.mu.Unlock()
}

// AgentID returns the bound agent identity (empty before SetIdentity).
// Callers use this to scope store calls (SaveMemory, SaveActionRecord, etc.)
// without having to thread the agent ID separately.
func (a *AgentState) AgentID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentID
}

// Store returns the bound persistence store (nil in in-memory mode).
// Callers use this to access memory/action_history methods directly,
// checking for nil before proceeding.
func (a *AgentState) Store() storage.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.store
}

// LoadPersistent hydrates the four persistent fields from the store.
// Called by registerAgent after SetIdentity, before spawning the worker.
//
//   - ErrNotFound (first run / cold start): keeps defaults (currentDay=-1),
//     so the worker generates a fresh daily plan as if no DB existed.
//   - Other errors: logs a warning and keeps defaults — the agent
//     degrades to re-planning rather than failing to start.
//   - Success: overwrites the four fields; the worker sees currentDay
//     matches today and skips generateDailyPlan (plan survives restart).
func (a *AgentState) LoadPersistent(ctx context.Context) error {
	if a.store == nil {
		return nil
	}
	snap, err := a.store.LoadScheduleState(ctx, a.agentID)
	if err == storage.ErrNotFound {
		return nil
	}
	if err != nil {
		slog.Default().Warn("[agentstate] load persistent state failed, degrading to cold start",
			"agent_id", a.agentID, "err", err)
		return err
	}
	a.mu.Lock()
	a.dailyPlan = snap.DailyPlan
	a.currentDay = snap.CurrentDay
	a.currentPlanIndex = snap.CurrentPlanIndex
	a.currentSlot = snap.CurrentSlot
	a.mu.Unlock()
	return nil
}

// SnapshotPersistent returns a copy of the four persistent fields. Used by
// tests and diagnostics to assert write-through state without touching the
// store.
func (a *AgentState) SnapshotPersistent() storage.ScheduleState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotPersistentLocked()
}

// snapshotPersistentLocked reads the four persistent fields; caller holds a.mu.
func (a *AgentState) snapshotPersistentLocked() storage.ScheduleState {
	return storage.ScheduleState{
		DailyPlan:        a.dailyPlan,
		CurrentDay:       a.currentDay,
		CurrentPlanIndex: a.currentPlanIndex,
		CurrentSlot:      a.currentSlot,
	}
}

// persistSchedule write-throughs the snapshot to the store. Caller must NOT
// hold a.mu (DB I/O outside the lock avoids blocking other goroutines).
// Errors are logged as warnings — the in-memory state is already correct,
// and the DB will catch up on the next successful write. This matches the
// "log and continue" pattern: persistence is best-effort, never blocks the
// decision pipeline.
func (a *AgentState) persistSchedule(snap storage.ScheduleState) {
	if a.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.store.SaveScheduleState(ctx, a.agentID, snap); err != nil {
		slog.Default().Warn("[agentstate] persist schedule state failed",
			"agent_id", a.agentID, "err", err)
	}
}

// PerceptionUpdate carries the before/after deltas computed by SetPerception,
// so the caller (agentContext.observePerception) can run trigger detection
// without holding the AgentState lock.
type PerceptionUpdate struct {
	CurZone         string
	CurObjectIDs    []string
	PrevZone        string
	PrevObjectIDs   []string
	PrevPhysical    *protocol.PhysicalState
	PerceptionCount int
}

// SetPerception stores the latest perception payload and advances the
// perception counter. It returns the zone/object/physical deltas so the
// caller can run reactive trigger detection. The caller is responsible
// for the stopped-check (coordination field, lives in agentContext).
func (a *AgentState) SetPerception(payload json.RawMessage) (PerceptionUpdate, error) {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return PerceptionUpdate{}, fmt.Errorf("parse perception: %w", err)
	}
	curZone := ""
	if p.Location.CurrentZone != nil {
		curZone = *p.Location.CurrentZone
	}
	curObjectIDs := extractObjectIDs(p)

	a.mu.Lock()
	prevZone := a.prevZone
	prevObjectIDs := a.prevObjectIDs
	prevPhysical := a.latestPhysical
	a.latestPerception = cloneRawMessage(payload)
	a.prevZone = curZone
	a.prevObjectIDs = curObjectIDs
	a.perceptionCount++
	count := a.perceptionCount
	a.mu.Unlock()

	return PerceptionUpdate{
		CurZone:         curZone,
		CurObjectIDs:    curObjectIDs,
		PrevZone:        prevZone,
		PrevObjectIDs:   prevObjectIDs,
		PrevPhysical:    prevPhysical,
		PerceptionCount: count,
	}, nil
}

// SetPhysicalState stores the authoritative physical/task state and
// returns the previous physical state for reactive trigger detection.
func (a *AgentState) SetPhysicalState(physical *protocol.PhysicalState, task *protocol.CurrentTaskProgress) (prev *protocol.PhysicalState) {
	a.mu.Lock()
	prev = a.latestPhysical
	a.latestPhysical = physical
	a.currentTask = task
	a.mu.Unlock()
	return prev
}

// SetOnline sets the agent online flag.
func (a *AgentState) SetOnline(v bool) {
	a.mu.Lock()
	a.online = v
	a.mu.Unlock()
}

// RecordActionStarted records a newly-dispatched in-flight action.
func (a *AgentState) RecordActionStarted(actionID, cmd string, params map[string]any, src ActionSource) {
	a.mu.Lock()
	a.currentActionID = actionID
	a.currentActionSrc = src
	a.currentActionCmd = cmd
	a.currentActionParams = params
	a.currentActionStart = time.Now()
	a.mu.Unlock()
}

// CompletionResult carries the flags computed by RecordActionCompletion
// so the caller (agentContext.recordActionCompletion) can handle
// coordination fields (pendingActionTimeouts, completedBeforeArm) and
// reactive trigger decisions.
//
// Stage 4 adds Cmd/Params/Start: captured BEFORE clearing in-flight tracking,
// so the caller can record a full action_history row at completion time.
type CompletionResult struct {
	WasInFlight    bool
	WasPendingStop bool
	WasSelfStop    bool
	Src            ActionSource
	Cmd            string
	Params         map[string]any
	Start          time.Time
}

// RecordActionCompletion clears in-flight tracking for the given action
// and resolves pendingStop/selfStop markers. Returns flags describing
// what was cleared so the caller can handle coordination timers
// (pendingActionTimeouts, completedBeforeArm) and reactive triggers.
func (a *AgentState) RecordActionCompletion(actionID string) CompletionResult {
	a.mu.Lock()
	if a.currentTask != nil && a.currentTask.ActionID == actionID {
		a.currentTask = nil
	}
	wasInFlight := a.currentActionID == actionID
	// Stage 4: capture in-flight fields BEFORE clearing, for action_history.
	var cmd string
	var params map[string]any
	var start time.Time
	var src ActionSource
	if wasInFlight {
		cmd = a.currentActionCmd
		params = a.currentActionParams
		start = a.currentActionStart
		src = a.currentActionSrc
		a.currentActionID = ""
		a.currentActionCmd = ""
		a.currentActionParams = nil
		a.currentActionStart = time.Time{}
	} else if a.clearedAction != nil && a.clearedAction.ActionID == actionID {
		// Delayed stop completion for a long-composite action whose
		// in-flight tracking was already dropped by ClearForSlotSwitch /
		// ClearForReplan (slot switch or replan fallback). Restore the
		// stashed fields so the caller records a full action_history
		// row. One-shot consume.
		cmd = a.clearedAction.Cmd
		params = a.clearedAction.Params
		start = a.clearedAction.Start
		src = a.clearedAction.Src
		wasInFlight = true
		a.clearedAction = nil
	}
	wasPendingStop := a.pendingStopActionID == actionID
	if wasPendingStop {
		a.pendingStopActionID = ""
	}
	wasSelfStop := a.selfStopInProgress == actionID
	if wasSelfStop {
		a.selfStopInProgress = ""
	}
	if wasInFlight {
		a.currentActionSrc = ""
	}
	// 约定21: action 完成（无论 success/failed/interrupted）都清排队状态。
	// timeout 路径下 action_queued{timeout} 已先行清理，这里兜底覆盖其他分支。
	a.clearQueueStatusLocked()
	a.mu.Unlock()

	return CompletionResult{
		WasInFlight:    wasInFlight,
		WasPendingStop: wasPendingStop,
		WasSelfStop:    wasSelfStop,
		Src:            src,
		Cmd:            cmd,
		Params:         params,
		Start:          start,
	}
}

// RecordQueueStatus updates the agent's queue tracking state based on an
// incoming action_queued message (约定21). Caller is the WS handler in
// main.go. Behavior by status:
//   - queued:   write queue fields (agent is now waiting for the object)
//   - advanced: clear queue fields (the action is now executing — queue
//     phase is over, normal action_started/completed lifecycle resumes)
//   - timeout:  clear queue fields (UE will follow with
//     action_completed{failed, reason=queue_timeout} which also clears)
//
// Unknown action_id (e.g. queue notification for an already-cancelled
// action) is tolerated: we still update fields on queued, since the UE
// side is the source of truth for queue membership.
func (a *AgentState) RecordQueueStatus(payload protocol.ActionQueuedPayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch payload.Status {
	case protocol.QueueStatusQueued:
		a.queuedActionID = payload.ActionID
		a.queuedGroup = payload.Group
		a.queuedPosition = payload.Position
		a.queuedEstimatedWait = payload.EstimatedWaitSec
		a.queuedAt = time.Now()
	case protocol.QueueStatusAdvanced, protocol.QueueStatusTimeout:
		a.clearQueueStatusLocked()
	}
}

// clearQueueStatusLocked resets all queue tracking fields. Caller must
// hold a.mu.
func (a *AgentState) clearQueueStatusLocked() {
	a.queuedActionID = ""
	a.queuedGroup = ""
	a.queuedPosition = nil
	a.queuedEstimatedWait = nil
	a.queuedAt = time.Time{}
}

// ClearInFlightAction clears in-flight tracking for the given actionID if it
// matches the current action. Used by recordActionStarted's TOCTOU recovery:
// when a completion arrives in the microsecond window between the
// completedBeforeArm check and RecordActionStarted setting currentActionID,
// the completion runs with wasInFlight=false (currentActionID was still empty)
// and never clears the field. This method clears the stale currentActionID
// so the worker's hasInFlightAction() gate doesn't block forever.
func (a *AgentState) ClearInFlightAction(actionID string) {
	a.mu.Lock()
	if a.currentActionID == actionID {
		a.currentActionID = ""
		a.currentActionCmd = ""
		a.currentActionParams = nil
		a.currentActionStart = time.Time{}
		a.currentActionSrc = ""
	}
	a.mu.Unlock()
}

// SetPendingStopActionID records a long-composite action ID that should be
// stopped after the next tactical refill (slot switch deferred-stop strategy).
func (a *AgentState) SetPendingStopActionID(id string) {
	a.mu.Lock()
	a.pendingStopActionID = id
	a.mu.Unlock()
}

// PendingStopActionID returns the current pending-stop action ID.
func (a *AgentState) PendingStopActionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingStopActionID
}

// SetSelfStopInProgress marks an action ID we actively stopped and are
// awaiting interrupted completion for (suppresses reactive trigger).
func (a *AgentState) SetSelfStopInProgress(id string) {
	a.mu.Lock()
	a.selfStopInProgress = id
	a.mu.Unlock()
}

// SelfStopInProgress returns the current self-stop-in-progress action ID.
func (a *AgentState) SelfStopInProgress() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selfStopInProgress
}

// SetCurrentActionSrc overrides the current action source without touching
// other in-flight fields. Used by tests that need to simulate "previous
// tactical action completed (currentActionID cleared) but source marker
// retained" to exercise the prepend-on-busy-rejection defensive path.
// Production code should use RecordActionStarted to set the source together
// with the action ID.
func (a *AgentState) SetCurrentActionSrc(src ActionSource) {
	a.mu.Lock()
	a.currentActionSrc = src
	a.mu.Unlock()
}

// ClearForReplan resets queue, in-flight tracking, slot marker, and
// redecompose counter — used by advanceSlotIfNeeded on slot expiry and
// by tacticalRefillForReplan before re-decomposition. Returns the
// in-flight action info (id, cmd) so the caller can issue a deferred
// stop for long-composite actions.
type InFlightInfo struct {
	ActionID  string
	ActionCmd string
	QueueLen  int
}

// ClearForSlotSwitch clears queue + in-flight + slot + redecompose counter.
// Returns in-flight info so the caller can decide whether to set
// pendingStopActionID for long-composite actions.
func (a *AgentState) ClearForSlotSwitch() InFlightInfo {
	a.mu.Lock()
	info := InFlightInfo{
		ActionID:  a.currentActionID,
		ActionCmd: a.currentActionCmd,
		QueueLen:  len(a.actionQueue),
	}
	// Stash the in-flight action so its delayed stop completion
	// (action_completed with reason=interrupted, arriving after the
	// stop_action issued by popAndSendQueueAction) can still be
	// recorded as a full action_history row. Without this stash,
	// RecordActionCompletion sees currentActionID="" and skips
	// recording — losing the long-composite action entirely.
	if a.currentActionID != "" {
		a.clearedAction = &clearedActionInfo{
			ActionID: a.currentActionID,
			Cmd:      a.currentActionCmd,
			Params:   a.currentActionParams,
			Start:    a.currentActionStart,
			Src:      a.currentActionSrc,
		}
	}
	a.actionQueue = nil
	a.currentActionID = ""
	a.currentActionCmd = ""
	a.currentActionParams = nil
	a.currentActionStart = time.Time{}
	a.currentActionSrc = ""
	a.clearQueueStatusLocked()
	a.currentSlot = ""
	a.redecomposeCount = 0
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
	return info
}

// ClearForReplan resets queue + in-flight + slot but preserves redecompose
// counter semantics (caller manages). Used by tacticalRefillForReplan.
func (a *AgentState) ClearForReplan() InFlightInfo {
	return a.ClearForSlotSwitch()
}

// Stop clears all transient state when an agent goes offline. Mirrors the
// original agentContext.stop business-field cleanup. Does not touch
// coordination fields (cancel, timers) — those are the caller's job.
func (a *AgentState) Stop() {
	a.mu.Lock()
	a.online = false
	a.latestPerception = nil
	a.currentActionID = ""
	a.currentActionSrc = ""
	a.currentActionCmd = ""
	a.currentActionParams = nil
	a.currentActionStart = time.Time{}
	a.actionQueue = nil
	a.clearQueueStatusLocked()
	a.currentSlot = ""
	a.redecomposeCount = 0
	a.clearedAction = nil // drop stash — offline agent has no pending completion
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// RefillQueue replaces the action queue with the given actions and records
// the slot they were decomposed for.
func (a *AgentState) RefillQueue(actions []PlannedAction, slot string) {
	a.mu.Lock()
	a.actionQueue = actions
	a.currentSlot = slot
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// PopAction removes and returns the first action in the queue (FIFO).
// Returns ok=false if the queue is empty.
func (a *AgentState) PopAction() (PlannedAction, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.actionQueue) == 0 {
		return PlannedAction{}, false
	}
	act := a.actionQueue[0]
	a.actionQueue = a.actionQueue[1:]
	return act, true
}

// PeekAction returns the first action without removing it.
func (a *AgentState) PeekAction() (PlannedAction, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.actionQueue) == 0 {
		return PlannedAction{}, false
	}
	return a.actionQueue[0], true
}

// PopActionIfIdle atomically checks that no action is in-flight and, if
// so, pops the first queued action. Returns ok=false if the queue is
// empty or an action is in-flight (UE busy). Also returns and clears
// the pendingStopActionID so the caller can issue a deferred stop.
func (a *AgentState) PopActionIfIdle() (action PlannedAction, pendingStop string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.actionQueue) == 0 {
		return PlannedAction{}, "", false
	}
	if a.currentActionID != "" {
		return PlannedAction{}, "", false
	}
	action = a.actionQueue[0]
	a.actionQueue = a.actionQueue[1:]
	pendingStop = a.pendingStopActionID
	a.pendingStopActionID = ""
	return action, pendingStop, true
}

// PrependAction pushes an action to the front of the queue (used when
// re-queueing an action that failed to dispatch due to UE busy).
func (a *AgentState) PrependAction(action PlannedAction) {
	a.mu.Lock()
	a.actionQueue = append([]PlannedAction{action}, a.actionQueue...)
	a.mu.Unlock()
}

// AppendQueueAction appends an action to the queue (used by tactical
// streaming callback).
func (a *AgentState) AppendQueueAction(action PlannedAction) {
	a.mu.Lock()
	a.actionQueue = append(a.actionQueue, action)
	a.mu.Unlock()
}

// ShouldDispatchFirst reports whether the just-appended action is the
// first in the queue and no action is in-flight (streaming fast-path).
// Caller should call PopActionIfIdle to dispatch.
func (a *AgentState) ShouldDispatchFirst() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionID == "" && len(a.actionQueue) == 1
}

// ReplaceQueue replaces the entire action queue (used by non-streaming
// tactical refill).
func (a *AgentState) ReplaceQueue(actions []PlannedAction) {
	a.mu.Lock()
	a.actionQueue = actions
	a.mu.Unlock()
}

// NeedFallbackDispatch reports whether there's a queued action ready to
// dispatch with no in-flight action (used after tactical refill completes).
func (a *AgentState) NeedFallbackDispatch() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionID == "" && len(a.actionQueue) > 0
}

// TacticalRefillPrep carries the snapshot data needed by the tactical
// layer to call the LLM, plus guard flags. Produced atomically by
// BeginTacticalRefill.
type TacticalRefillPrep struct {
	Goal            string
	Slot            string
	Index           int
	Zone            string
	Physical        *protocol.PhysicalState
	Hint            string
	IsRedecompose   bool
	ShouldSkip      bool // guard failed (in-flight, no goal, redecompose limit)
	AlreadyHasQueue bool // queue non-empty in same slot → skip redecompose
}

// BeginTacticalRefill atomically checks guards and prepares for a tactical
// refill LLM call. It receives the goal/slot/idx pre-computed by the caller
// (via selectCurrentGoal on the daily plan). If ShouldSkip is true the
// caller must abort the refill. On success, the action queue is cleared
// and the replanHint is consumed (returned in Hint for prompt injection).
func (a *AgentState) BeginTacticalRefill(goal, slot string, idx int, hasTacticalHc bool) TacticalRefillPrep {
	a.mu.Lock()
	defer a.mu.Unlock()
	prep := TacticalRefillPrep{
		Goal:     goal,
		Slot:     slot,
		Index:    idx,
		Zone:     a.latestZoneLocked(),
		Physical: clonePhysical(a.latestPhysical),
	}
	if !hasTacticalHc {
		prep.ShouldSkip = true
		return prep
	}
	if a.currentActionID != "" {
		prep.ShouldSkip = true
		return prep
	}
	if goal == "" {
		prep.ShouldSkip = true
		return prep
	}
	// 同时段重复分解守卫
	if slot == a.currentSlot {
		if len(a.actionQueue) > 0 {
			prep.AlreadyHasQueue = true
			prep.ShouldSkip = true
			return prep
		}
		if a.redecomposeCount >= 3 {
			prep.ShouldSkip = true
			return prep
		}
		// 注入"未安排长动作"hint
		if a.replanHint == "" {
			a.replanHint = "上次队列提前耗尽，未安排长动作收尾——本次请确保最后一个 action 是标记为 [复合] 的长复合动作（见上方可用工具列表），让 NPC 持续工作到下一时段"
		}
	}
	prep.IsRedecompose = slot == a.currentSlot
	prep.Hint = a.replanHint
	a.replanHint = ""
	a.actionQueue = nil
	return prep
}

// CommitTacticalRefill records the slot/index after a successful tactical
// refill LLM call. For redecompose (same slot), bumps the counter; for a
// new slot, resets the counter and updates slot/index.
func (a *AgentState) CommitTacticalRefill(slot string, idx int, isRedecompose bool) {
	a.mu.Lock()
	if isRedecompose {
		a.redecomposeCount++
		a.mu.Unlock()
		return
	}
	a.currentSlot = slot
	a.currentPlanIndex = idx
	a.redecomposeCount = 0
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// QueueSnapshot returns a copy of the current action queue (for logging).
func (a *AgentState) QueueSnapshot() []PlannedAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneQueue(a.actionQueue)
}

// RedecomposeCountSnapshot returns the current redecompose count.
func (a *AgentState) RedecomposeCountSnapshot() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.redecomposeCount
}

// ReplanPrep carries the snapshot data needed by the tactical layer to
// re-decompose for a reactive replan. Unlike BeginTacticalRefill, this
// does NOT check the in-flight guard (replan explicitly allows planning
// while an action is in-flight — the caller will stop it after success).
type ReplanPrep struct {
	Goal     string
	Slot     string
	Index    int
	Zone     string
	Physical *protocol.PhysicalState
}

// BeginReplan reads the snapshot for a reactive replan. Does not clear
// the queue or consume hint — the caller provides the hint. Returns
// ShouldSkip=true if tacticalHc is nil or no goal.
func (a *AgentState) BeginReplan(goal, slot string, idx int, hasTacticalHc bool) ReplanPrep {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ReplanPrep{
		Goal:     goal,
		Slot:     slot,
		Index:    idx,
		Zone:     a.latestZoneLocked(),
		Physical: clonePhysical(a.latestPhysical),
	}
}

// CommitReplan replaces the queue, resets counters, and updates slot on
// successful replan. Called after the LLM returns new actions.
func (a *AgentState) CommitReplan(actions []PlannedAction, slot string, idx int) {
	a.mu.Lock()
	a.actionQueue = actions
	a.redecomposeCount = 0
	a.currentSlot = slot
	a.currentPlanIndex = idx
	a.replanHint = ""
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// HasQueueNext reports whether the action queue has any pending actions.
func (a *AgentState) HasQueueNext() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actionQueue) > 0
}

// HasInFlightAction reports whether an action is currently in flight
// (dispatched, awaiting completion).
func (a *AgentState) HasInFlightAction() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionID != ""
}

// CurrentActionID returns the in-flight action ID (empty if none).
func (a *AgentState) CurrentActionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionID
}

// CurrentActionSrc returns the source of the in-flight action.
func (a *AgentState) CurrentActionSrc() ActionSource {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionSrc
}

// QueueLen returns the current action queue length.
func (a *AgentState) QueueLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actionQueue)
}

// SnapshotSchedule returns the daily plan text, current slot, and plan
// index — used by /debug/plan and prompt builders.
func (a *AgentState) SnapshotSchedule() (plan string, slot string, idx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dailyPlan, a.currentSlot, a.currentPlanIndex
}

// SetDailyPlan replaces the daily plan and records which day_count it
// was generated for.
func (a *AgentState) SetDailyPlan(plan string, day int) {
	a.mu.Lock()
	a.dailyPlan = plan
	a.currentDay = day
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// SetCurrentPlanIndex records which daily-plan item is currently executing.
func (a *AgentState) SetCurrentPlanIndex(idx int) {
	a.mu.Lock()
	a.currentPlanIndex = idx
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
}

// CurrentDay returns the day_count the current daily plan was generated for
// (-1 = unplanned). Read-only query used by tests and the worker loop.
func (a *AgentState) CurrentDay() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentDay
}

// IncrementRedecomposeCount bumps the per-slot re-decomposition counter.
func (a *AgentState) IncrementRedecomposeCount() {
	a.mu.Lock()
	a.redecomposeCount++
	a.mu.Unlock()
}

// ResetRedecomposeCount zeros the per-slot re-decomposition counter
// (called on slot switch).
func (a *AgentState) ResetRedecomposeCount() {
	a.mu.Lock()
	a.redecomposeCount = 0
	a.mu.Unlock()
}

// RedecomposeCount returns the current per-slot re-decomposition count.
func (a *AgentState) RedecomposeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.redecomposeCount
}

// SetReplanHint stores the reason string injected into the next tactical
// prompt (e.g. "zone changed to workshop" from a reactive replan).
func (a *AgentState) SetReplanHint(reason string) {
	a.mu.Lock()
	a.replanHint = reason
	a.mu.Unlock()
}

// SetReplanTimestamps records when a replan happened (wall-clock + game time),
// used for dedupe and logging.
func (a *AgentState) SetReplanTimestamps(at time.Time, gameTime string) {
	a.mu.Lock()
	a.lastReplanAt = at
	a.lastReplanGameTime = gameTime
	a.mu.Unlock()
}

// LastReactiveAt returns the last reactive trigger time for the given dedupe key.
func (a *AgentState) LastReactiveAt(key string) (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.lastReactiveAt[key]
	return t, ok
}

// SetLastReactiveAt records the last reactive trigger time for a dedupe key.
func (a *AgentState) SetLastReactiveAt(key string, t time.Time) {
	a.mu.Lock()
	a.lastReactiveAt[key] = t
	a.mu.Unlock()
}

// DedupeReactive atomically checks the dedupe window for a reactive trigger
// and records now if the trigger should proceed. Returns true when the key
// has not been seen within window (caller should proceed; now is recorded).
// Returns false when a recent trigger exists within window (caller should
// skip; timestamp is left unchanged). The check-and-set is atomic under
// AgentState.mu, preserving the original single-mutex dedupe semantics.
func (a *AgentState) DedupeReactive(key string, now time.Time, window time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	lastAt, exists := a.lastReactiveAt[key]
	if exists && now.Sub(lastAt) < window {
		return false
	}
	a.lastReactiveAt[key] = now
	return true
}

// DetectDayRollover checks for day_count increment (cross-day) and updates
// currentDay. Returns rollover=true when prev>=0 and day>prev (real cross-day,
// caller should re-run generateDailyPlan). First sync (prev<0) updates
// currentDay but returns rollover=false (worker already planned on startup).
func (a *AgentState) DetectDayRollover() (rollover bool, prevDay, newDay int) {
	a.mu.Lock()
	day := a.latestDayCountLocked()
	prev := a.currentDay
	if day <= prev {
		a.mu.Unlock()
		return false, prev, day
	}
	a.currentDay = day
	snap := a.snapshotPersistentLocked()
	a.mu.Unlock()
	a.persistSchedule(snap)
	if prev < 0 {
		return false, prev, day
	}
	return true, prev, day
}

// LatestTimeOfDay returns "HH:MM" extracted from the latest perception.
func (a *AgentState) LatestTimeOfDay() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestTimeOfDayLocked()
}

// LatestDayCount returns the day_count from the latest perception, or -1
// if no perception has arrived or parsing fails.
func (a *AgentState) LatestDayCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestDayCountLocked()
}

// LatestZone returns the current zone id from the latest perception.
func (a *AgentState) LatestZone() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestZoneLocked()
}

// Snapshot returns an exported read-only copy of all business fields.
// Slice and pointer fields are deep-copied so the caller can mutate the
// snapshot without affecting the source state.
func (a *AgentState) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Snapshot{
		Online:              a.online,
		LatestPhysical:      clonePhysical(a.latestPhysical),
		LatestPerception:    cloneRawMessage(a.latestPerception),
		CurrentTask:         cloneTask(a.currentTask),
		CurrentActionID:     a.currentActionID,
		CurrentActionCmd:    a.currentActionCmd,
		CurrentActionParams: cloneParams(a.currentActionParams),
		CurrentActionStart:  a.currentActionStart,
		CurrentActionSrc:    a.currentActionSrc,
		QueuedActionID:      a.queuedActionID,
		QueuedGroup:         a.queuedGroup,
		QueuedPosition:      cloneIntPtr(a.queuedPosition),
		QueuedEstimatedWait: cloneFloat64Ptr(a.queuedEstimatedWait),
		QueuedAt:            a.queuedAt,
		ActionQueue:         cloneQueue(a.actionQueue),
		DailyPlan:           a.dailyPlan,
		CurrentDay:          a.currentDay,
		CurrentPlanIndex:    a.currentPlanIndex,
		CurrentSlot:         a.currentSlot,
		RedecomposeCount:    a.redecomposeCount,
		PrevZone:            a.prevZone,
		PrevObjectIDs:       cloneStrings(a.prevObjectIDs),
		PerceptionCount:     a.perceptionCount,
		ReplanHint:          a.replanHint,
		LastReplanAt:        a.lastReplanAt,
		LastReplanGameTime:  a.lastReplanGameTime,
		PendingStopActionID: a.pendingStopActionID,
		SelfStopInProgress:  a.selfStopInProgress,
	}
}

// --- internal helpers (assume caller holds a.mu) ---

func (a *AgentState) latestTimeOfDayLocked() string {
	return extractTimeOfDay(a.latestPerception)
}

func (a *AgentState) latestDayCountLocked() int {
	if len(a.latestPerception) == 0 {
		return -1
	}
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(a.latestPerception, &p); err != nil {
		return -1
	}
	return p.Environment.DayCount
}

func (a *AgentState) latestZoneLocked() string {
	if len(a.latestPerception) == 0 {
		return ""
	}
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(a.latestPerception, &p); err != nil {
		return ""
	}
	if p.Location.CurrentZone != nil {
		return *p.Location.CurrentZone
	}
	return ""
}

// --- package helpers ---

func extractTimeOfDay(raw json.RawMessage) string {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return formatTodSec(p.Environment.TimeOfDaySec)
}

func formatTodSec(todSec float64) string {
	if todSec < 0 || todSec >= 86400 {
		return ""
	}
	totalSec := int(todSec)
	hh := totalSec / 3600
	mm := (totalSec % 3600) / 60
	return fmt.Sprintf("%02d:%02d", hh, mm)
}

func extractObjectIDs(p protocol.PerceptionPayload) []string {
	ids := make([]string, 0, len(p.NearbyObjects))
	for _, obj := range p.NearbyObjects {
		if obj.ID != "" {
			ids = append(ids, obj.ID)
		}
	}
	return ids
}

func cloneRawMessage(payload json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), payload...)
}

func clonePhysical(physical *protocol.PhysicalState) *protocol.PhysicalState {
	if physical == nil {
		return nil
	}
	cp := *physical
	return &cp
}

func cloneTask(task *protocol.CurrentTaskProgress) *protocol.CurrentTaskProgress {
	if task == nil {
		return nil
	}
	cp := *task
	return &cp
}

func cloneParams(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	cp := make(map[string]any, len(params))
	for k, v := range params {
		cp[k] = v
	}
	return cp
}

func cloneQueue(q []PlannedAction) []PlannedAction {
	if q == nil {
		return nil
	}
	cp := make([]PlannedAction, len(q))
	for i, act := range q {
		cp[i] = PlannedAction{
			Action: act.Action,
			Params: cloneParams(act.Params),
		}
	}
	return cp
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp
}

func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func cloneFloat64Ptr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}
