// Package tools registers the AgentTown MCP tools that Hermes calls during
// a turn. Per the protocol spec (§6.4):
//
//   - Every tool's FIRST parameter is agent_id (multi-NPC isolation).
//   - Composite tools translate to ExecuteComposite action_command.
//   - Atomic tools translate to their corresponding cmd (MoveTo/Speak/...).
//   - The tool returns after the action_started ACK (with estimated
//     duration); action_completed arrives asynchronously and is folded
//     into the next perception (§6.2 bridge).
//
// Tools are stateless from the MCP side — Mock UE (simulating UE) is the
// source of truth for physical/spatial state.
package tools

import (
	"context"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// Executor is the interface tools use to send an action_command and await
// the ACK. The concrete implementation wraps *wsserver.Server; tests can
// substitute a fake.
type Executor interface {
	// SendAction sends an action_command (cmd + params) for the given agent
	// and waits for the action_started ACK. Returns the ACK.
	SendAction(ctx context.Context, agentID string, decisionEpoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error)

	// RequestScan asks Mock UE to emit an immediate perception_update for
	// the given agent (backs the scan_area tool). Returns after the request
	// is sent.
	RequestScan(ctx context.Context, agentID string, decisionEpoch int64) error

	// LookupCurrentActionID returns the action_id currently executing for
	// the agent (empty if none). Used by the stop tool for stop_action ID
	// matching (约定9).
	LookupCurrentActionID(agentID string) string

	// ClearCurrentActionID clears the local tracking of the current action
	// (called after sending stop_action).
	ClearCurrentActionID(agentID string)

	// SendStopAction sends a stop_action control message to UE for the
	// given action_id (约定9). Fire-and-forget — no ACK expected.
	SendStopAction(ctx context.Context, agentID, actionID string) error
}

// RegisterAll installs all AgentTown tools onto the given mcp.Server.
//
// kb is the loaded World KB, used by atomic tools (currently move_to) to
// resolve semantic targets to coordinates before dispatching to UE. May be
// nil in tests that don't exercise target resolution — nil-safe methods on
// KB return zero values.
func RegisterAll(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	setToolsLogger(logger)
	registerComposite(s, ex, logger)
	registerAtomic(s, ex, kb, logger)
}

// ackResult is the common output shape all tools return: it echoes the
// action_id and estimated duration from the ACK so the LLM knows the
// action was accepted and roughly how long it will take. The actual
// completion arrives later via perception.
type ackResult struct {
	OK                   bool     `json:"ok"                     jsonschema:"true if the action was accepted by UE"`
	DecisionEpoch        int64    `json:"decision_epoch"         jsonschema:"decision epoch accepted by MCP"`
	ActionID             string   `json:"action_id"              jsonschema:"the action's unique id"`
	EstimatedDurationSec *float64 `json:"estimated_duration_sec" jsonschema:"UE's estimate of how long the action takes (seconds)"`
	Message              string   `json:"message,omitempty"      jsonschema:"human-readable status"`
}

// buildAckResult constructs the standard output from an ACK.
func buildAckResult(ack *protocol.ActionStartedPayload, decisionEpoch int64) ackResult {
	r := ackResult{OK: true, DecisionEpoch: decisionEpoch}
	if ack != nil {
		r.ActionID = ack.ActionID
		r.EstimatedDurationSec = ack.EstimatedDurationSec
	}
	r.Message = "action accepted"
	return r
}
