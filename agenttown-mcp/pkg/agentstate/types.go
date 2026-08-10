// Package agentstate holds per-agent business state with semantics-named
// methods. Fields are private; callers interact through methods and the
// exported Snapshot type. Coordination fields (wake channel, cancel func,
// pending timers, replan-in-progress flag) live in the main package's
// agentContext, which holds a *AgentState pointer and forwards business
// reads/writes.
package agentstate

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// PlannedAction is a single step produced by the tactical layer that
// corresponds to one MCP tool call.
type PlannedAction struct {
	Action string         `json:"action"` // tool name: work_at_workbench / move_to_location / ...
	Params map[string]any `json:"params"` // tool params (raw LLM output, duration_sec not converted)
}

// ActionSource identifies which layer issued an in-flight action and
// determines routing on completion.
type ActionSource string

const (
	// SourceTool marks actions issued via MCP tool calls.
	SourceTool ActionSource = "mcp_tool"
	// SourceTactical marks actions issued by the tactical layer queue.
	SourceTactical ActionSource = "tactical"
)

// Snapshot is an exported read-only copy of AgentState's fields, used by
// the prompt package (stage 2) and tests. All fields are value types or
// deep copies; mutating a Snapshot does not affect the source AgentState.
type Snapshot struct {
	Online              bool
	LatestPhysical      *protocol.PhysicalState
	LatestPerception    json.RawMessage
	CurrentTask         *protocol.CurrentTaskProgress
	CurrentActionID     string
	CurrentActionCmd    string
	CurrentActionParams map[string]any
	CurrentActionStart  time.Time
	CurrentActionSrc    ActionSource
	ActionQueue         []PlannedAction
	DailyPlan           string
	CurrentDay          int
	CurrentPlanIndex    int
	CurrentSlot         string
	RedecomposeCount    int
	PrevZone            string
	PrevObjectIDs       []string
	PerceptionCount     int
	ReplanHint          string
	LastReplanAt        time.Time
	LastReplanGameTime  string
	PendingStopActionID string
	SelfStopInProgress  string
}

// LatestZone returns the current zone from the snapshot's embedded
// perception. Convenience method for callers that need zone info.
func (s Snapshot) LatestZone() string {
	if len(s.LatestPerception) == 0 {
		return ""
	}
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(s.LatestPerception, &p); err != nil {
		return ""
	}
	if p.Location.CurrentZone != nil {
		return *p.Location.CurrentZone
	}
	return ""
}

// LatestTimeOfDay returns "HH:MM" from the snapshot's embedded perception.
func (s Snapshot) LatestTimeOfDay() string {
	if len(s.LatestPerception) == 0 {
		return ""
	}
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(s.LatestPerception, &p); err != nil {
		return ""
	}
	totalSec := int(p.Environment.TimeOfDaySec)
	if totalSec < 0 || totalSec >= 86400 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", totalSec/3600, (totalSec%3600)/60)
}
