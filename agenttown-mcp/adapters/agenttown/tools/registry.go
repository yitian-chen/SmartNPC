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

// ToolSpec describes one built-in tool's name and the UE cmd it depends
// on. RequiredCmd == "" means the tool has no UE cmd dependency (e.g.
// scan_area, which only triggers an immediate perception_update) and is
// therefore always available.
type ToolSpec struct {
	Name        string
	RequiredCmd string
}

// BuiltinToolSpecs is the static metadata table for all 15 built-in
// tools, mapping each tool name to the UE cmd it translates to. Used by
// ReconcileTools to decide which tools to keep/remove based on the
// capability registry.
//
// Order is irrelevant — ReconcileTools reads it as a set.
func BuiltinToolSpecs() []ToolSpec {
	return []ToolSpec{
		// Atomic tools (8).
		{Name: "move_to_location", RequiredCmd: protocol.CmdMoveToLocation},
		{Name: "move_to_agent", RequiredCmd: protocol.CmdMoveToAgent},
		{Name: "turn_to", RequiredCmd: protocol.CmdTurnTo},
		{Name: "play_montage", RequiredCmd: protocol.CmdPlayMontage},
		{Name: "speak", RequiredCmd: protocol.CmdSpeak},
		{Name: "emote", RequiredCmd: protocol.CmdEmote},
		{Name: "interact", RequiredCmd: protocol.CmdInteractSmartObject},
		{Name: "wait", RequiredCmd: protocol.CmdWait},
		// stop has no UE cmd dependency — it sends the stop_action control
		// message (TypeStopAction), not an action_command. RequiredCmd is
		// "" so ReconcileTools never removes it based on capability state.
		{Name: "stop", RequiredCmd: ""},
		// scan_area has no UE cmd — it triggers an immediate
		// perception_update via RequestScan, not an action_command.
		{Name: "scan_area", RequiredCmd: ""},
		// Composite tools (6) — each maps to its own Composite cmd.
		{Name: "work_at_workbench", RequiredCmd: protocol.CmdWorkAtWorkbench},
		{Name: "work_at_workshop", RequiredCmd: protocol.CmdWorkAtWorkshop},
		{Name: "chat_with", RequiredCmd: protocol.CmdChatWith},
		{Name: "repair_target", RequiredCmd: protocol.CmdRepairTarget},
		{Name: "charge_at_station", RequiredCmd: protocol.CmdChargeAtStation},
		{Name: "patrol_zone", RequiredCmd: protocol.CmdPatrolZone},
	}
}

// ReconcileTools ensures the tools registered on s match the capability
// set implied by hasCmd. Tools whose RequiredCmd is unavailable (and
// isn't "") are removed via s.RemoveTools; the rest are (re-)registered
// via RegisterAll (mcp.AddTool is idempotent — it replaces existing
// tools with the same name).
//
// hasCmd returns whether a given cmd is currently available to the
// global/system scope. Per-agent capability enforcement happens
// separately in the guardedExecutor (SendAction gates on
// registry.HasCmd(agentID, cmd)).
//
// This is invoked on every capability_registry message and is safe to
// call multiple times.
func ReconcileTools(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger, hasCmd func(string) bool) {
	if logger == nil {
		logger = slog.Default()
	}
	RegisterAll(s, ex, kb, logger)
	var drop []string
	for _, spec := range BuiltinToolSpecs() {
		if spec.RequiredCmd == "" {
			continue
		}
		if !hasCmd(spec.RequiredCmd) {
			drop = append(drop, spec.Name)
		}
	}
	if len(drop) > 0 {
		s.RemoveTools(drop...)
		logger.Info("capability reconcile: removed tools for unavailable cmds", "tools", drop)
	}
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
