package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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
	streamDeltas []string             // SendLoopStreaming 时依次回调 onDelta（模拟 token 到达）
	resps        []*llmtypes.Response // 按次响应队列：非空时依次弹出（耗尽回落 resp/err）
	capturedMsgs []llmtypes.Message
	allMsgs      [][]llmtypes.Message // 每次调用的 messages 快照（按调用序）
	capturedTool []venus.Tool
	capturedTC   string
	capturedSc   string
	resetCount   int
	calls        atomic.Int64
	gate         chan struct{} // 非-nil 时 SendLoop 阻塞至 close（并发时序测试用）
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
	if f.gate != nil {
		<-f.gate
	}
	f.calls.Add(1)
	f.capturedMsgs = append([]llmtypes.Message(nil), msgs...)
	f.allMsgs = append(f.allMsgs, append([]llmtypes.Message(nil), msgs...))
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
	// resps 队列：前 N 次调用依次返回预设响应（模拟空结果后恢复）。
	if len(f.resps) > 0 {
		r := f.resps[0]
		f.resps = f.resps[1:]
		return r, nil
	}
	return f.resp, f.err
}

func (f *fakeLoopLLM) SendLoopStreaming(_ context.Context, msgs []llmtypes.Message, tools []venus.Tool, toolChoice, schemaName string, _ []byte, onDelta func(string), onToolCall func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	f.calls.Add(1)
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
	if fake.calls.Load() != 3 {
		t.Errorf("SendLoop calls = %d, want 3 (2 rate-limited + 1 success)", fake.calls.Load())
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
	if fake.calls.Load() != 1+maxTacticalRetries {
		t.Errorf("SendLoop calls = %d, want %d", fake.calls.Load(), 1+maxTacticalRetries)
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
	if fake.calls.Load() != 1 {
		t.Errorf("SendLoop calls = %d, want 1 (timeout must not retry)", fake.calls.Load())
	}
}

// TestAgenticTurn_EmptyCompletionRetrySucceeds 验证空完成（后端过载返回空流，
// venus.ErrEmptyCompletion）触发立即重试且重试后成功：调用次数 = 失败 + 1。
func TestAgenticTurn_EmptyCompletionRetrySucceeds(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{
		resp: speakToolCallResp(),
		errs: []error{venus.ErrEmptyCompletion},
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	resp, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err != nil {
		t.Fatalf("agenticTurn should succeed after empty-completion retry: %v", err)
	}
	if resp == nil {
		t.Fatal("resp should be non-nil")
	}
	if fake.calls.Load() != 2 {
		t.Errorf("SendLoop calls = %d, want 2 (1 empty completion + 1 success)", fake.calls.Load())
	}
	// 成功后历史正常追加（user + assistant）。
	if hist := as.Conversation(); len(hist) != 2 {
		t.Errorf("history len = %d, want 2 (user+assistant)", len(hist))
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
		resp:         speakToolCallResp(),
		streamDeltas: []string{"a", "b", "c"},
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls.Load())
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
		resp: speakToolCallResp(),
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
		{venus.ErrEmptyCompletion, llmmetrics.ErrEmptyCompletion},
		{fmt.Errorf("wrapped: %w", venus.ErrEmptyCompletion), llmmetrics.ErrEmptyCompletion},
		{errors.New(`venus status 429: {"error":{"code":"4029"}}`), llmmetrics.ErrRateLimited},
		{errors.New(`http do: context deadline exceeded`), llmmetrics.ErrTimeout},
		{errors.New(`venus status 500: internal`), llmmetrics.ErrHTTPError},
		{errors.New(`dial tcp: connection refused`), llmmetrics.ErrNetwork},
		{errors.New(`http do: Post "http://x/v1/chat/completions": EOF`), llmmetrics.ErrNetwork},
		{errors.New(`something unknown`), llmmetrics.ErrOther},
	}
	for _, c := range cases {
		if got := classifyLLMError(c.err); got != c.want {
			t.Errorf("classifyLLMError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestAgenticTurn_EmptyResultRetries 验证战术层空结果（tool_choice=required 但
// 全是 speak）触发重试；重试耗尽后返回错误并计入 empty_result。
func TestAgenticTurn_EmptyResultRetries(t *testing.T) {
	llmMetricsCollector = llmmetrics.New() // reset

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	// 一直返回 speak-only：重试耗尽后应返回错误。
	fake := &fakeLoopLLM{
		resp: makeToolCallResponse([]llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}}}),
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	_, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err == nil {
		t.Fatal("expected empty-result error after retries exhausted")
	}
	if fake.calls.Load() != 1+maxTacticalRetries {
		t.Errorf("SendLoop calls = %d, want %d (1 initial + 3 retries)", fake.calls.Load(), 1+maxTacticalRetries)
	}
	// 历史不应追加（失败不留半截）。
	if hist := as.Conversation(); len(hist) != 0 {
		t.Errorf("history should stay empty on empty-result failure, got %d msgs", len(hist))
	}
	// 错误分布应计入 empty_result。
	rep := llmMetricsCollector.Snapshot()
	lr := rep.Layers["tactical"]
	if lr == nil || lr.ErrorDist[llmmetrics.ErrEmptyResult] != 1 {
		t.Errorf("error_dist should record empty_result=1, got %+v", rep.Layers["tactical"])
	}
}

// TestAgenticTurn_FilteredToSpeakOnlyRetries 验证"过滤后只剩 speak"（非 speak
// 的 tool_calls 全被 filterValidActions 过滤）也被判为空结果：重试 + 计 empty_result。
func TestAgenticTurn_FilteredToSpeakOnlyRetries(t *testing.T) {
	llmMetricsCollector = llmmetrics.New()

	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	// speak + scan_area：scan_area 会被 tacticalActionAvailable 过滤，只剩 speak。
	fake := &fakeLoopLLM{
		resp: makeToolCallResponse([]llmtypes.ToolCall{
			{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}},
			{Function: llmtypes.ToolFunction{Name: "scan_area", Arguments: `{}`}},
		}),
	}
	ac := &agentContext{as: as, tacticalHc: fake}

	_, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err == nil {
		t.Fatal("expected empty-result error for filtered-to-speak-only")
	}
	rep := llmMetricsCollector.Snapshot()
	lr := rep.Layers["tactical"]
	if lr == nil || lr.ErrorDist[llmmetrics.ErrEmptyResult] != 1 {
		t.Errorf("error_dist should record empty_result=1, got %+v", rep.Layers["tactical"])
	}
}

// makeLoopToolCallsResponse 构造带 tool_calls 的战术层响应。
func makeLoopToolCallsResponse(tcs ...llmtypes.ToolCall) *llmtypes.Response {
	return &llmtypes.Response{
		Status:    "completed",
		ToolCalls: tcs,
		Output: []llmtypes.Block{{Type: "message", Role: "assistant",
			Content: []llmtypes.Content{{Type: "output_text", Text: "[]"}}}},
	}
}

// TestAgenticTurn_EmptyResultCorrectiveRetry 验证空结果（speak-only）的
// 纠正性重试：第 1 次重试同体；第 2 次重试起在请求尾部追加纠正指令
// （瞬态 user 消息，不落历史）。背景：speak_to 单动作化后对话历史的
// few-shot 把模型持续推向"只输出一条宣告独白"（2026-10-16 复现 6/6
// speak-only，H-01 连续 6 轮 refill 失败呆站），同体重试救不回来。
func TestAgenticTurn_EmptyResultCorrectiveRetry(t *testing.T) {
	speakOnly := makeLoopToolCallsResponse(llmtypes.ToolCall{
		Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"说定了就动身。"}`},
	})
	valid := makeLoopToolCallsResponse(
		llmtypes.ToolCall{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"走起。"}`}},
		llmtypes.ToolCall{Function: llmtypes.ToolFunction{Name: "InteractSmartObject",
			Arguments: `{"duration":1800,"interaction":"run","semantic_group":"treadmill"}`}},
	)
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{resps: []*llmtypes.Response{speakOnly, speakOnly, valid}}
	ac := &agentContext{as: as, tacticalHc: fake}

	resp, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err != nil {
		t.Fatalf("agenticTurn should succeed after corrective retry: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("final resp tool_calls = %d, want 2", len(resp.ToolCalls))
	}
	if got := fake.calls.Load(); got != 3 {
		t.Fatalf("SendLoop calls = %d, want 3 (2 empty + 1 corrective success)", got)
	}
	if len(fake.allMsgs) != 3 {
		t.Fatalf("captured %d calls, want 3", len(fake.allMsgs))
	}
	// 前两次（首发 + 同体重试）不带纠正指令。
	for i := 0; i < 2; i++ {
		last := fake.allMsgs[i][len(fake.allMsgs[i])-1]
		if last.Content == emptyResultCorrection {
			t.Errorf("call %d must not carry the correction (same-body retry)", i+1)
		}
	}
	// 第三次（纠正性重试）末尾追加纠正指令。
	last := fake.allMsgs[2][len(fake.allMsgs[2])-1]
	if last.Role != "user" || last.Content != emptyResultCorrection {
		t.Errorf("call 3 must append the correction as the last user message, got role=%q content=%.80s",
			last.Role, last.Content)
	}
	// 成功后历史只有 user+assistant 两条——纠正指令是瞬态，不落历史。
	if hist := as.Conversation(); len(hist) != 2 {
		t.Errorf("history len = %d, want 2 (correction must not persist)", len(hist))
	}
}

// TestAgenticTurn_EmptyResultCorrectiveRetriesExhausted 验证纠正性重试
// 耗尽后返回错误、历史不变（失败不留半截），且最后两次请求都带纠正指令。
func TestAgenticTurn_EmptyResultCorrectiveRetriesExhausted(t *testing.T) {
	speakOnly := makeLoopToolCallsResponse(llmtypes.ToolCall{
		Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"宣告。"}`},
	})
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{resp: speakOnly}
	ac := &agentContext{as: as, tacticalHc: fake}

	_, err := ac.agenticTurn(context.Background(), fake, nil, nil, slog.Default(), "H-01",
		"tactical", "分解请求", "required", "", nil)
	if err == nil {
		t.Fatal("expected error after corrective retries exhausted")
	}
	if got := fake.calls.Load(); got != 1+maxTacticalRetries {
		t.Errorf("SendLoop calls = %d, want %d", got, 1+maxTacticalRetries)
	}
	// 第 2 次重试起（第 3、4 次调用）都带纠正指令。
	for i := 2; i < len(fake.allMsgs); i++ {
		last := fake.allMsgs[i][len(fake.allMsgs[i])-1]
		if last.Content != emptyResultCorrection {
			t.Errorf("call %d must carry the correction", i+1)
		}
	}
	if hist := as.Conversation(); len(hist) != 0 {
		t.Errorf("history len = %d, want 0 (failure leaves no half-turns)", len(hist))
	}
}
