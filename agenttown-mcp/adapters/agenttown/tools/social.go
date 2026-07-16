package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// SpeakInput is the request payload for the `speak` tool.
type SpeakInput struct {
	Text string `json:"text" jsonschema:"what to say"`
	To   string `json:"to,omitempty" jsonschema:"target NPC ID (empty = self-talk, still audible to nearby)"`
}

// SpeakOutput is the structured response of the `speak` tool.
type SpeakOutput struct {
	OK      bool   `json:"ok"      jsonschema:"true if speech broadcast"`
	Text    string `json:"text"    jsonschema:"echo of spoken text"`
	To      string `json:"to,omitempty"`
	Message string `json:"message,omitempty"`
}

// EmoteInput is the request payload for the `emote` tool.
type EmoteInput struct {
	Emotion string `json:"emotion" jsonschema:"wave | nod | shake_head | idle_think | worried"`
}

// EmoteOutput is the structured response of the `emote` tool.
type EmoteOutput struct {
	OK      bool   `json:"ok"      jsonschema:"true if emote played"`
	Emotion string `json:"emotion" jsonschema:"echo of emotion"`
	Message string `json:"message,omitempty"`
}

// registerSocial installs speak and emote.
func registerSocial(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "speak",
		Description: "Say something aloud. Nearby NPCs hear it. Side-effect: WRITE — " +
			"broadcasts to the local perception bubble. Use `to` to address a " +
			"specific NPC.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SpeakInput,
	) (*mcp.CallToolResult, SpeakOutput, error) {
		if in.Text == "" {
			return nil, SpeakOutput{}, fmt.Errorf("text is required")
		}
		logToolCall("speak", in)
		raw, err := ue.Call(ctx, wsserver.ActionSpeak, in)
		if err != nil {
			logger.Error("speak ue call failed", "err", err)
			return nil, SpeakOutput{}, fmt.Errorf("speak: %w", err)
		}
		var out SpeakOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "emote",
		Description: "Play an emotion animation (wave, nod, shake_head, idle_think, worried). " +
			"Side-effect: pure expression, no state change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in EmoteInput,
	) (*mcp.CallToolResult, EmoteOutput, error) {
		if in.Emotion == "" {
			return nil, EmoteOutput{}, fmt.Errorf("emotion is required")
		}
		logToolCall("emote", in)
		raw, err := ue.Call(ctx, wsserver.ActionEmote, in)
		if err != nil {
			logger.Error("emote ue call failed", "err", err)
			return nil, EmoteOutput{}, fmt.Errorf("emote: %w", err)
		}
		var out EmoteOutput
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &out)
		}
		out.OK = true
		return nil, out, nil
	})
}
