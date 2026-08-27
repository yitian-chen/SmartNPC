// Package contract defines the interface boundary between the WS transport
// layer (pkg/wsserver) and the agent decision layer (cmd/agenttown-mcp).
//
// The agent side depends on Transport + MessageHandler/DisconnectHandler
// instead of the concrete *wsserver.Server, so the two sides can be split
// into independent modules and the agent module can be swapped by changing a
// dependency version without touching the transport implementation.
package contract

import (
	"context"
	"encoding/json"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// MessageHandler receives inbound envelopes from UE (UE → Agent).
// It receives the message type, agent_id, and raw payload.
type MessageHandler func(ctx context.Context, msgType, agentID string, payload json.RawMessage)

// DisconnectHandler is invoked once when the active UE WebSocket connection
// ends (not when an existing connection is replaced by a newer one).
type DisconnectHandler func()

// Transport is the outbound + inbound surface the agent decision layer needs
// from the WS transport. wsserver.Server implements it.
//
// The action lifecycle (action_command → action_started ACK) stays inside the
// transport implementation; the agent side only sees the returned ACK.
type Transport interface {
	// SendAction sends an action_command (cmd + params) and waits for the
	// action_started ACK. Returns the ACK.
	SendAction(ctx context.Context, agentID, cmd string, params map[string]any, autoQueue bool) (*protocol.ActionStartedPayload, error)
	// RequestScan asks UE to emit an immediate perception_update (fire-and-forget).
	RequestScan(ctx context.Context, agentID, scanID string) error
	// SendStopAction sends a stop_action control message (fire-and-forget).
	SendStopAction(agentID, actionID string) error
	// SendEnvelope sends an arbitrary envelope (e.g. chat_invite_rsp / chat_turn).
	SendEnvelope(agentID, msgType string, payload any) error
	// IsConnected reports whether a UE WebSocket connection is active.
	IsConnected() bool
	// SetMessageHandler registers the inbound message callback.
	SetMessageHandler(MessageHandler)
	// SetDisconnectHandler registers the disconnect callback.
	SetDisconnectHandler(DisconnectHandler)
}
