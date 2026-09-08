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
	TypeActionQueued      = "action_queued"      // UE → Agent (queue status: queued/advanced/timeout)
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

	// Phase 2 Module C: NPC-to-NPC dialogue protocol (§3.1 of
	// AgentTown_Dialogue_Design.md). UE owns the session state machine
	// (Inviting/Active/Closed) and conv_id generation; MCP handles
	// accept/reject decisions and chat_turn content generation.
	TypeChatInvite    = "chat_invite"     // UE → B: A wants to talk (carries conv_id + A's opening line)
	TypeChatInviteRsp = "chat_invite_rsp" // B → UE (forwarded to A): accept/reject decision only
	TypeChatTurn      = "chat_turn"       // speaker → UE (forwarded to peer): one utterance
)

// action_command cmd constants (§2.3).
// Data-driven via UE's DT_ActionBTMap; these are the framework-builtin
// representative cmds. Atomic = standalone minimal action; Composite =
// pre-registered high-level behavior tree.
//
// 13 cmds total (7 atomic + 6 composite), aligned with the real UE5
// capability_registry push (2026-08-11 + Phase 2 Module C SocialChat).
const (
	// Atomic cmds (7).
	CmdGenericAct          = "GenericAct"
	CmdMoveTo              = "MoveTo"
	CmdWait                = "Wait"
	CmdTurnTo              = "TurnTo"
	CmdSpeak               = "Speak"
	CmdInteractSmartObject = "InteractSmartObject"
	CmdEmote               = "Emote"
	// Composite cmds (6).
	CmdWorkShift        = "WorkShift"
	CmdChargeAtStation  = "ChargeAtStation"
	CmdSelfMaintenance  = "SelfMaintenance"
	CmdRestAtResidence  = "RestAtResidence"
	CmdSurfInternet     = "SurfInternet"
	CmdSocialChat       = "SocialChat" // Phase 2 Module C: proactive NPC-to-NPC dialogue
)

// IsCompositeCmd reports whether the given cmd is one of the long
// composite cmds. Used by main.go to skip armActionTimeout for long
// composites — they run until the next schedule slot transition, not
// until a self timeout.
func IsCompositeCmd(cmd string) bool {
	switch cmd {
	case CmdWorkShift, CmdChargeAtStation, CmdSelfMaintenance,
		CmdRestAtResidence, CmdSurfInternet, CmdSocialChat:
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

// action_queued status constants (约定21, §2.3).
// queued: Agent 已入队开始排队; advanced: 轮到该 Agent 开始执行;
// timeout: 排队超时被移除，UE 会补 action_completed{failed,queue_timeout}.
const (
	QueueStatusQueued   = "queued"
	QueueStatusAdvanced = "advanced"
	QueueStatusTimeout  = "timeout"
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
