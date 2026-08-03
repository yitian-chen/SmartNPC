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
// agent_id as the first parameter. Per 约定13, static targets are resolved
// to coordinates by the MCP layer (via World KB) before dispatch; dynamic
// targets (target_agent_id) are passed through for UE-side Actor lookup.

// MoveToLocationInput — atomic: move to a static coordinate.
// The MCP layer resolves the semantic target to a coordinate via the
// World KB before dispatching. UE receives {dest, speed}.
type MoveToLocationInput struct {
	AgentID       string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Target        string  `json:"target" jsonschema:"semantic destination: zone id or object id from the world_kb"`
	Speed         string  `json:"speed,omitempty" jsonschema:"walk or run (default walk)"`
}

// MoveToAgentInput — atomic: follow a dynamic agent target.
// UE resolves target_agent_id to an Actor at runtime and follows it.
type MoveToAgentInput struct {
	AgentID        string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID  string  `json:"target_agent_id" jsonschema:"the agent to follow"`
	Speed          string  `json:"speed,omitempty" jsonschema:"walk or run (default walk)"`
	StopDistance   float64 `json:"stop_distance,omitempty" jsonschema:"stopping distance in cm"`
	KeepFollowing  bool    `json:"keep_following,omitempty" jsonschema:"whether to keep following if the target moves"`
}

// TurnToInput — atomic: face a target agent or direction.
type TurnToInput struct {
	AgentID        string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetAgentID  string  `json:"target_agent_id,omitempty" jsonschema:"agent id to face (mutually exclusive with direction)"`
	Direction      []float64 `json:"direction,omitempty" jsonschema:"target heading vector [dx,dy,dz] (mutually exclusive with target_agent_id)"`
}

// SpeakInput — atomic: say something.
type SpeakInput struct {
	AgentID        string  `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch  int64   `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	Content        string  `json:"content"  jsonschema:"what to say"`
	TargetAgentID  string  `json:"target_agent_id,omitempty" jsonschema:"target agent id (empty = public speech)"`
	AudioURL       string  `json:"audio_url,omitempty" jsonschema:"TTS audio URL (empty = subtitle only)"`
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
	AgentID         string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch   int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	TargetObjectID  string `json:"target_object_id" jsonschema:"smart object id from the world_kb"`
	Interaction     string `json:"interaction"    jsonschema:"verb from the object's available_interactions"`
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

// PlayMontageInput — atomic: play a registered montage.
type PlayMontageInput struct {
	AgentID       string `json:"agent_id" jsonschema:"the NPC's id"`
	DecisionEpoch int64  `json:"decision_epoch" jsonschema:"required epoch from the current decision_context"`
	MontageID     string `json:"montage_id" jsonschema:"registered montage name"`
	WaitFinish    bool   `json:"wait_finish,omitempty" jsonschema:"whether to wait for playback to finish (default true)"`
}

// registerAtomic installs the atomic-behavior tools.
func registerAtomic(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger) {
	// move_to_location → MoveToLocation
	//
	// Static target resolution (约定13): the LLM passes a semantic ID
	// (e.g. "workbench_01") as `target`, and the MCP layer translates it
	// to a coordinate via the World KB before dispatching to UE. UE
	// receives {dest, speed} — dest is the authoritative coordinate.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "move_to_location",
		Description: "Move to a semantic destination (zone or object id). The MCP layer resolves it to a coordinate via the World KB.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToLocationInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Target == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target are required")
		}
		logToolCall("move_to_location", in.AgentID, in.DecisionEpoch, in)
		coord, _, err := kb.GetPosition(in.Target)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to_location: %w", err)
		}
		speed := in.Speed
		if speed == "" {
			speed = "walk"
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdMoveToLocation, map[string]any{
			"dest":  []float64{coord[0], coord[1], coord[2]},
			"speed": speed,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to_location: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// move_to_agent → MoveToAgent
	//
	// Dynamic target (约定13): UE resolves target_agent_id to an Actor
	// at runtime via AgentBridgeClient.FindAgentActor and follows it.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "move_to_agent",
		Description: "Follow a dynamic agent target. UE resolves the target at runtime.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToAgentInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetAgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target_agent_id are required")
		}
		logToolCall("move_to_agent", in.AgentID, in.DecisionEpoch, in)
		speed := in.Speed
		if speed == "" {
			speed = "walk"
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdMoveToAgent, map[string]any{
			"target_agent_id": in.TargetAgentID,
			"speed":           speed,
			"stop_distance":   in.StopDistance,
			"keep_following":  in.KeepFollowing,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to_agent: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// turn_to → TurnTo
	mcp.AddTool(s, &mcp.Tool{
		Name:        "turn_to",
		Description: "Face a specific agent or heading direction. Useful before speaking or interacting.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in TurnToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		if in.TargetAgentID == "" && len(in.Direction) == 0 {
			return nil, ackResult{}, fmt.Errorf("either target_agent_id or direction is required")
		}
		logToolCall("turn_to", in.AgentID, in.DecisionEpoch, in)
		params := map[string]any{}
		if in.TargetAgentID != "" {
			params["target_agent_id"] = in.TargetAgentID
		}
		if len(in.Direction) > 0 {
			params["direction"] = in.Direction
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdTurnTo, params)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("turn_to: %w", err)
		}
		return nil, buildAckResult(ack, in.DecisionEpoch), nil
	})

	// play_montage → PlayMontage
	mcp.AddTool(s, &mcp.Tool{
		Name:        "play_montage",
		Description: "Play a registered montage animation.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PlayMontageInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.MontageID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and montage_id are required")
		}
		logToolCall("play_montage", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdPlayMontage, map[string]any{
			"montage_id":  in.MontageID,
			"wait_finish": in.WaitFinish,
		})
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("play_montage: %w", err)
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
			"content":         in.Content,
			"target_agent_id": in.TargetAgentID,
			"audio_url":       in.AudioURL,
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
		Description: "Interact with a smart object using a verb from its available_interactions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in InteractInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.TargetObjectID == "" || in.Interaction == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id, target_object_id and interaction are required")
		}
		logToolCall("interact", in.AgentID, in.DecisionEpoch, in)
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdInteractSmartObject, map[string]any{
			"target_object_id": in.TargetObjectID,
			"interaction":      in.Interaction,
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
			// NPC 正在执行长动作时，Mock UE 会拒绝 Wait（disruptive guard）。
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
