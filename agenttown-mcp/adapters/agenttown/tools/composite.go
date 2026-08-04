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

// WorkAtWorkbenchInput — composite: work at a specific workbench.
type WorkAtWorkbenchInput struct {
	AgentID        string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetObjectID string  `json:"target_object_id" jsonschema:"workbench object id from the world_kb"`
	DurationSec    float64 `json:"duration_sec,omitempty" jsonschema:"work duration in seconds (optional)"`
}

// WorkAtWorkshopInput — composite: work in the workshop (auto-pick bench).
type WorkAtWorkshopInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
}

// ChatWithInput — composite: chat with another agent.
type ChatWithInput struct {
	AgentID        string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID  string `json:"target_agent_id" jsonschema:"the agent to chat with"`
	Topic          string `json:"topic,omitempty" jsonschema:"conversation topic (optional)"`
}

// RepairTargetInput — composite: repair another agent.
type RepairTargetInput struct {
	AgentID        string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID  string `json:"target_agent_id" jsonschema:"the agent to repair"`
	ToolID         string `json:"tool_id,omitempty" jsonschema:"tool id (optional)"`
}

// ChargeAtStationInput — composite: charge at a station.
type ChargeAtStationInput struct {
	AgentID        string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetObjectID string `json:"target_object_id,omitempty" jsonschema:"charging station id (optional; auto-pick if empty)"`
}

// PatrolZoneInput — composite: patrol a zone.
type PatrolZoneInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetZone    string  `json:"target_zone" jsonschema:"zone id to patrol"`
	DurationSec   float64 `json:"duration_sec,omitempty" jsonschema:"patrol duration in seconds (optional)"`
}

// registerComposite installs the composite-behavior tools.
func registerComposite(s *mcp.Server, ex Executor, logger *slog.Logger) {
	// work_at_workbench
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_at_workbench",
		Description: "Work at a specific workbench for a duration. Composite behavior — runs a full assembly routine.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkAtWorkbenchInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetObjectID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_object_id are required")
		}
		logToolCall("work_at_workbench", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{
			"target_object_id": in.TargetObjectID,
		}
		if in.DurationSec > 0 {
			params["duration_sec"] = in.DurationSec
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdWorkAtWorkbench, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("work_at_workbench: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// work_at_workshop
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_at_workshop",
		Description: "Go to the workshop and perform routine work (auto-picks an available bench). Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkAtWorkshopInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("work_at_workshop", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdWorkAtWorkshop, map[string]any{})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("work_at_workshop: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// chat_with
	mcp.AddTool(s, &mcp.Tool{
		Name:        "chat_with",
		Description: "Have a social chat with another agent. Composite behavior — approaches, faces, and converses.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatWithInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_agent_id are required")
		}
		logToolCall("chat_with", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{
			"target_agent_id": in.TargetAgentID,
		}
		if in.Topic != "" {
			params["topic"] = in.Topic
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdChatWith, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("chat_with: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// repair_target
	mcp.AddTool(s, &mcp.Tool{
		Name:        "repair_target",
		Description: "Repair another agent. Composite behavior — approaches, inspects, and repairs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RepairTargetInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_agent_id are required")
		}
		logToolCall("repair_target", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{
			"target_agent_id": in.TargetAgentID,
		}
		if in.ToolID != "" {
			params["tool_id"] = in.ToolID
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdRepairTarget, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("repair_target: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// charge_at_station
	mcp.AddTool(s, &mcp.Tool{
		Name:        "charge_at_station",
		Description: "Charge at a charging station. Composite behavior — restores battery. Auto-picks a station if target_object_id is empty.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChargeAtStationInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("charge_at_station", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{}
		if in.TargetObjectID != "" {
			params["target_object_id"] = in.TargetObjectID
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdChargeAtStation, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("charge_at_station: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// patrol_zone
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_zone",
		Description: "Patrol a zone. Composite behavior — enters the zone and follows its patrol strategy.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PatrolZoneInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetZone == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_zone are required")
		}
		logToolCall("patrol_zone", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{
			"target_zone": in.TargetZone,
		}
		if in.DurationSec > 0 {
			params["duration_sec"] = in.DurationSec
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdPatrolZone, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("patrol_zone: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
