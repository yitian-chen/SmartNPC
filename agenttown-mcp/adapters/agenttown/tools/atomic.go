package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// Atomic tools (§2.3) translate to their corresponding cmd. Each carries
// agent_id as the first parameter. Per the new 12-cmd system (2026-08-11),
// MoveTo no longer does MCP-side KB resolution — UE resolves target_type
// + target_id/target_position itself.

// GenericActInput — atomic: fallback action with inner thought + small behavior.
type GenericActInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Behavior      string `json:"behavior,omitempty" jsonschema:"small action category: look_around|groom|think (default idle)"`
	Thought       string `json:"thought" jsonschema:"what the NPC should do, spoken as inner thought"`
}

// MoveToInput — atomic: move to a target (agent/smart_object/zone/position).
// UE resolves the target itself; MCP just passes through.
type MoveToInput struct {
	AgentID        string   `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64    `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetType     string   `json:"target_type,omitempty" jsonschema:"target type: agent|smart_object|zone|position (default agent)"`
	TargetID       string   `json:"target_id,omitempty" jsonschema:"actor id when target_type is agent/smart_object/zone"`
	TargetPosition []float64 `json:"target_position,omitempty" jsonschema:"[x,y,z] coords when target_type is position"`
}

// TurnToInput — atomic: face a target (agent/smart_object/zone/position).
type TurnToInput struct {
	AgentID        string   `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64    `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetType     string   `json:"target_type,omitempty" jsonschema:"target type: agent|smart_object|zone|position (default agent)"`
	TargetID       string   `json:"target_id,omitempty" jsonschema:"actor id when target_type is agent/smart_object/zone"`
	TargetPosition []float64 `json:"target_position,omitempty" jsonschema:"[x,y,z] coords when target_type is position"`
}

// SpeakInput — atomic: say something.
type SpeakInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Content       string `json:"content"  jsonschema:"what to say"`
}

// EmoteInput — atomic: express an emotion.
type EmoteInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Emotion       string `json:"emotion"  jsonschema:"emotion: happy|sad|angry|neutral"`
}

// InteractInput — atomic: interact with a smart object.
type InteractInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	SmartObject   string `json:"smart_object" jsonschema:"smart object id from the world_kb"`
	Interaction   string `json:"interaction"  jsonschema:"verb from the object's available_interactions"`
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
// Note: stop does not translate to a CmdStop action_command; it sends the
// stop_action control message (TypeStopAction). RequiredCmd is "" so
// ReconcileTools never removes it based on capability_registry state.
type StopInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
}

// registerAtomic installs the atomic-behavior tools.
func registerAtomic(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger) {
	// generic_act → GenericAct
	mcp.AddTool(s, &mcp.Tool{
		Name:        "generic_act",
		Description: "Fallback bridging action: speaks an inner thought and plays a small behavior. Use only when no specific action fits.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GenericActInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Thought == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and thought are required")
		}
		logToolCall("generic_act", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{
			"thought": in.Thought,
		}
		if in.Behavior != "" {
			params["behavior"] = in.Behavior
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdGenericAct, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("generic_act: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// move_to → MoveTo
	// UE resolves target_type + target_id/target_position itself.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "move_to",
		Description: "Move to a target (agent / smart_object / zone / position). UE resolves the target.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		if in.TargetID == "" && len(in.TargetPosition) == 0 {
			return nil, ackResult{}, fmt.Errorf("either target_id or target_position is required")
		}
		logToolCall("move_to", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{}
		if in.TargetType != "" {
			params["target_type"] = in.TargetType
		}
		if in.TargetID != "" {
			params["target_id"] = in.TargetID
		}
		if len(in.TargetPosition) > 0 {
			params["target_position"] = in.TargetPosition
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdMoveTo, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// turn_to → TurnTo
	mcp.AddTool(s, &mcp.Tool{
		Name:        "turn_to",
		Description: "Face a target (agent / smart_object / zone / position). Does not move.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TurnToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		if in.TargetID == "" && len(in.TargetPosition) == 0 {
			return nil, ackResult{}, fmt.Errorf("either target_id or target_position is required")
		}
		logToolCall("turn_to", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{}
		if in.TargetType != "" {
			params["target_type"] = in.TargetType
		}
		if in.TargetID != "" {
			params["target_id"] = in.TargetID
		}
		if len(in.TargetPosition) > 0 {
			params["target_position"] = in.TargetPosition
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdTurnTo, params)
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
			"content": in.Content,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("speak: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// emote → Emote
	mcp.AddTool(s, &mcp.Tool{
		Name:        "emote",
		Description: "Express an emotion (happy|sad|angry|neutral).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in EmoteInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Emotion == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and emotion are required")
		}
		logToolCall("emote", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdEmote, map[string]any{
			"emotion": in.Emotion,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("emote: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// interact → InteractSmartObject
	mcp.AddTool(s, &mcp.Tool{
		Name:        "interact",
		Description: "Interact with a smart object using a verb from its available_interactions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in InteractInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.SmartObject == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, smart_object and interaction are required")
		}
		logToolCall("interact", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdInteractSmartObject, map[string]any{
			"smart_object": in.SmartObject,
			"interaction":  in.Interaction,
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
			// NPC 正在执行长动作时，UE 会拒绝 Wait（disruptive guard）。
			// 此时"等待"已经是隐式的——NPC 在忙，时间自然会走。返回成功而非
			// 错误，避免 LLM 把 rejected 当成需要重试的失败而反复调用 wait，
			// 每次重试都多耗一轮 LLM 上下文。把拒绝原因原样回传，让 LLM 知道
			// NPC 当前在忙什么、还剩多久。
			if msg := err.Error(); strings.Contains(msg, "busy with") {
				return nil, ackResult{
					OK:            true,
					DecisionEpoch: in.DecisionEpoch,
					Message:       "wait implicit: " + msg,
				}, nil
			}
			return nil, ackResult{}, fmt.Errorf("wait: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// scan_area → 请求 UE 立即回吐一次 perception_update（P1 恢复）。
	// 异步：handler 只触发扫描，不阻塞等待结果。下一个 perception_update
	// 会自然到达并触发 observePerception → 反应层评估。scanID 供日志关联。
	mcp.AddTool(s, &mcp.Tool{
		Name:        "scan_area",
		Description: "Request an immediate perception update (look around now).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ScanAreaInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("scan_area", in.AgentID, in.DecisionEpoch, in)
		scanID := "scan_" + uuid.NewString()[:12]
		if err := ex.RequestScan(ctx, in.AgentID, scanID); err != nil {
			return nil, ackResult{}, fmt.Errorf("scan_area: %w", err)
		}
		return nil, ackResult{
			OK:            true,
			DecisionEpoch: in.DecisionEpoch,
			Message:       "scan requested, perception will arrive next cycle",
		}, nil
	})

	// stop → 发送 stop_action 停止当前在途 action（P1 恢复）。
	// actionID 为空时由 Executor 查 agentContext.currentActionID。
	// 无在途 action 时 no-op，返回 OK。不依赖任何 Cmd* 常量。
	mcp.AddTool(s, &mcp.Tool{
		Name:        "stop",
		Description: "Stop the current action. No-op if no action is running.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StopInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("stop", in.AgentID, in.DecisionEpoch, in)
		if err := ex.SendStopAction(in.AgentID, ""); err != nil {
			return nil, ackResult{}, fmt.Errorf("stop: %w", err)
		}
		return nil, ackResult{
			OK:            true,
			DecisionEpoch: in.DecisionEpoch,
			Message:       "stop_action sent (or no-op if no in-flight action)",
		}, nil
	})
}
