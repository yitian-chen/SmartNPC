// Package wsserver hosts the WebSocket server that Mock UE connects to.
//
// Mock UE is the client; agenttown-mcp is the server. This is the inverse of
// the SmartNPC layout (where mcp was the WS client to a SMAPI mod server).
//
// Three frame types flow over the connection:
//
//   - Request  (MCP -> Mock UE): tool execution call, correlated by ID
//   - Response (Mock UE -> MCP): tool execution result, matched by ID
//   - Event    (Mock UE -> MCP): push (perception_update, day_started, ...)
package wsserver

import "encoding/json"

// Frame type constants.
const (
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeEvent    = "event"
)

// Event names (Mock UE -> MCP, push).
const (
	EventPerceptionUpdate = "perception_update"
	EventDayStarted       = "day_started"
	EventDayEnded         = "day_ended"
)

// Action names (MCP -> Mock UE, request/response). One per MCP tool.
const (
	ActionMoveTo       = "move_to"
	ActionTurnTo       = "turn_to"
	ActionInteractWith = "interact_with"
	ActionSpeak        = "speak"
	ActionWait         = "wait"
	ActionChargeAt     = "charge_at"
	ActionWorkAssemble = "work_assemble"
	ActionSelfCheck    = "self_check"
	ActionEmote        = "emote"
	ActionUpdatePlan   = "update_plan"
)

// Request is a tool-execution call from MCP to Mock UE.
type Request struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Action string `json:"action"`
	Params any    `json:"params,omitempty"`
}

// Response is the result of a Request, correlated by ID.
type Response struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *ResponseError  `json:"error,omitempty"`
}

// ResponseError describes a tool execution failure.
type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Event is a push frame from Mock UE to MCP (no ID, fire-and-forget).
type Event struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
	Timestamp int64           `json:"timestamp"`
}
