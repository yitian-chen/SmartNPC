package agentstate

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// AgentState holds per-agent business state: persistent fields (survive
// restart, will be backed by MySQL in stage 3) and transient fields
// (in-process only). All fields are private; access goes through
// semantics-named methods. Coordination fields (wake channel, cancel,
// pending timers, replan-in-progress flag, debug override) live in the
// main package's agentContext, which holds a *AgentState pointer.
type AgentState struct {
	mu sync.Mutex

	// Persistent fields (cross-session, backed by MySQL in stage 3)
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
	actionQueue         []PlannedAction
	redecomposeCount    int
	pendingStopActionID string
	selfStopInProgress  string
	prevZone            string
	prevObjectIDs       []string
	lastReactiveAt      map[string]time.Time
	perceptionCount     int
	replanHint          string
	lastReplanAt        time.Time
	lastReplanGameTime  string
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
type CompletionResult struct {
	WasInFlight    bool
	WasPendingStop bool
	WasSelfStop    bool
	Src            ActionSource
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
	if wasInFlight {
		a.currentActionID = ""
		a.currentActionCmd = ""
		a.currentActionParams = nil
		a.currentActionStart = time.Time{}
	}
	wasPendingStop := a.pendingStopActionID == actionID
	if wasPendingStop {
		a.pendingStopActionID = ""
	}
	wasSelfStop := a.selfStopInProgress == actionID
	if wasSelfStop {
		a.selfStopInProgress = ""
	}
	src := a.currentActionSrc
	a.currentActionSrc = ""
	a.mu.Unlock()

	return CompletionResult{
		WasInFlight:    wasInFlight,
		WasPendingStop: wasPendingStop,
		WasSelfStop:    wasSelfStop,
		Src:            src,
	}
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
	a.actionQueue = nil
	a.currentActionID = ""
	a.currentActionCmd = ""
	a.currentActionParams = nil
	a.currentActionStart = time.Time{}
	a.currentActionSrc = ""
	a.currentSlot = ""
	a.redecomposeCount = 0
	a.mu.Unlock()
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
	a.currentSlot = ""
	a.redecomposeCount = 0
	a.mu.Unlock()
}

// RefillQueue replaces the action queue with the given actions and records
// the slot they were decomposed for.
func (a *AgentState) RefillQueue(actions []PlannedAction, slot string) {
	a.mu.Lock()
	a.actionQueue = actions
	a.currentSlot = slot
	a.mu.Unlock()
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
	a.mu.Unlock()
}

// SetCurrentPlanIndex records which daily-plan item is currently executing.
func (a *AgentState) SetCurrentPlanIndex(idx int) {
	a.mu.Lock()
	a.currentPlanIndex = idx
	a.mu.Unlock()
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

// DetectDayRollover checks for day_count increment (cross-day) and updates
// currentDay. Returns rollover=true when prev>=0 and day>prev (real cross-day,
// caller should re-run generateDailyPlan). First sync (prev<0) updates
// currentDay but returns rollover=false (worker already planned on startup).
func (a *AgentState) DetectDayRollover() (rollover bool, prevDay, newDay int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	day := a.latestDayCountLocked()
	prev := a.currentDay
	if day <= prev {
		return false, prev, day
	}
	a.currentDay = day
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
