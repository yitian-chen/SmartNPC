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
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// TacticalInput aggregates all inputs needed by BuildTactical.
// Replaces the 9 positional args of the old buildTacticalPrompt.
type TacticalInput struct {
	Goal      string
	Zone      string
	TimeOfDay string
	Slot      string
	// DailyPlan is the agent's full-day schedule (formatted "HH:MM-HH:MM: goal"
	// lines), injected as 【全天日程】 so the tactical LLM sees the day's
	// context around the current slot. Empty = skip the segment.
	DailyPlan string
	// Compact switches the tactical user message to the compact form: the
	// per-day-invariant blocks (【全天日程】 and the full TacticalRules) are
	// replaced by a short core-rules digest plus a reference line pointing at
	// the day's first tactical message which carried them in full. Caller
	// (generateTacticalPlan) sets it only when that full header is already in
	// the conversation history. Zero value false = full form (backward
	// compatible).
	Compact  bool
	Physical *protocol.PhysicalState
	KB       *worldkb.KB
	Profiles map[string]*profile.Profile // NPC persona override; nil → AgentRole falls back to KB then hardcoded
	Hint     string
	Memories string // Stage 4: formatted bullet list of recent memories; empty = skip segment
	// Relationships is the merged other-NPC roster + affection list
	// (formatRelationshipsForPrompt: every KB peer with 好感度 x/10, stored
	// rows overlay, default 5 when absent) for the 【人际关系】 segment;
	// empty = skip segment (single-NPC scenario). Also suppresses the
	// 【其他NPC】 KB-roster fallback in BuildTactical (redundant roster).
	Relationships string
	// Commitments is the formatted pending-commitment list（临近到期/命中眼前人）
	// for the 【当前需兑现的约定】 segment; empty = skip segment.
	Commitments string
	// Events is the safe-point drain of queued non-force world events
	// (P3-7, §4.4/§5.1 第三输入), pre-rendered via FormatWorldEventList;
	// empty = skip the 【发生的事件】 segment. Unlike 【全天日程】 it is
	// per-call data — injected in BOTH full and compact forms.
	Events string
	// Situations is the pre-rendered active-situation list（P3-9 修复 A：
	// combat 类事件登记的持续威胁，combat_exit/TTL 解除）。Injected as
	// 【当前处境】 so every refill sees the ongoing threat until it is
	// actually resolved — the LLM must not declare it over on its own.
	// Empty = skip.
	Situations string
	// Conversation is the in-progress conversation transcript block (peer +
	// turns + round count), rendered from the agent-side session state
	// (recordConversationTurn: own utterances via speak_to dispatch, peer
	// utterances via speech_heard). Consumed only by the 【社交回应】 branch
	// of tacticalHintLine so the LLM can judge where the dialogue stands
	// instead of seeing only the latest utterance; empty = omit.
	Conversation string
	// ConversationCapped marks the conversation having reached the
	// turn cap (caller-side maxConversationTurns) — the 【社交回应】 branch
	// appends a hard wrap-up directive (one farewell + next action, no new
	// topics) so a conversation never loops forever regardless of sampling.
	ConversationCapped bool
	AgentID            string
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
	// VisibleAgents is the visible-NPC list from the latest
	// perception_update (Phase 2 Module C). Injected as 【附近NPC】 so the
	// LLM can pick a conversation target. nil/empty = no nearby
	// NPCs → skip the segment.
	VisibleAgents []protocol.VisibleAgent
}
