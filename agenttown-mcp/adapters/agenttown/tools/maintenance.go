package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// ChargeAtInput is the request payload for the `charge_at` tool.
type ChargeAtInput struct {
	StationID   string  `json:"station_id"   jsonschema:"charging station ID, e.g. charging_station_01"`
	DurationMin float64 `json:"duration_min" jsonschema:"charge duration in game-minutes"`
}

// ChargeAtOutput is the structured response of the `charge_at` tool.
type ChargeAtOutput struct {
	OK         bool    `json:"ok"          jsonschema:"true if charge accepted"`
	StationID  string  `json:"station_id"  jsonschema:"echo of station_id"`
	Duration   float64 `json:"duration_game_min" jsonschema:"game-minutes consumed"`
	EnergyGain float64 `json:"energy_gain" jsonschema:"battery gained (%)"`
	Energy     float64 `json:"energy"      jsonschema:"new battery level (%)"`
	Message    string  `json:"message,omitempty"`
}

// SelfCheckInput is the request payload for the `self_check` tool.
type SelfCheckInput struct{}

// SelfCheckOutput is the structured response of the `self_check` tool.
type SelfCheckOutput struct {
	OK        bool    `json:"ok"        jsonschema:"true if check completed"`
	Energy    float64 `json:"energy"    jsonschema:"battery level (%)"`
	Fatigue   float64 `json:"fatigue"   jsonschema:"fatigue level"`
	WearLevel float64 `json:"wear_level,omitempty" jsonschema:"joint wear (0-100)"`
	Message   string  `json:"message,omitempty"`
}

// registerMaintenance installs charge_at and self_check.
func registerMaintenance(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "charge_at",
		Description: "Charge at a charging station for a given duration. " +
			"Side-effect: WRITE — restores battery. Returns energy gained and " +
			"new battery level.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChargeAtInput,
	) (*mcp.CallToolResult, ChargeAtOutput, error) {
		if in.StationID == "" {
			return nil, ChargeAtOutput{}, fmt.Errorf("station_id is required")
		}
		if in.DurationMin <= 0 {
			return nil, ChargeAtOutput{}, fmt.Errorf("duration_min must be positive")
		}
		logToolCall("charge_at", in)
		raw, err := ue.Call(ctx, wsserver.ActionChargeAt, in)
		if err != nil {
			logger.Error("charge_at ue call failed", "err", err)
			return nil, ChargeAtOutput{}, fmt.Errorf("charge_at: %w", err)
		}
		var out ChargeAtOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "self_check",
		Description: "Run a self-diagnostic. Side-effect: READ — only consumes time. " +
			"Returns current battery, fatigue, and joint wear levels.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SelfCheckInput,
	) (*mcp.CallToolResult, SelfCheckOutput, error) {
		logToolCall("self_check", in)
		raw, err := ue.Call(ctx, wsserver.ActionSelfCheck, in)
		if err != nil {
			logger.Error("self_check ue call failed", "err", err)
			return nil, SelfCheckOutput{}, fmt.Errorf("self_check: %w", err)
		}
		var out SelfCheckOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})
}
