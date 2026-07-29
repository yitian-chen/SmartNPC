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
//
// P1 恢复 scan_area/stop 工具后，Executor 增加 RequestScan 和 SendStopAction
// 两个方法，对应 reaction 层的两个工具调用。
type Executor interface {
	// SendAction sends an action_command (cmd + params) for the given agent
	// and waits for the action_started ACK. Returns the ACK.
	SendAction(ctx context.Context, agentID string, decisionEpoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error)

	// RequestScan asks UE to emit an immediate perception_update for the
	// agent (fire-and-forget). The perception arrives asynchronously via
	// the normal perception_update channel, triggering observePerception.
	// scanID is echoed by the response for correlation.
	RequestScan(ctx context.Context, agentID, scanID string) error

	// SendStopAction sends a stop_action control message to stop the given
	// action. If actionID is empty, the implementation looks up the agent's
	// current in-flight action. Fire-and-forget (no ACK expected).
	SendStopAction(agentID, actionID string) error
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
