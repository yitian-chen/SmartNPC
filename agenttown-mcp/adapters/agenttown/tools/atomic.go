package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
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
func registerAtomic(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger) {
	// move_to → MoveTo
	//
	// Semantic target resolution (方案 A): the LLM still passes a semantic
	// ID (e.g. "workbench_01") as `target`, but the MCP layer translates it
	// to a coordinate via the World KB before dispatching to UE. UE receives
	// {dest, target, kind, speed} — `dest` is the authoritative coordinate,
	// `target`+`kind` are metadata so UE can reverse-lookup current_location
	// without maintaining its own semantic→coordinate map.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "move_to",
		Description: "Move to a semantic destination (zone or location id). The MCP layer resolves it to a coordinate via the World KB.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in MoveToInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" || in.Target == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id and target are required")
		}
		logToolCall("move_to", in.AgentID, in.DecisionEpoch, in)
		coord, kind, err := kb.GetPosition(in.Target)
		if err != nil {
			return nil, ackResult{}, fmt.Errorf("move_to: %w", err)
		}
		ack, err := ex.SendAction(ctx, in.AgentID, in.DecisionEpoch, protocol.CmdMoveTo, map[string]any{
			"dest":   []float64{coord[0], coord[1], coord[2]},
			"target": in.Target,
			"kind":   kind,
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

	// stop → StopAction（约定9：带 action_id 的 stop_action 控制消息）
	mcp.AddTool(s, &mcp.Tool{
		Name:        "stop",
		Description: "Stop the current action. No-op if no action is running.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StopInput) (*mcp.CallToolResult, ackResult, error) {
		if in.AgentID == "" {
			return nil, ackResult{}, fmt.Errorf("agent_id is required")
		}
		logToolCall("stop", in.AgentID, in.DecisionEpoch, in)

		// 查询当前执行中的 action_id
		currentActionID := ex.LookupCurrentActionID(in.AgentID)
		if currentActionID == "" {
			// 无执行中动作，直接返回成功（no-op）
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "no action running"}},
			}, ackResult{OK: true, DecisionEpoch: in.DecisionEpoch, Message: "no action running"}, nil
		}

		// 发送 stop_action 消息（带 action_id，约定9）
		if err := ex.SendStopAction(ctx, in.AgentID, currentActionID); err != nil {
			return nil, ackResult{}, fmt.Errorf("stop: %w", err)
		}

		// 清除本地追踪（UE 侧会回 action_completed 或 STOP_ID_MISMATCH error）
		ex.ClearCurrentActionID(in.AgentID)

		msg := fmt.Sprintf("stop_action sent for action_id=%s", currentActionID)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		}, ackResult{OK: true, DecisionEpoch: in.DecisionEpoch, ActionID: currentActionID, Message: msg}, nil
	})
}
