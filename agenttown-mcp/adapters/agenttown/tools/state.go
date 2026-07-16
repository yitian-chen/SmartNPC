package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// WaitInput is the request payload for the `wait` tool.
type WaitInput struct {
	Seconds float64 `json:"seconds" jsonschema:"how long to wait (real-seconds equivalent in game time)"`
}

// WaitOutput is the structured response of the `wait` tool.
type WaitOutput struct {
	OK       bool    `json:"ok"        jsonschema:"true if wait completed"`
	Seconds  float64 `json:"seconds"   jsonschema:"echo of wait duration"`
	Duration float64 `json:"duration_game_min" jsonschema:"game-minutes consumed"`
	Message  string  `json:"message,omitempty"`
}

// registerState installs wait.
func registerState(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "wait",
		Description: "Wait in place for a given duration. Side-effect: consumes " +
			"game time, slightly drains battery. Use to pace between actions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in WaitInput,
	) (*mcp.CallToolResult, WaitOutput, error) {
		if in.Seconds <= 0 {
			return nil, WaitOutput{}, fmt.Errorf("seconds must be positive")
		}
		logToolCall("wait", in)
		raw, err := ue.Call(ctx, wsserver.ActionWait, in)
		if err != nil {
			logger.Error("wait ue call failed", "err", err)
			return nil, WaitOutput{}, fmt.Errorf("wait: %w", err)
		}
		var out WaitOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})
}
