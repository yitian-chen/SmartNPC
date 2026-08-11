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
// params schema: smart_object + interaction.

// WorkShiftInput — composite: work at a specified facility.
type WorkShiftInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"work facility id, e.g. workbench_01, sortconveyor_01"`
	Interaction   string `json:"interaction" jsonschema:"interaction work type"`
}

// ChargeAtStationInput — composite: charge at a charging station.
type ChargeAtStationInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"charging station id, e.g. charging_pillar_01"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to charge"`
}

// SelfMaintenanceInput — composite: self-maintenance at a repair table.
type SelfMaintenanceInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"repair table id, e.g. repair_table_01"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to repair_self"`
}

// RestAtResidenceInput — composite: rest at a sleep pod.
type RestAtResidenceInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"sleep pod id, e.g. sleep_pod_01"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to sleep"`
}

// SurfInternetInput — composite: surf the internet at a computer.
type SurfInternetInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"computer id"`
	Interaction   string `json:"interaction" jsonschema:"interaction type, fixed to surf_internet"`
}

// registerComposite installs the composite-behavior tools.
func registerComposite(s *mcp.Server, ex Executor, logger *slog.Logger) {
	// work_shift
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_shift",
		Description: "Go to a specified facility and work. Composite behavior — runs a full work routine.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkShiftInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("work_shift", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdWorkShift, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
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
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("charge_at_station", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdChargeAtStation, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
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
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("self_maintenance", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSelfMaintenance, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
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
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("rest_at_residence", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdRestAtResidence, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
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
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("surf_internet", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSurfInternet, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("surf_internet: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
