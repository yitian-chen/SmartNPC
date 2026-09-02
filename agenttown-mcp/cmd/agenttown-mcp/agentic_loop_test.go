package main

import (
	"context"
	"errors"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

// fakeLoopLLM 捕获 SendLoop 的参数（messages/tools/toolChoice/schemaName）
// 并返回预设响应，用于 agenticTurn 单测。
type fakeLoopLLM struct {
	resp         *llmtypes.Response
	err          error
	capturedMsgs []llmtypes.Message
	capturedTool []venus.Tool
	capturedTC   string
	capturedSc   string
	resetCount   int
	calls        int
}

func (f *fakeLoopLLM) SendWithSummary(_ context.Context, _, _ string, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendStreaming(_ context.Context, _, _ string, _ func(string)) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendWithSchema(_ context.Context, _, _, _ string, _ []byte, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendWithSummaryTools(_ context.Context, _, _ string, _ []venus.Tool) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendStreamingTools(_ context.Context, _, _ string, _ []venus.Tool, _ func(string), _ func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendMessagesTools(_ context.Context, _ []llmtypes.Message, _ []venus.Tool) (*llmtypes.Response, error) {
	return f.resp, f.err
}
func (f *fakeLoopLLM) SendLoop(_ context.Context, msgs []llmtypes.Message, tools []venus.Tool, toolChoice, schemaName string, _ []byte) (*llmtypes.Response, error) {
	f.calls++
	f.capturedMsgs = append([]llmtypes.Message(nil), msgs...)
	f.capturedTool = tools
	f.capturedTC = toolChoice
	f.capturedSc = schemaName
	return f.resp, f.err
}
func (f *fakeLoopLLM) ResetSession() { f.resetCount++ }

func makeLoopTextResponse(text string) *llmtypes.Response {
	return &llmtypes.Response{
		Status: "completed",
		Output: []llmtypes.Block{{Type: "message", Role: "assistant", Content: []llmtypes.Content{{Type: "output_text", Text: text}}}},
	}
}

// TestAgenticTurn_AppendsUserAndAssistant 验证成功轮次把 user+assistant 两条
// 消息追加进当天 loop 历史，且请求形态为 [system, ...历史, user]。
func TestAgenticTurn_AppendsUserAndAssistant(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{resp: makeLoopTextResponse(`{"accept":true}`)}
	ac := &agentContext{as: as, tacticalHc: fake}

	resp, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"dialogue", "user内容A", "none", "", nil)
	if err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	if resp == nil {
		t.Fatal("resp should be non-nil")
	}

	// 历史：user + assistant 各一条。
	hist := as.Conversation()
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (user+assistant)", len(hist))
	}
	if hist[0].Role != "user" || hist[0].Content != "user内容A" {
		t.Errorf("hist[0] = %+v, want user/user内容A", hist[0])
	}
	if hist[1].Role != "assistant" || hist[1].Content != `{"accept":true}` {
		t.Errorf("hist[1] = %+v, want assistant/{\"accept\":true}", hist[1])
	}

	// 请求形态：[system, user]（历史初始为空）。
	if len(fake.capturedMsgs) != 2 {
		t.Fatalf("request messages len = %d, want 2", len(fake.capturedMsgs))
	}
	if fake.capturedMsgs[0].Role != "system" || fake.capturedMsgs[0].Content == "" {
		t.Errorf("messages[0] should be non-empty system, got %+v", fake.capturedMsgs[0])
	}
	if fake.capturedMsgs[1].Content != "user内容A" {
		t.Errorf("messages[1] = %+v, want user内容A", fake.capturedMsgs[1])
	}
	if fake.capturedTC != "none" {
		t.Errorf("tool_choice = %q, want none", fake.capturedTC)
	}
	if fake.resetCount != 1 {
		t.Errorf("resetCount = %d, want 1", fake.resetCount)
	}

	// 第二轮：历史携带第一轮的 user+assistant。
	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"dialogue", "user内容B", "none", "", nil); err != nil {
		t.Fatalf("agenticTurn 2nd: %v", err)
	}
	// 第二次请求 = [system, u1, a1, u2]。
	if len(fake.capturedMsgs) != 4 {
		t.Fatalf("2nd request messages len = %d, want 4", len(fake.capturedMsgs))
	}
	if fake.capturedMsgs[3].Content != "user内容B" {
		t.Errorf("messages[3] = %+v, want user内容B", fake.capturedMsgs[3])
	}
	// 历史累计 4 条。
	if hist := as.Conversation(); len(hist) != 4 {
		t.Fatalf("history len after 2 turns = %d, want 4", len(hist))
	}
}

// TestAgenticTurn_FailureLeavesHistoryUnchanged 验证失败轮次不污染历史。
func TestAgenticTurn_FailureLeavesHistoryUnchanged(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{err: errors.New("network down")}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"strategic", "规划请求", "none", "daily_plan", []byte("{}")); err == nil {
		t.Fatal("expected error")
	}
	if hist := as.Conversation(); len(hist) != 0 {
		t.Errorf("history should be empty after failure, got %d entries", len(hist))
	}
	if fake.resetCount != 0 {
		t.Errorf("resetCount = %d, want 0 on failure", fake.resetCount)
	}
}

// TestAgenticTurn_SchemaPassThrough 验证战略轮的 schema 名透传给 SendLoop。
func TestAgenticTurn_SchemaPassThrough(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{resp: makeLoopTextResponse("[]")}
	ac := &agentContext{as: as, strategicHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"strategic", "规划请求", "none", "daily_plan", []byte(`{"type":"array"}`)); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	if fake.capturedSc != "daily_plan" {
		t.Errorf("schemaName = %q, want daily_plan", fake.capturedSc)
	}
	if fake.capturedTC != "none" {
		t.Errorf("tool_choice = %q, want none", fake.capturedTC)
	}
}
