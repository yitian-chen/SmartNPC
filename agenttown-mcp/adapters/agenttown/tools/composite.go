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
// 6 composite cmds (2026-08-11 + Phase 2 Module C): WorkShift /
// ChargeAtStation / SelfMaintenance / RestAtResidence / SurfInternet /
// SocialChat. The first 5 share the params schema semantic_group +
// interaction; SocialChat uses target_agent_id + content (dialogue is
// not a queueable Smart Object action).
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
// the authoritative source for real UE5. SocialChat does NOT carry
// auto_queue — it targets another NPC, not a queueable Smart Object.

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

// SocialChatInput — composite: proactively initiate dialogue with another NPC.
// Per docs/AgentTown_Dialogue_Design.md §3.1, this is A's action_command that
// opens a session. params are target_agent_id + content (NOT semantic_group/
// interaction). No auto_queue — dialogue targets an NPC, not a queueable
// Smart Object. UE opens a DialogueSession(Inviting), preempts B, and sends
// chat_invite to B; MCP then handles chat_invite_rsp / chat_turn exchange.
type SocialChatInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID string `json:"target_agent_id" jsonschema:"the target NPC's id to talk to"`
	Content       string `json:"content" jsonschema:"opening line / 开场白"`
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

	// social_chat — Phase 2 Module C: proactive NPC-to-NPC dialogue.
	// Sends CmdSocialChat with target_agent_id + content. No auto_queue
	// (dialogue is not a queueable Smart Object action). UE opens a
	// DialogueSession, preempts B, and sends chat_invite to B; the
	// subsequent chat_invite_rsp / chat_turn exchange is handled by the
	// dialogue runner in cmd/agenttown-mcp/dialogue.go, not by this tool.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "social_chat",
		Description: "Proactively walk to another NPC and start a dialogue. Composite behavior — runs MoveTo+TurnTo+WaitDialogue until the conversation ends.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SocialChatInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" || in.Content == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, target_agent_id and content are required")
		}
		if in.AgentID == in.TargetAgentID {
			return nil, ackResult{}, fmt.Errorf("social_chat: target_agent_id must differ from agent_id")
		}
		logToolCall("social_chat", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSocialChat, map[string]any{
			"target_agent_id": in.TargetAgentID,
			"content":         in.Content,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("social_chat: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
