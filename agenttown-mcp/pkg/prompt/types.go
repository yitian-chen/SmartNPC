// Package prompt holds the pure-function prompt builders for the three
// decision layers (strategic / tactical / reactive). Functions in this
// package take data inputs (KB, capability actions, snapshots) and return
// prompt text — they do not touch agentContext or call LLM clients.
//
// Design:
//   - All functions are pure: no side effects, no I/O, no mutexes.
//   - KB and capability actions are passed as parameters (prompt holds no
//     references), so the package stays dependency-light.
//   - Capability registry is decoupled: callers pass []protocol.CapabilityAction
//     (from registry.EffectiveActions) instead of *CapabilityRegistry.
package prompt

import (
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// TacticalInput aggregates all inputs needed by BuildTactical.
// Replaces the 9 positional args of the old buildTacticalPrompt.
type TacticalInput struct {
	Goal      string
	Zone      string
	TimeOfDay string
	Slot      string
	Physical  *protocol.PhysicalState
	KB        *worldkb.KB
	Profiles  map[string]*profile.Profile // NPC persona override; nil → AgentRole falls back to KB then hardcoded
	Hint      string
	Memories  string // Stage 4: formatted bullet list of recent memories; empty = skip segment
	Relationships string // Stage 5: formatted relationship list for 【人际关系】段; empty = skip segment (single-NPC scenario)
	Actions   []protocol.CapabilityAction // from registry.EffectiveActions(agentID); nil → builtin fallback
	AgentID   string
	// ObjectStatus is UE5's per-category smart object availability aggregate
	// (cross-zone). Injected into the tactical prompt as 【物体实时占用】 so
	// the LLM can avoid planning actions targeting occupied objects. nil =
	// no perception data yet (cold start) → skip the segment.
	ObjectStatus map[string]protocol.ObjectCategoryStatus
	// NearbyObjects is the per-instance object state for the NPC's current
	// zone (from the latest perception_update). Used to disambiguate which
	// semantic_group within a multi-group category is occupied. nil/empty =
	// no nearby objects → only the category aggregate is shown.
	NearbyObjects []protocol.NearbyObject
}

// ReactiveTrigger identifies what triggered a reactive evaluation.
// Used for dedupe, logging, and as part of ReactiveInput.
type ReactiveTrigger string

const (
	TriggerZoneChange    ReactiveTrigger = "zone_change"    // NPC entered a new zone
	TriggerNewObject     ReactiveTrigger = "new_object"     // nearby_objects gained a new object
	TriggerEventNotify   ReactiveTrigger = "event_notify"   // received event_notification
	TriggerPhysicalAlert ReactiveTrigger = "physical_alert" // physical state crossed alert threshold
	TriggerActionDone    ReactiveTrigger = "action_done"    // action_completed, natural evaluation point
	TriggerPeriodic      ReactiveTrigger = "periodic"       // periodic trigger: force evaluation every N perceptions
)

// ReactiveInput aggregates all inputs needed by BuildReactive.
// Moved verbatim from reactive.go (field types unchanged).
type ReactiveInput struct {
	AgentID           string
	AgentName         string // agent display name (e.g. "老陈"), used in prompt salutation; empty → fall back to AgentID
	AgentRole         string // 【你的角色】段 (name/profession/background/personality/speech style), from AgentRole(); empty → kb unavailable or agent missing
	TimeOfDay         string // "HH:MM" game time
	Zone              string // current zone id
	Energy            float64
	Fatigue           float64
	JointWear         float64 // joint wear (0-100, higher = more worn); triggers maintenance alert above JointWearAlertThreshold
	PhysicalAvailable bool    // whether physical state is usable (perception_update all-0 → false, skip physical segment)
	CurrentAction     string // readable description of in-flight action (e.g. "WorkShift(smart_object=workbench_01)"), empty = no in-flight
	ElapsedSec        int    // seconds the current action has been running
	ActionSrc         string // in-flight action source: tactical / mcp_tool / empty
	QueuedFor         string // 排队状态描述（约定21，如 "正在排队等待 workbench（位置 2，预计等待 30 秒）"），空 = 不在排队
	CurrentSlot       string // current tactical slot "HH:MM-HH:MM", empty = not decomposed
	DailyPlan         string // strategic daily plan summary (formatted string), empty = not generated
	Trigger           ReactiveTrigger
	TriggerDetail     string // trigger reason detail
}

// ToolEntry is the intermediate representation for the tactical prompt's
// tool list segment. Produced by ToolEntries, consumed by BuildTacticalToolList
// and StrategicCapabilitySummary.
type ToolEntry struct {
	Name        string
	RequiredCmd string
	Kind        string // "atomic" | "composite"
	Desc        string
	Params      string
}
