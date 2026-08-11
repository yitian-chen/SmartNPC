package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// Composite tools (§2.3) translate to their own Composite cmd. Each
// carries agent_id as the first parameter. Per 约定14, Agent prefers
// composite behaviors for routine goals; atomic behaviors are for
// reactions or cases the composite library doesn't cover.
//
// New 5 composite cmds (2026-08-11): WorkShift / ChargeAtStation /
// SelfMaintenance / RestAtResidence / SurfInternet. All share the same
// params schema: semantic_group + interaction.
//
// Parameter naming (2026-08-11 fix): MCP previously sent smart_object
// as the param key, but real UE5's capability_registry declares the
// required param as semantic_group. Without this key, UE5 cannot find
// the target facility and the composite action fast-returns without
// executing the work phase. The value passed is the semantic group name
// (e.g. "workbench", "charger") which the world_kb already uses as
// object IDs — UE5 resolves an idle instance from that group.
//
// auto_queue (约定21) lives inside params per UE5's schema, not at the
// envelope level. UE5 expects a string "true"/"false" for
// ChargeAtStation and a bool for InteractSmartObject. The envelope-level
// AutoQueue field is deprecated (always omitted); params-level value is
// the authoritative source for real UE5.

// WorkShiftInput — composite: work at a specified facility.
type WorkShiftInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SemanticGroup string `json:"semantic_group" jsonschema:"work facility semantic group name, e.g. workbench, sorting_conveyor"`
	Interaction   string `json:"interaction" jsonschema:"interaction work type"`
}

// ChargeAtStationInput — composite: charge at a charging station.
type ChargeAtStationInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SemanticGroup string `json:"semantic_group" jsonschema:"charging facility semantic group name, e.g. charger"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to charge"`
}

// SelfMaintenanceInput — composite: self-maintenance at a repair table.
type SelfMaintenanceInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SemanticGroup string `json:"semantic_group" jsonschema:"repair facility semantic group name, e.g. repair_table"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to repair_self"`
}

// RestAtResidenceInput — composite: rest at a sleep pod.
type RestAtResidenceInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SemanticGroup string `json:"semantic_group" jsonschema:"rest facility semantic group name, e.g. sleep_pod"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to sleep"`
}

// SurfInternetInput — composite: surf the internet at a computer.
type SurfInternetInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SemanticGroup string `json:"semantic_group" jsonschema:"computer semantic group name, e.g. computer"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to surf_internet"`
}

// registerComposite installs the composite-behavior tools.
func registerComposite(s *mcp.Server, ex Executor, logger *slog.Logger) {
	// work_shift
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_shift",
		Description: "Go to a specified facility and work. Composite behavior — runs a full work routine.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkShiftInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SemanticGroup == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, semantic_group and interaction are required")
		}
		logToolCall("work_shift", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdWorkShift, map[string]any{
			"semantic_group": in.SemanticGroup,
			"interaction":    in.Interaction,
			"auto_queue":     "true",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("work_shift: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// charge_at_station
	mcp.AddTool(s, &mcp.Tool{
		Name:        "charge_at_station",
		Description: "Charge at a charging station. Composite behavior — restores battery.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChargeAtStationInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SemanticGroup == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, semantic_group and interaction are required")
		}
		logToolCall("charge_at_station", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdChargeAtStation, map[string]any{
			"semantic_group": in.SemanticGroup,
			"interaction":    in.Interaction,
			"auto_queue":     "true",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("charge_at_station: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// self_maintenance
	mcp.AddTool(s, &mcp.Tool{
		Name:        "self_maintenance",
		Description: "Go to a repair table and perform self-inspection/maintenance. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SelfMaintenanceInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SemanticGroup == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, semantic_group and interaction are required")
		}
		logToolCall("self_maintenance", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSelfMaintenance, map[string]any{
			"semantic_group": in.SemanticGroup,
			"interaction":    in.Interaction,
			"auto_queue":     "true",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("self_maintenance: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// rest_at_residence
	mcp.AddTool(s, &mcp.Tool{
		Name:        "rest_at_residence",
		Description: "Go to a sleep pod and rest. Composite behavior — restores energy overnight.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RestAtResidenceInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SemanticGroup == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, semantic_group and interaction are required")
		}
		logToolCall("rest_at_residence", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdRestAtResidence, map[string]any{
			"semantic_group": in.SemanticGroup,
			"interaction":    in.Interaction,
			"auto_queue":     "true",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("rest_at_residence: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// surf_internet
	mcp.AddTool(s, &mcp.Tool{
		Name:        "surf_internet",
		Description: "Go to a computer and surf the internet. Composite behavior — for leisure or research.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SurfInternetInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SemanticGroup == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, semantic_group and interaction are required")
		}
		logToolCall("surf_internet", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSurfInternet, map[string]any{
			"semantic_group": in.SemanticGroup,
			"interaction":    in.Interaction,
			"auto_queue":     "true",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("surf_internet: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
