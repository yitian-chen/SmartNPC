package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmmetrics"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

// fakeLoopLLM 捕获 SendLoop 的参数（messages/tools/toolChoice/schemaName）
// 并返回预设响应，用于 agenticTurn 单测。errs 非空时前 len(errs) 次调用
// 依次返回对应错误（之后回落 resp/err），用于重试路径测试。
type fakeLoopLLM struct {
	resp         *llmtypes.Response
	err          error
	errs         []error
	streamDeltas []string // SendLoopStreaming 时依次回调 onDelta（模拟 token 到达）
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
	// errs 队列：前 N 次调用依次返回预设错误（模拟 429/4001 后恢复），
	// 耗尽后回落到 resp/err。
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return nil, e
	}
	return f.resp, f.err
}

func (f *fakeLoopLLM) SendLoopStreaming(_ context.Context, msgs []llmtypes.Message, tools []venus.Tool, toolChoice, schemaName string, _ []byte, onDelta func(string), onToolCall func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	f.calls++
	f.capturedMsgs = append([]llmtypes.Message(nil), msgs...)
	f.capturedTool = tools
	f.capturedTC = toolChoice
	f.capturedSc = schemaName
	for _, d := range f.streamDeltas {
		if onDelta != nil {
			onDelta(d)
		}
	}
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return nil, e
	}
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

// TestAgenticTurn_AppendsPendingToolsForTactical 验证战术轮（带 tool_calls）
// 成功后，assistant 之后紧跟 N 条占位 tool（result=pending），闭环
// "assistant(tool_calls) → tool" 协议约束。
func TestAgenticTurn_AppendsPendingToolsForTactical(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	resp := &llmtypes.Response{
		Status: "completed",
		ToolCalls: []llmtypes.ToolCall{
			{ID: "tc-1", Type: "function", Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}},
			{ID: "tc-2", Type: "function", Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_type":"zone"}`}},
		},
	}
	fake := &fakeLoopLLM{resp: resp}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"tactical", "分解请求", "required", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}

	hist := as.Conversation()
	// user + assistant + 2 占位 tool
	if len(hist) != 4 {
		t.Fatalf("history len = %d, want 4 (user+assistant+2 pending tools)", len(hist))
	}
	if hist[0].Role != "user" || hist[1].Role != "assistant" || len(hist[1].ToolCalls) != 2 {
		t.Fatalf("hist[0..1] = %+v, want user+assistant(2 tool_calls)", hist[:2])
	}
	for i := 2; i < 4; i++ {
		if hist[i].Role != "tool" || hist[i].Content != "result=pending" {
			t.Errorf("hist[%d] = %+v, want tool(result=pending)", i, hist[i])
		}
	}
	// 占位 tool 的 tool_call_id 与 assistant 的 tool_calls 对齐。
	if hist[2].ToolCallID != "tc-1" || hist[3].ToolCallID != "tc-2" {
		t.Errorf("pending tool ids = %q,%q, want tc-1,tc-2", hist[2].ToolCallID, hist[3].ToolCallID)
	}
}

// TestAgenticTurn_RateLimitRetrySucceeds 验证 429 限流（venus code 4029）
// 触发退避重试且重试后成功：历史正常追加、调用次数 = 失败次数 + 1。
// 2026-09-03 实测：5 NPC 战略轮同时撞限流（30/min），无重试直接全员
// 兜底"长椅作业"。
func TestAgenticTurn_RateLimitRetrySucceeds(t *testing.T) {
	orig := rateLimitBackoffBase
	rateLimitBackoffBase = time.Millisecond
	defer func() { rateLimitBackoffBase = orig }()

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{
		resp: makeLoopTextResponse(`{"accept":true}`),
		errs: []error{
			errors.New(`venus status 429: {"error":{"message":"当前使用的是公共模型服务, 并发有限; 当前的限流为: 30/min","type":"venus_error","code":"4029"}}`),
			errors.New(`venus status 429: {"error":{"message":"当前使用的是公共模型服务, 并发有限","type":"venus_error","code":"4029"}}`),
		},
	}
	ac := &agentContext{as: as, strategicHc: fake}

	resp, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"strategic", "规划请求", "none", "daily_plan", nil)
	if err != nil {
		t.Fatalf("agenticTurn should succeed after rate-limit retries: %v", err)
	}
	if resp == nil {
		t.Fatal("resp should be non-nil")
	}
	if fake.calls != 3 {
		t.Errorf("SendLoop calls = %d, want 3 (2 rate-limited + 1 success)", fake.calls)
	}
	// 成功后历史正常追加（user + assistant）。
	if hist := as.Conversation(); len(hist) != 2 {
		t.Errorf("history len = %d, want 2 (user+assistant)", len(hist))
	}
}

// TestAgenticTurn_RateLimitRetriesExhausted 验证持续限流时重试耗尽后
// 返回错误且历史保持不变（失败不留半截）。
func TestAgenticTurn_RateLimitRetriesExhausted(t *testing.T) {
	orig := rateLimitBackoffBase
	rateLimitBackoffBase = time.Millisecond
	defer func() { rateLimitBackoffBase = orig }()

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	rateErr := errors.New(`venus status 429: {"error":{"message":"并发有限","type":"venus_error","code":"4029"}}`)
	fake := &fakeLoopLLM{
		errs: []error{rateErr, rateErr, rateErr, rateErr, rateErr},
	}
	ac := &agentContext{as: as, strategicHc: fake}

	_, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"strategic", "规划请求", "none", "daily_plan", nil)
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	// 1 次首发 + 3 次重试 = 4 次调用。
	if fake.calls != 1+maxTacticalRetries {
		t.Errorf("SendLoop calls = %d, want %d", fake.calls, 1+maxTacticalRetries)
	}
	if hist := as.Conversation(); len(hist) != 0 {
		t.Errorf("history should stay empty on failure, got %d msgs", len(hist))
	}
}

// TestAgenticTurn_TimeoutErrorNoRetry 验证非限流/非 4001 错误（如超时）
// 不重试——避免浪费战术层 30s 超时预算。
func TestAgenticTurn_TimeoutErrorNoRetry(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{
		err: errors.New(`tactical llm: http do: context deadline exceeded`),
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	_, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if fake.calls != 1 {
		t.Errorf("SendLoop calls = %d, want 1 (timeout must not retry)", fake.calls)
	}
}

// TestAgenticTurn_StreamingTacticalCollectsTTFT 验证 --tactical-stream 开启时
// 战术层走 SendLoopStreaming，onDelta 回调采集 TTFT/ITL 样本写入 collector。
func TestAgenticTurn_StreamingTacticalCollectsTTFT(t *testing.T) {
	orig := tacticalStreamingEnabled
	tacticalStreamingEnabled = true
	defer func() { tacticalStreamingEnabled = orig }()
	llmMetricsCollector = llmmetrics.New() // reset 全局 collector 隔离测试

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{
		resp:         makeToolCallResponse([]llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}}}),
		streamDeltas: []string{"a", "b", "c"},
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}
	rep := llmMetricsCollector.Snapshot()
	lr := rep.Layers["tactical"]
	if lr == nil {
		t.Fatal("tactical layer missing from metrics")
	}
	if lr.TTFT == nil || lr.ITL == nil {
		t.Errorf("streaming tactical should collect TTFT/ITL, got %+v", lr)
	}
}

// TestAgenticTurn_NonStreamingSkipsTTFT 验证非流式（默认）战术轮只采集 E2E，
// 不采集 TTFT/ITL。
func TestAgenticTurn_NonStreamingSkipsTTFT(t *testing.T) {
	llmMetricsCollector = llmmetrics.New() // reset

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{
		resp: makeToolCallResponse([]llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}}}),
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	rep := llmMetricsCollector.Snapshot()
	lr := rep.Layers["tactical"]
	if lr == nil {
		t.Fatal("tactical layer missing from metrics")
	}
	if lr.TTFT != nil || lr.ITL != nil {
		t.Errorf("non-streaming should not collect TTFT/ITL: %+v", lr)
	}
	if lr.E2E.P50Ms <= 0 {
		t.Errorf("E2E should have a sample")
	}
}

// TestClassifyLLMError 验证错误分类映射（复用 isVenusErrorCode/isRateLimited）。
func TestClassifyLLMError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, llmmetrics.ErrSuccess},
		{venusErr4001, llmmetrics.ErrBadJSON4001},
		{errors.New(`venus status 429: {"error":{"code":"4029"}}`), llmmetrics.ErrRateLimited},
		{errors.New(`http do: context deadline exceeded`), llmmetrics.ErrTimeout},
		{errors.New(`venus status 500: internal`), llmmetrics.ErrHTTPError},
		{errors.New(`dial tcp: connection refused`), llmmetrics.ErrNetwork},
		{errors.New(`something unknown`), llmmetrics.ErrOther},
	}
	for _, c := range cases {
		if got := classifyLLMError(c.err); got != c.want {
			t.Errorf("classifyLLMError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
