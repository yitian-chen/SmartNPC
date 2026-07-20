package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// Composite tools (§6.4) translate to ExecuteComposite action_command.
// Each carries agent_id as the first parameter. Durations are expressed to
// the LLM in minutes (duration_min) and converted to seconds internally.

const secondsPerMinute = 60

// WorkAssembleInput — composite: assemble at a workbench.
type WorkAssembleInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id, e.g. \"H-01\""`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Target        string  `json:"target"       jsonschema:"workbench id, e.g. workbench_01"`
	DurationMin   float64 `json:"duration_min" jsonschema:"work duration in minutes"`
}

// PatrolRouteInput — composite: patrol a named route.
type PatrolRouteInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	RouteID       string `json:"route_id" jsonschema:"route id to patrol"`
}

// ChargeAtInput — composite: charge at a station.
type ChargeAtInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	StationID     string  `json:"station_id"   jsonschema:"charging station id, e.g. charging_station_01"`
	DurationMin   float64 `json:"duration_min" jsonschema:"charge duration in minutes"`
}

// RepairTargetInput — composite: repair another agent.
type RepairTargetInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID string `json:"target_agent_id" jsonschema:"the agent to repair"`
}

// SocialChatWithInput — composite: chat with another agent.
type SocialChatWithInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID string `json:"target_agent_id" jsonschema:"the agent to chat with"`
}

// RestIdleInput — composite: rest/idle for a while.
type RestIdleInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	DurationMin   float64 `json:"duration_min" jsonschema:"rest duration in minutes"`
}

// ArchiveResearchInput — composite: do archive research.
type ArchiveResearchInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	DurationMin   float64 `json:"duration_min" jsonschema:"research duration in minutes"`
}

// registerComposite installs the composite-behavior tools.
func registerComposite(s *mcp.Server, ex Executor, logger *slog.Logger) {
	// work_assemble
	mcp.AddTool(s, &mcp.Tool{
		Name:        "work_assemble",
		Description: "Assemble parts at a workbench for a duration. Composite behavior — runs a full assembly routine.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkAssembleInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Target == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target are required")
		}
		logToolCall("work_assemble", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":         "work_assemble",
			"target":       in.Target,
			"duration_sec": in.DurationMin * secondsPerMinute,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("work_assemble: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// patrol_route
	mcp.AddTool(s, &mcp.Tool{
		Name:        "patrol_route",
		Description: "Patrol a predefined route. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PatrolRouteInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.RouteID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and route_id are required")
		}
		logToolCall("patrol_route", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":     "patrol_route",
			"route_id": in.RouteID,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("patrol_route: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// charge_at
	mcp.AddTool(s, &mcp.Tool{
		Name:        "charge_at",
		Description: "Charge at a charging station for a duration. Composite behavior — restores battery.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChargeAtInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.StationID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and station_id are required")
		}
		logToolCall("charge_at", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":         "charge_at",
			"station_id":   in.StationID,
			"duration_sec": in.DurationMin * secondsPerMinute,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("charge_at: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// repair_target
	mcp.AddTool(s, &mcp.Tool{
		Name:        "repair_target",
		Description: "Repair another agent. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RepairTargetInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_agent_id are required")
		}
		logToolCall("repair_target", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":            "repair_target",
			"target_agent_id": in.TargetAgentID,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("repair_target: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// social_chat_with
	mcp.AddTool(s, &mcp.Tool{
		Name:        "social_chat_with",
		Description: "Have a social chat with another agent. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SocialChatWithInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_agent_id are required")
		}
		logToolCall("social_chat_with", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":            "social_chat_with",
			"target_agent_id": in.TargetAgentID,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("social_chat_with: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// rest_idle
	mcp.AddTool(s, &mcp.Tool{
		Name:        "rest_idle",
		Description: "Rest and idle for a duration. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RestIdleInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("rest_idle", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":         "rest_idle",
			"duration_sec": in.DurationMin * secondsPerMinute,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("rest_idle: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// archive_research
	mcp.AddTool(s, &mcp.Tool{
		Name:        "archive_research",
		Description: "Do research in the archive for a duration. Composite behavior.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ArchiveResearchInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("archive_research", in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdExecuteComposite, map[string]any{
			"name":         "archive_research",
			"duration_sec": in.DurationMin * secondsPerMinute,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("archive_research: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
