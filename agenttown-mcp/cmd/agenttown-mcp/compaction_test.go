package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

func mkRound() []llmtypes.Message {
	return []llmtypes.Message{
		{Role: "user", Content: "tactical prompt"},
		{Role: "assistant", ToolCalls: []llmtypes.ToolCall{{ID: "c1", Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{}`}}}},
		{Role: "tool", Content: "result=pending", ToolCallID: "c1"},
		{Role: "tool", Content: "result=pending", ToolCallID: "c2"},
	}
}

func mkRounds(n int) []llmtypes.Message {
	var out []llmtypes.Message
	for i := 0; i < n; i++ {
		out = append(out, mkRound()...)
	}
	return out
}

func countAssistant(msgs []llmtypes.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func TestSplitTailRounds_NoopWhenBelowLimit(t *testing.T) {
	history := mkRounds(2)
	evict, tail := splitTailRounds(history, 4)
	if len(evict) != 0 {
		t.Fatalf("evict len = %d, want 0", len(evict))
	}
	if len(tail) != len(history) {
		t.Fatalf("tail len = %d, want %d", len(tail), len(history))
	}
}

func TestSplitTailRounds_KeepsLastNRounds(t *testing.T) {
	history := mkRounds(5)
	evict, tail := splitTailRounds(history, 2)
	if len(evict) == 0 || len(tail) == 0 {
		t.Fatalf("evict=%d tail=%d, both should be non-empty", len(evict), len(tail))
	}
	// 尾部起点必须是 assistant（不能以孤儿 tool 开头，否则破坏协议配对）。
	if tail[0].Role != "assistant" {
		t.Fatalf("tail[0].Role = %q, want assistant", tail[0].Role)
	}
	if got := countAssistant(tail); got != 2 {
		t.Fatalf("assistants in tail = %d, want 2", got)
	}
	// 尾部首个 assistant 的 tool 占位仍在尾部（配对未拆断）。
	if tail[1].Role != "tool" || tail[1].ToolCallID != "c1" {
		t.Fatalf("tail[1] = %+v, want tool c1", tail[1])
	}
}

func TestSplitTailRounds_NonPositiveKeepsAll(t *testing.T) {
	history := mkRounds(3)
	evict, tail := splitTailRounds(history, 0)
	if len(evict) != 0 || len(tail) != len(history) {
		t.Fatalf("rounds=0: evict=%d tail=%d, want 0/%d", len(evict), len(tail), len(history))
	}
}

func TestSplitTailRounds_Empty(t *testing.T) {
	evict, tail := splitTailRounds(nil, 4)
	if len(evict) != 0 || len(tail) != 0 {
		t.Fatalf("empty: evict=%d tail=%d, want 0/0", len(evict), len(tail))
	}
}

func TestFormatConversationForSummary(t *testing.T) {
	msgs := []llmtypes.Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", ToolCalls: []llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"x"}`}}}},
		{Role: "tool", Content: "result=pending", ToolCallID: "c1"},
	}
	got := formatConversationForSummary(msgs)
	for _, want := range []string{"用户：你好", "助手调用工具 speak(", `{"content":"x"}`, "工具结果[c1]：result=pending"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatConversationForSummary missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatConversationForSummary_Empty(t *testing.T) {
	if got := formatConversationForSummary(nil); got != "（空）" {
		t.Fatalf("empty = %q, want （空）", got)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	system := "你好" // 2 wide = 2
	tools := []venus.Tool{{
		Function: venus.ToolFunction{
			Name:        "speak",  // 5 narrow → 2
			Description: "说话",     // 2 wide = 2
			Parameters:  json.RawMessage(`{}`), // 2 narrow → 1
		},
	}}
	history := []llmtypes.Message{{Role: "user", Content: "你好"}} // 2
	user := "你好"                                                 // 2

	got := estimateInputTokens(system, tools, "", history, user)
	want := 1 + 4 + 0 + 1 + 1 // 7
	if got != want {
		t.Fatalf("estimateInputTokens = %d, want %d", got, want)
	}
}
