package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// WorkAssembleInput is the request payload for the `work_assemble` tool.
type WorkAssembleInput struct {
	Target      string  `json:"target"            jsonschema:"workbench ID, e.g. workbench_01"`
	DurationMin float64 `json:"duration_min"      jsonschema:"work duration in game-minutes"`
}

// WorkAssembleOutput is the structured response of the `work_assemble` tool.
type WorkAssembleOutput struct {
	OK         bool    `json:"ok"          jsonschema:"true if work accepted"`
	Target     string  `json:"target"      jsonschema:"echo of target workbench"`
	Duration   float64 `json:"duration_game_min" jsonschema:"game-minutes consumed"`
	EnergyUsed float64 `json:"energy_used" jsonschema:"battery consumed (%)"`
	Fatigue    float64 `json:"fatigue"     jsonschema:"new fatigue level"`
	Message    string  `json:"message,omitempty"`
}

// InteractWithInput is the request payload for the `interact_with` tool.
type InteractWithInput struct {
	ObjectID string `json:"object_id" jsonschema:"smart object ID, e.g. workbench_01"`
	Action   string `json:"action"    jsonschema:"verb from the object's available_actions list"`
}

// InteractWithOutput is the structured response of the `interact_with` tool.
type InteractWithOutput struct {
	OK       bool    `json:"ok"        jsonschema:"true if interaction accepted"`
	ObjectID string  `json:"object_id" jsonschema:"echo of object_id"`
	Action   string  `json:"action"    jsonschema:"echo of action verb"`
	Duration float64 `json:"duration_game_min" jsonschema:"game-minutes consumed"`
	Message  string  `json:"message,omitempty"`
}

// registerWork installs work_assemble and interact_with.
func registerWork(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "work_assemble",
		Description: "Assemble parts at a workbench for a given duration. " +
			"Side-effect: WRITE — consumes energy, increases fatigue. Returns " +
			"game-minutes consumed and new fatigue level.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WorkAssembleInput,
	) (*mcp.CallToolResult, WorkAssembleOutput, error) {
		if in.Target == "" {
			return nil, WorkAssembleOutput{}, fmt.Errorf("target is required")
		}
		if in.DurationMin <= 0 {
			return nil, WorkAssembleOutput{}, fmt.Errorf("duration_min must be positive")
		}
		logToolCall("work_assemble", in)
		raw, err := ue.Call(ctx, wsserver.ActionWorkAssemble, in)
		if err != nil {
			logger.Error("work_assemble ue call failed", "err", err)
			return nil, WorkAssembleOutput{}, fmt.Errorf("work_assemble: %w", err)
		}
		var out WorkAssembleOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "interact_with",
		Description: "Interact with a smart object using a verb from its available_actions " +
			"list (e.g. inspect, repair). Side-effect: depends on verb.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in InteractWithInput,
	) (*mcp.CallToolResult, InteractWithOutput, error) {
		if in.ObjectID == "" {
			return nil, InteractWithOutput{}, fmt.Errorf("object_id is required")
		}
		if in.Action == "" {
			return nil, InteractWithOutput{}, fmt.Errorf("action is required")
		}
		logToolCall("interact_with", in)
		raw, err := ue.Call(ctx, wsserver.ActionInteractWith, in)
		if err != nil {
			logger.Error("interact_with ue call failed", "err", err)
			return nil, InteractWithOutput{}, fmt.Errorf("interact_with: %w", err)
		}
		var out InteractWithOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})
}
