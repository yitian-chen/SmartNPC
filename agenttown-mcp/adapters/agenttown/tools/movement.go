package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// MoveToInput is the request payload for the `move_to` tool.
type MoveToInput struct {
	Target string `json:"target" jsonschema:"zone ID or location ID, e.g. \"main_workshop\", \"workbench_01\""`
}

// MoveToOutput is the structured response of the `move_to` tool.
type MoveToOutput struct {
	OK       bool      `json:"ok"        jsonschema:"true if movement accepted"`
	Target   string    `json:"target"    jsonschema:"echo of target"`
	Zone     string    `json:"zone"      jsonschema:"resulting zone after move"`
	Position []float64 `json:"position"  jsonschema:"resulting [x,y,z]"`
	Duration float64   `json:"duration_game_min" jsonschema:"game-minutes consumed"`
	Message  string    `json:"message,omitempty"`
}

// TurnToInput is the request payload for the `turn_to` tool.
type TurnToInput struct {
	Target string `json:"target" jsonschema:"entity ID to face"`
}

// TurnToOutput is the structured response of the `turn_to` tool.
type TurnToOutput struct {
	OK      bool    `json:"ok"       jsonschema:"true if accepted"`
	Target  string  `json:"target"   jsonschema:"echo of target"`
	Message string  `json:"message,omitempty"`
}

// registerMovement installs move_to and turn_to.
func registerMovement(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "move_to",
		Description: "Move to a zone or location (e.g. main_workshop, workbench_01). " +
			"Side-effect: WRITE — moves the NPC. Returns new zone/position and " +
			"game-minutes consumed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToInput,
	) (*mcp.CallToolResult, MoveToOutput, error) {
		if in.Target == "" {
			return nil, MoveToOutput{}, fmt.Errorf("target is required")
		}
		logToolCall("move_to", in)
		raw, err := ue.Call(ctx, wsserver.ActionMoveTo, in)
		if err != nil {
			logger.Error("move_to ue call failed", "err", err)
			return nil, MoveToOutput{}, fmt.Errorf("move_to: %w", err)
		}
		var out MoveToOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "turn_to",
		Description: "Face a specific entity (NPC, object). Side-effect: minimal. " +
			"Useful before speaking or interacting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TurnToInput,
	) (*mcp.CallToolResult, TurnToOutput, error) {
		if in.Target == "" {
			return nil, TurnToOutput{}, fmt.Errorf("target is required")
		}
		logToolCall("turn_to", in)
		raw, err := ue.Call(ctx, wsserver.ActionTurnTo, in)
		if err != nil {
			logger.Error("turn_to ue call failed", "err", err)
			return nil, TurnToOutput{}, fmt.Errorf("turn_to: %w", err)
		}
		var out TurnToOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})
}
