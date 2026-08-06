// Package protocol defines the AgentTown WebSocket communication protocol
// as specified in docs/AgentTown_CommProtocol_Values.md.
//
// All messages share a 7-field envelope. Every business field (including
// action_id) lives inside payload — the envelope stays pure (约定1).
//
// Units (约定2/3): timestamps are Unix epoch milliseconds; durations use
// _ms/_sec suffixes; coordinates are UE5 centimeters, position=[X,Y,Z],
// rotation=[Pitch,Yaw,Roll] in degrees.
package protocol

import "encoding/json"

// Version is the current protocol version.
const Version = "1.0"

// SystemAgentID is the reserved agent_id for system-level messages
// (heartbeat / error) that don't belong to a specific agent (约定4).
const SystemAgentID = "system"

// Envelope is the fixed 7-field outer structure shared by all messages.
type Envelope struct {
	Version   string          `json:"version"`
	MsgID     string          `json:"msg_id"`   // UUID, for dedup/tracing
	Seq       int64           `json:"seq"`      // per-sender monotonic sequence
	Timestamp int64           `json:"timestamp"` // Unix epoch milliseconds
	Type      string          `json:"type"`
	AgentID   string          `json:"agent_id"`
	Payload   json.RawMessage `json:"payload"`
}

// Message type constants (§2.2).
const (
	TypePerceptionUpdate  = "perception_update"  // UE → Agent
	TypeActionCommand     = "action_command"     // Agent → UE
	TypeActionStarted     = "action_started"     // UE → Agent (ACK)
	TypeActionCompleted   = "action_completed"   // UE → Agent
	TypeStopAction        = "stop_action"        // Agent → UE
	TypeEventNotification = "event_notification" // Agent → Agent (internal)
	TypeStateReport       = "state_report"       // UE → Agent
	TypeAgentRegistered   = "agent_registered"   // UE → Agent
	TypeAgentUnregistered = "agent_unregistered" // UE → Agent
	TypeHeartbeat         = "heartbeat"          // bidirectional
	TypeError             = "error"              // bidirectional

	// TypeScanArea is a control message (not in the spec's type table) used
	// by the scan_area tool to request an immediate perception_update.
	TypeScanArea = "scan_area"

	// TypeResync is a control message exchanged after reconnect to convey
	// the sender's last successfully received seq so the peer can replay
	// discrete messages beyond it (约定11, §4.2).
	TypeResync = "resync"

	// TypeEventLost is a warning logged/emitted when the send buffer has
	// already rolled over the peer's requested resume point, so some
	// discrete messages cannot be replayed (约定11).
	TypeEventLost = "event_lost"

	// TypeCapabilityRegistry is sent by UE on connection to declare which
	// cmds the NPC can execute. agent_id="system" sets the global default;
	// a specific agent_id overrides it for that agent. MCP uses this to
	// drive tactical-layer prompt generation and dynamic MCP tool
	// registration (AddTool/RemoveTools).
	TypeCapabilityRegistry = "capability_registry"

	// TypeWorldKB is sent by UE on connection (agent_id="system") to push
	// the full world knowledge base (generated + authored JSON). MCP merges
	// the two halves via the worldkb pipeline, persists to --world-kb path,
	// and swaps the in-memory KB. Only accepted before the first
	// agent_registered; later pushes are rejected with a warning.
	TypeWorldKB = "world_kb"
)

// action_command cmd constants (§2.3).
// Data-driven via UE's DT_ActionBTMap; these are the framework-builtin
// representative cmds. Atomic = standalone minimal action; Composite =
// pre-registered high-level behavior tree.
const (
	// Atomic cmds (8).
	CmdMoveToLocation      = "MoveToLocation"
	CmdMoveToAgent         = "MoveToAgent"
	CmdTurnTo              = "TurnTo"
	CmdPlayMontage         = "PlayMontage"
	CmdSpeak               = "Speak"
	CmdEmote               = "Emote"
	CmdWait                = "Wait"
	CmdInteractSmartObject = "InteractSmartObject"
	// Composite cmds (6).
	CmdWorkAtWorkbench  = "WorkAtWorkbench"
	CmdWorkAtWorkshop   = "WorkAtWorkshop"
	CmdChatWith         = "ChatWith"
	CmdRepairTarget     = "RepairTarget"
	CmdChargeAtStation  = "ChargeAtStation"
	CmdPatrolZone       = "PatrolZone"
)

// IsCompositeCmd reports whether the given cmd is one of the 6 long
// composite cmds (ExecuteComposite family). Used by main.go to skip
// armActionTimeout for long composites — they run until the next
// schedule slot transition, not until a self timeout.
func IsCompositeCmd(cmd string) bool {
	switch cmd {
	case CmdWorkAtWorkbench, CmdWorkAtWorkshop, CmdChatWith,
		CmdRepairTarget, CmdChargeAtStation, CmdPatrolZone:
		return true
	}
	return false
}

// action_completed result constants (§2.3).
const (
	ResultSuccess     = "success"
	ResultFailed      = "failed"
	ResultInterrupted = "interrupted"
	ResultError       = "error"
)

// error_code constants (§2.3).
const (
	ErrActionFailed    = "ACTION_FAILED"
	ErrStopIDMismatch  = "STOP_ID_MISMATCH"
	ErrInvalidMessage  = "INVALID_MESSAGE"
	ErrUnknownAgent    = "UNKNOWN_AGENT"
	ErrInternalError   = "INTERNAL_ERROR"
)

// perception_level constants for event_notification (§2.3).
const (
	PerceptionDirect    = "direct"
	PerceptionBroadcast = "broadcast"
	PerceptionRumor     = "rumor"
)
