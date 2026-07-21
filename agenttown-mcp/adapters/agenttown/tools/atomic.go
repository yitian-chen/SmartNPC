package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// Atomic tools (§6.4) translate to their corresponding cmd. Each carries
// agent_id as the first parameter. Semantic targets (e.g. "工作台") are
// resolved to coordinates by Mock UE (which owns the world), so the Agent
// never touches coordinates.

// MoveToInput — atomic: move to a semantic target.
type MoveToInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Target        string `json:"target"   jsonschema:"semantic destination: zone id or location id, e.g. main_workshop, workbench_01"`
}

// TurnToInput — atomic: face a target.
type TurnToInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Target        string `json:"target"   jsonschema:"entity id to face"`
}

// SpeakInput — atomic: say something.
type SpeakInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Content       string `json:"content"  jsonschema:"what to say"`
	Target        string `json:"target,omitempty" jsonschema:"target agent id (empty = to nearby)"`
}

// EmoteInput — atomic: express an emotion.
type EmoteInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Emotion       string `json:"emotion"  jsonschema:"emotion: happy|sad|worried|..."`
	Mode          string `json:"mode,omitempty" jsonschema:"oneshot (play once) or sustained (hold until changed); default oneshot"`
}

// InteractInput — atomic: interact with a smart object.
type InteractInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	ObjectID      string `json:"object_id" jsonschema:"smart object id, e.g. workbench_01"`
	Action        string `json:"action"    jsonschema:"verb from the object's available_actions"`
}

// WaitInput — atomic: wait in place.
type WaitInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	DurationSec   float64 `json:"duration_sec" jsonschema:"wait duration in seconds"`
}

// ScanAreaInput — atomic: request an immediate perception snapshot.
type ScanAreaInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
}

// StopInput — atomic: stop the current action.
type StopInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
}

// registerAtomic installs the atomic-behavior tools.
func registerAtomic(s *mcp.Server, ex Executor, logger *slog.Logger) {
	// move_to → MoveTo
	mcp.AddTool(s, &mcp.Tool{
		Name:        "move_to",
		Description: "Move to a semantic destination (zone or location id). The world resolves coordinates.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Target == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target are required")
		}
		logToolCall("move_to", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdMoveTo, map[string]any{
			"target": in.Target,
			"speed":  "walk",
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// turn_to → TurnTo
	mcp.AddTool(s, &mcp.Tool{
		Name:        "turn_to",
		Description: "Face a specific entity. Useful before speaking or interacting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TurnToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Target == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target are required")
		}
		logToolCall("turn_to", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdTurnTo, map[string]any{
			"target": in.Target,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("turn_to: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// speak → Speak
	mcp.AddTool(s, &mcp.Tool{
		Name:        "speak",
		Description: "Say something aloud. Nearby agents hear it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SpeakInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Content == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and content are required")
		}
		logToolCall("speak", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdSpeak, map[string]any{
			"content":   in.Content,
			"target":    in.Target,
			"audio_url": nil,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("speak: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// emote → Emote
	mcp.AddTool(s, &mcp.Tool{
		Name:        "emote",
		Description: "Express an emotion. mode=oneshot plays once; mode=sustained holds until changed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in EmoteInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Emotion == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and emotion are required")
		}
		mode := in.Mode
		if mode == "" {
			mode = "oneshot"
		}
		logToolCall("emote", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdEmote, map[string]any{
			"emotion": in.Emotion,
			"mode":    mode,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("emote: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// interact → InteractSmartObject
	mcp.AddTool(s, &mcp.Tool{
		Name:        "interact",
		Description: "Interact with a smart object using a verb from its available_actions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in InteractInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.ObjectID == "" || in.Action == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, object_id and action are required")
		}
		logToolCall("interact", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdInteractSmartObject, map[string]any{
			"object_id": in.ObjectID,
			"action":    in.Action,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("interact: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// wait → Wait
	mcp.AddTool(s, &mcp.Tool{
		Name:        "wait",
		Description: "Wait in place for a duration (seconds).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WaitInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		if in.DurationSec <= 0 {
			return nil, ackResult{}, fmt.Errorf("duration_sec must be positive")
		}
		logToolCall("wait", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdWait, map[string]any{
			"duration_sec": in.DurationSec,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("wait: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// scan_area → immediate perception request (not a cmd)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "scan_area",
		Description: "Request an immediate perception update (look around now).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ScanAreaInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("scan_area", in.AgentID, in.DecisionEpoch, in)
		if err := ex.RequestScan(ctx, in.AgentID, in.DecisionEpoch); err != nil {
			return nil, ackResult{}, fmt.Errorf("scan_area: %w", err)
		}
		return nil, ackResult{OK: true, DecisionEpoch: in.DecisionEpoch, Message: "perception requested"}, nil
	})

	// stop → Stop
	mcp.AddTool(s, &mcp.Tool{
		Name:        "stop",
		Description: "Stop the current action.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StopInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("stop", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdStop, map[string]any{})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("stop: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})
}
