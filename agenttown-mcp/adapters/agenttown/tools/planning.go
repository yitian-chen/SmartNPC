package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// UpdatePlanInput is the request payload for the `update_plan` tool.
type UpdatePlanInput struct {
	Plan string `json:"plan" jsonschema:"new daily plan text (free-form)"`
}

// UpdatePlanOutput is the structured response of the `update_plan` tool.
type UpdatePlanOutput struct {
	OK      bool   `json:"ok"      jsonschema:"true if plan updated"`
	Message string `json:"message,omitempty"`
}

// registerPlanning installs update_plan. This tool is stateless from the
// MCP side — it logs the plan change and notifies Mock UE, but does not
// affect game state. Mock UE may persist the plan for cross-day context.
func registerPlanning(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "update_plan",
		Description: "Update the NPC's daily plan. Use when situation changes and " +
			"the remaining schedule should be replanned. Side-effect: WRITE — " +
			"replaces the in-memory plan; no physical state change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in UpdatePlanInput,
	) (*mcp.CallToolResult, UpdatePlanOutput, error) {
		if in.Plan == "" {
			return nil, UpdatePlanOutput{}, fmt.Errorf("plan is required")
		}
		logToolCall("update_plan", in)
		// Fire-and-forget to Mock UE; plan updates don't need a structured
		// response — the OK is enough for Hermes to continue.
		_, err := ue.Call(ctx, wsserver.ActionUpdatePlan, in)
		if err != nil {
			logger.Warn("update_plan ue call failed (non-fatal)", "err", err)
		}
		return nil, UpdatePlanOutput{
			OK:      true,
			Message: "plan updated",
		}, nil
	})
}
