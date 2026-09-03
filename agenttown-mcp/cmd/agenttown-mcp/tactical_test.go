package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// loadTestKB 加载项目自带的 assets/world_kb.yaml 供 mapTacticalAction 测试用。
func loadTestKB(t *testing.T) *worldkb.KB {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "assets", "world_kb.yaml"))
	if err != nil {
		t.Fatalf("resolve kb path: %v", err)
	}
	kb, err := worldkb.Load(p)
	if err != nil {
		t.Fatalf("load kb: %v", err)
	}
	return kb
}

// ─── parseToolCalls ────────────────────────────────────────

func TestParseToolCalls_Basic(t *testing.T) {
	tcs := []llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"先去车间再装配"}`}},
		{Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_type":"zone","target_id":"main_workshop"}`}},
		{Function: llmtypes.ToolFunction{Name: "work_shift", Arguments: `{"semantic_group":"workbench_01","interaction":"assemble"}`}},
	}
	actions := parseToolCalls(tcs, nil, "")
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3", len(actions))
	}
	if actions[0].Action != "speak" || actions[0].Params["content"] != "先去车间再装配" {
		t.Errorf("actions[0]=%+v, want speak with content", actions[0])
	}
	if actions[1].Action != "move_to" || actions[2].Action != "work_shift" {
		t.Errorf("actions=%+v, want [speak move_to work_shift]", actions)
	}
}

func TestParseToolCalls_Empty(t *testing.T) {
	if got := parseToolCalls(nil, nil, ""); len(got) != 0 {
		t.Errorf("got %d actions, want 0", len(got))
	}
}

func TestParseToolCalls_EmptyName(t *testing.T) {
	tcs := []llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "", Arguments: `{}`}},
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"hi"}`}},
	}
	actions := parseToolCalls(tcs, nil, "")
	if len(actions) != 1 || actions[0].Action != "speak" {
		t.Errorf("actions=%+v, want [speak] (empty name skipped)", actions)
	}
}

func TestParseToolCalls_InvalidArgumentsSkipped(t *testing.T) {
	tcs := []llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `not-json`}},
		{Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_type":"zone"}`}},
	}
	actions := parseToolCalls(tcs, nil, "")
	if len(actions) != 1 || actions[0].Action != "move_to" {
		t.Errorf("actions=%+v, want [move_to] (invalid arguments skipped)", actions)
	}
}

func TestParseToolCalls_FiltersInvalidTool(t *testing.T) {
	tcs := []llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "scan_area", Arguments: `{}`}},
		{Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_type":"zone"}`}},
	}
	actions := parseToolCalls(tcs, nil, "")
	if len(actions) != 1 || actions[0].Action != "move_to" {
		t.Errorf("actions=%+v, want [move_to] (scan_area filtered)", actions)
	}
}

func TestParseToolCalls_InteractAlias(t *testing.T) {
	tcs := []llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "interact", Arguments: `{"semantic_group":"sleep_pod","interaction":"meditate"}`}},
	}
	actions := parseToolCalls(tcs, nil, "")
	if len(actions) != 1 || actions[0].Action != "InteractSmartObject" {
		t.Errorf("actions=%+v, want [InteractSmartObject] (interact alias)", actions)
	}
}

// ─── mapTacticalAction ───────────────────────────────────────

func TestMapTacticalAction_Composite(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "work_shift", Params: map[string]any{"semantic_group": "workbench_01", "interaction": "assemble"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdWorkShift {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdWorkShift)
	}
	if params["semantic_group"] != "workbench_01" {
		t.Errorf("semantic_group=%v", params["semantic_group"])
	}
	if params["interaction"] != "assemble" {
		t.Errorf("interaction=%v, want assemble", params["interaction"])
	}
}

// TestMapTacticalAction_SocialChat verifies the Phase 2 Module C dialogue
// action maps to CmdSocialChat with target_agent_id + content params and
// NO auto_queue (dialogue targets an NPC, not a queueable Smart Object).
func TestMapTacticalAction_SocialChat(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "social_chat", Params: map[string]any{
		"target_agent_id": "H-02",
		"content":         "最近怎么样？",
	}}
	cmd, params, err := mapTacticalAction(pa, "H-01", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdSocialChat {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdSocialChat)
	}
	if params["target_agent_id"] != "H-02" {
		t.Errorf("target_agent_id=%v, want H-02", params["target_agent_id"])
	}
	if params["content"] != "最近怎么样？" {
		t.Errorf("content=%v", params["content"])
	}
	if _, has := params["auto_queue"]; has {
		t.Errorf("social_chat must NOT carry auto_queue (dialogue is not a queueable Smart Object action): %+v", params)
	}
	if _, has := params["semantic_group"]; has {
		t.Errorf("social_chat must NOT carry semantic_group: %+v", params)
	}
}

func TestMapTacticalAction_MoveToPassthrough(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "move_to", Params: map[string]any{"target_type": "zone", "target_id": "main_workshop"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdMoveTo {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdMoveTo)
	}
	// MoveTo 不再做 MCP 侧 KB 解析，params 直接透传给 UE
	if params["target_type"] != "zone" {
		t.Errorf("target_type=%v, want zone", params["target_type"])
	}
	if params["target_id"] != "main_workshop" {
		t.Errorf("target_id=%v, want main_workshop", params["target_id"])
	}
}

func TestMapTacticalAction_Speak(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "speak", Params: map[string]any{"content": "你好"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdSpeak {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdSpeak)
	}
	if params["content"] != "你好" {
		t.Errorf("content=%v", params["content"])
	}
}

func TestMapTacticalAction_Emote(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "emote", Params: map[string]any{"emotion": "happy"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdEmote {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdEmote)
	}
	if params["emotion"] != "happy" {
		t.Errorf("emotion=%v, want happy", params["emotion"])
	}
}

func TestMapTacticalAction_UnknownAction(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "fly_to", Params: map[string]any{}}
	if _, _, err := mapTacticalAction(pa, "", kb, nil); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// ─── selectCurrentGoal ───────────────────────────────────────

func TestSelectCurrentGoal_MatchSlot(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 车间装配\n12:00-13:00: 午餐"
	goal, slot, idx := selectCurrentGoal(plan, "09:30")
	if goal != "车间装配" {
		t.Errorf("goal=%q, want 车间装配", goal)
	}
	if slot != "08:00-12:00" {
		t.Errorf("slot=%q, want 08:00-12:00", slot)
	}
	if idx != 1 {
		t.Errorf("idx=%d, want 1", idx)
	}
}

func TestSelectCurrentGoal_NoMatch(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 装配"
	goal, slot, idx := selectCurrentGoal(plan, "04:00")
	if goal != "" || slot != "" || idx != -1 {
		t.Errorf("got goal=%q slot=%q idx=%d, want empty/-1", goal, slot, idx)
	}
}

func TestSelectCurrentGoal_EmptyPlan(t *testing.T) {
	goal, slot, idx := selectCurrentGoal("", "09:00")
	if goal != "" || slot != "" || idx != -1 {
		t.Errorf("got goal=%q slot=%q idx=%d, want empty/-1", goal, slot, idx)
	}
}

func TestSelectCurrentGoal_OvernightSlot(t *testing.T) {
	plan := "15:30-17:00: 收尾\n17:00-17:30: 日志\n17:30-06:00: 充电休息"
	// 19:30 在跨日时段 17:30-06:00 内
	goal, slot, idx := selectCurrentGoal(plan, "19:30")
	if goal != "充电休息" {
		t.Errorf("goal=%q, want 充电休息", goal)
	}
	if slot != "17:30-06:00" {
		t.Errorf("slot=%q, want 17:30-06:00", slot)
	}
	if idx != 2 {
		t.Errorf("idx=%d, want 2", idx)
	}
}

func TestSelectCurrentGoal_OvernightSlotEarlyMorning(t *testing.T) {
	plan := "17:30-06:00: 充电休息"
	// 03:00 在跨日时段的 [0,360) 部分
	goal, slot, _ := selectCurrentGoal(plan, "03:00")
	if goal != "充电休息" {
		t.Errorf("goal=%q, want 充电休息", goal)
	}
	if slot != "17:30-06:00" {
		t.Errorf("slot=%q, want 17:30-06:00", slot)
	}
}

// TestSelectCurrentGoal_PlanningWindowBlocked 验证 06:00-07:00 战略规划窗口
// 屏蔽战术层分解。即使 LLM 生成的夜间 slot 结束时间延到 07:00 之后
// （如 "22:00-07:00"），在该窗口内 selectCurrentGoal 也应返回空，防止
// 战术层反复分解夜间睡眠任务。
func TestSelectCurrentGoal_PlanningWindowBlocked(t *testing.T) {
	// 夜间 slot 故意写到 07:00（LLM 常见偏移），06:00-07:00 应被屏蔽
	plan := "07:00-12:00: 上午装配\n12:00-18:00: 下午工作\n22:00-07:00: 夜间休眠"
	for _, tod := range []string{"06:00", "06:30", "06:59"} {
		goal, slot, idx := selectCurrentGoal(plan, tod)
		if goal != "" || slot != "" || idx != -1 {
			t.Errorf("tod=%s: got goal=%q slot=%q idx=%d, want empty/-1 (planning window)", tod, goal, slot, idx)
		}
	}
}

// TestSelectCurrentGoal_PlanningWindowBoundary 验证屏蔽窗口的边界：
// 05:59（窗口前）夜间 slot 正常匹配，07:00（窗口后）首个活动 slot 正常匹配。
func TestSelectCurrentGoal_PlanningWindowBoundary(t *testing.T) {
	plan := "07:00-12:00: 上午装配\n22:00-06:00: 夜间休眠"
	// 05:59 在夜间 slot [22:00,06:00) 内，未被屏蔽
	goal, _, _ := selectCurrentGoal(plan, "05:59")
	if goal != "夜间休眠" {
		t.Errorf("tod=05:59: goal=%q, want 夜间休眠 (before planning window)", goal)
	}
	// 07:00 进入首个活动 slot，未被屏蔽
	goal, _, _ = selectCurrentGoal(plan, "07:00")
	if goal != "上午装配" {
		t.Errorf("tod=07:00: goal=%q, want 上午装配 (after planning window)", goal)
	}
}

// ─── generateTacticalPlan ────────────────────────────────────

func makeToolCallResponse(tcs []llmtypes.ToolCall) *llmtypes.Response {
	return &llmtypes.Response{
		Status:    "completed",
		Output:    []llmtypes.Block{{Type: "message", Role: "assistant", Content: []llmtypes.Content{{Type: "output_text", Text: ""}}}},
		ToolCalls: tcs,
	}
}

func TestGenerateTacticalPlan_HTTPError(t *testing.T) {
	tc := &fakeStrategicCaller{err: errors.New("network down")}
	actions, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error on HTTP failure")
	}
	if actions != nil {
		t.Errorf("got actions=%v, want nil", actions)
	}
	if tc.resetCalled {
		t.Error("ResetSession should not be called when SendWithSummary fails")
	}
}

func TestGenerateTacticalPlan_ValidResponse(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"先移动再装配"}`}},
		{Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_type":"zone","target_id":"main_workshop"}`}},
		{Function: llmtypes.ToolFunction{Name: "work_shift", Arguments: `{"semantic_group":"workbench_01","interaction":"assemble"}`}},
	})}
	actions, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3 (speak + move + work)", len(actions))
	}
	if actions[0].Action != "speak" {
		t.Errorf("actions[0]=%q, want speak", actions[0].Action)
	}
	if actions[0].Params["content"] != "先移动再装配" {
		t.Errorf("speak content=%v", actions[0].Params["content"])
	}
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful generation")
	}
}

func TestGenerateTacticalPlan_NoToolCalls(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeStrategicResponse("我今天打算去车间转转。")}
	if _, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error when no tool calls returned")
	}
}

func TestGenerateTacticalPlan_AllFiltered(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "scan_area", Arguments: `{}`}},
	})}
	if _, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error when all tool calls filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"开始"}`}},
	})}
	_, _ = generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "等待", "main_workshop", "09:00", "09:00-12:00", "", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
	}
}

// ─── venus 4001 重试 ────────────────────────────────────────

// sequenceCaller 按调用次数依次返回 seq[i] 的错误（nil 表示成功返回 resp），
// 用于验证 4001 重试次数。实现 llmClient 全部方法。
type sequenceCaller struct {
	seq        []error
	resp       *llmtypes.Response
	calls      int
	resetCount int
}

func (s *sequenceCaller) next() (*llmtypes.Response, error) {
	if s.calls >= len(s.seq) {
		s.calls++
		return nil, errors.New("unexpected call")
	}
	err := s.seq[s.calls]
	s.calls++
	if err == nil {
		return s.resp, nil
	}
	return nil, err
}

// tacticalCtxForTest 构造挂 fake 战术客户端的 agentContext（统一 agentic loop
// 测试用）：as 为全新 AgentState，tacticalHc 接 fake（sequenceCaller 或
// fakeStrategicCaller 均实现 llmClient）。
func tacticalCtxForTest(hc llmClient) *agentContext {
	return &agentContext{as: agentstate.New(), tacticalHc: hc}
}

func (s *sequenceCaller) SendWithSummary(_ context.Context, _, _ string, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendWithSummaryTools(_ context.Context, _, _ string, _ []venus.Tool) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendMessagesTools(_ context.Context, _ []llmtypes.Message, _ []venus.Tool) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendStreaming(_ context.Context, _, _ string, _ func(string)) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendStreamingTools(_ context.Context, _, _ string, _ []venus.Tool, _ func(string), _ func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendWithSchema(_ context.Context, _, _, _ string, _ []byte, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendLoop(_ context.Context, _ []llmtypes.Message, _ []venus.Tool, _, _ string, _ []byte) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) SendLoopStreaming(_ context.Context, _ []llmtypes.Message, _ []venus.Tool, _, _ string, _ []byte, _ func(string), _ func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	return s.next()
}
func (s *sequenceCaller) ResetSession() { s.resetCount++ }

// venusErr4001 模拟 venus 校验 tools JSON 失败的 500 响应（code 4001）。
var venusErr4001 = errors.New(`venus status 500: {"error":{"message":"Response generation failed: 1 validation error for list[FunctionDefinition]","type":"server_error","code":"4001"},"venusMarker":{"spanId":"test"}}`)

func TestGenerateTacticalPlan_RetryOn4001(t *testing.T) {
	// 前两次 4001，第三次成功 → 重试后成功，共调用 3 次（1 首调 + 2 重试）。
	tc := &sequenceCaller{
		seq:  []error{venusErr4001, venusErr4001, nil},
		resp: makeToolCallResponse([]llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"重试成功"}`}}}),
	}
	actions, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "", &protocol.PhysicalState{Energy: 80}, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if tc.calls != 3 {
		t.Fatalf("got %d calls, want 3 (1 initial + 2 retries)", tc.calls)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	if tc.resetCount != 1 {
		t.Errorf("resetCount=%d, want 1", tc.resetCount)
	}
}

func TestGenerateTacticalPlan_RetryExhausted(t *testing.T) {
	// 连续 4 次 4001 → 重试 3 次后仍失败，最终返回错误。
	tc := &sequenceCaller{seq: []error{venusErr4001, venusErr4001, venusErr4001, venusErr4001}}
	_, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if tc.calls != 4 {
		t.Fatalf("got %d calls, want 4 (1 initial + 3 retries)", tc.calls)
	}
}

func TestGenerateTacticalPlan_NoRetryOnNon4001(t *testing.T) {
	// 非 4001 错误（如超时/连接失败）不重试，仅调用 1 次。
	tc := &sequenceCaller{seq: []error{errors.New("http do: Post: context deadline exceeded")}}
	if _, err := generateTacticalPlan(context.Background(), tacticalCtxForTest(tc), "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if tc.calls != 1 {
		t.Fatalf("got %d calls, want 1 (no retry for non-4001)", tc.calls)
	}
}

// ─── 精简引用（compact）模式 ─────────────────────────────────

// lastUserPromptOf 取 SendLoop 捕获请求的末条 user 内容（[system,...历史,user]）。
func lastUserPromptOf(t *testing.T, msgs []llmtypes.Message) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("captured messages empty")
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	return last.Content
}

// speakToolCallResp 构造单个 speak 工具调用的成功响应（战术轮最小可用形态）。
func speakToolCallResp() *llmtypes.Response {
	return makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"开始"}`}},
	})
}

// TestGenerateTacticalPlan_FirstTurnFullSecondCompact 验证同一天同一计划：
// 首轮全量头（【全天日程】+完整规则）注入并置位标记，次轮精简（省略两块、
// 含引用行），且次轮请求的历史前缀携带首轮的全量 user。
func TestGenerateTacticalPlan_FirstTurnFullSecondCompact(t *testing.T) {
	plan := "07:00-09:00: 上午准备\n09:00-12:00: 车间装配"
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac := tacticalCtxForTest(fake)

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	first := lastUserPromptOf(t, fake.capturedMsgs)
	if !strings.Contains(first, "【全天日程】\n") || !strings.Contains(first, "推荐模式：工作段") {
		t.Errorf("first turn should be full form (schedule + full rules):\n%s", first)
	}
	if got := ac.as.TacticalHeaderPlan(); got != plan {
		t.Errorf("TacticalHeaderPlan after first turn = %q, want the plan", got)
	}

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "12:00", "12:00-14:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("second turn: %v", err)
	}
	second := lastUserPromptOf(t, fake.capturedMsgs)
	if strings.Contains(second, "推荐模式：工作段") {
		t.Errorf("second turn should omit the full rules:\n%s", second)
	}
	if !strings.Contains(second, "完整分解规则与全天日程见本日第一条战术分解指令") {
		t.Errorf("second turn should carry the reference line:\n%s", second)
	}
	// 次轮请求的历史前缀包含首轮全量 user（引用行指向的内容确实在历史里）。
	found := false
	for _, m := range fake.capturedMsgs[:len(fake.capturedMsgs)-1] {
		if m.Role == "user" && strings.Contains(m.Content, "【全天日程】\n") {
			found = true
		}
	}
	if !found {
		t.Error("second request history should carry the first turn's full user message")
	}
}

// TestGenerateTacticalPlan_FailureKeepsFullMode 验证 LLM 失败轮不置位标记
// （与 agenticTurn 失败不落历史对齐），重试成功后仍走全量形态。
func TestGenerateTacticalPlan_FailureKeepsFullMode(t *testing.T) {
	plan := "07:00-09:00: 上午准备\n09:00-12:00: 车间装配"
	fake := &fakeLoopLLM{errs: []error{errors.New("network down")}}
	ac := tacticalCtxForTest(fake)

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error on first turn")
	}
	if got := ac.as.TacticalHeaderPlan(); got != "" {
		t.Errorf("failed turn must not mark the header, got %q", got)
	}

	fake.errs = nil
	fake.resp = speakToolCallResp()
	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("retry turn: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	if !strings.Contains(user, "推荐模式：工作段") {
		t.Errorf("retry after failure should still be full form:\n%s", user)
	}
}

// TestGenerateTacticalPlan_EmptyDailyPlanNeverCompact 验证 /debug/schedule
// 路径（dailyPlan=""）永不精简——手动调试要确定性，且不置位标记。
func TestGenerateTacticalPlan_EmptyDailyPlanNeverCompact(t *testing.T) {
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac := tacticalCtxForTest(fake)

	for i := 0; i < 2; i++ {
		if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		user := lastUserPromptOf(t, fake.capturedMsgs)
		if !strings.Contains(user, "推荐模式：工作段") {
			t.Errorf("empty dailyPlan (debug path) turn %d must stay full form:\n%s", i+1, user)
		}
	}
	if got := ac.as.TacticalHeaderPlan(); got != "" {
		t.Errorf("debug path must not mark the header, got %q", got)
	}
}

// TestGenerateTacticalPlan_PlanChangeReinjectsFull 验证日内计划变化
// （未来反应层/事件触发的战略重规划）时标记比对不等 → 重新全量注入新计划。
func TestGenerateTacticalPlan_PlanChangeReinjectsFull(t *testing.T) {
	planA := "07:00-09:00: 上午准备\n09:00-12:00: 车间装配"
	planB := "07:00-09:00: 晨间维护\n09:00-12:00: 分拣作业"
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac := tacticalCtxForTest(fake)

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", planA, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "分拣", "logistics_hub", "09:00", "09:00-12:00", planB, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("plan-change turn: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	if !strings.Contains(user, "【全天日程】\n") || !strings.Contains(user, "分拣作业") {
		t.Errorf("plan change should re-inject the full header with the new plan:\n%s", user)
	}
	if got := ac.as.TacticalHeaderPlan(); got != planB {
		t.Errorf("TacticalHeaderPlan after plan change = %q, want planB", got)
	}
}

// TestGenerateTacticalPlan_ParseFailureStillSetsHeader 钉死置位时机：置位
// 在 agenticTurn 成功（err==nil）后、tool_calls 解析之前——即使解析失败
// 返回 err，全量头也已落进历史，标记已置位，下一轮走精简。
func TestGenerateTacticalPlan_ParseFailureStillSetsHeader(t *testing.T) {
	plan := "07:00-09:00: 上午准备\n09:00-12:00: 车间装配"
	fake := &fakeLoopLLM{resp: makeLoopTextResponse("我今天打算去车间转转。")}
	ac := tacticalCtxForTest(fake)

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error when no tool calls returned")
	}
	if got := ac.as.TacticalHeaderPlan(); got != plan {
		t.Errorf("header should be marked once the full user message is in history, got %q", got)
	}
	// 失败轮的 user 消息确实在历史里（agenticTurn 已追加）。
	if hist := ac.as.Conversation(); len(hist) == 0 {
		t.Error("history should carry the full user message after parse failure")
	}

	// 下一轮（同计划）走精简。
	fake.resp = speakToolCallResp()
	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", plan, nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("next turn: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	if strings.Contains(user, "推荐模式：工作段") {
		t.Errorf("turn after parse failure should be compact:\n%s", user)
	}
}

func TestIsVenusErrorCode(t *testing.T) {
	if !isVenusErrorCode(venusErr4001, "4001") {
		t.Error("expected 4001 match")
	}
	if isVenusErrorCode(venusErr4001, "4002") {
		t.Error("unexpected 4002 match")
	}
	if isVenusErrorCode(errors.New("http do: Post: context deadline exceeded"), "4001") {
		t.Error("non-venus error should not match")
	}
	if isVenusErrorCode(nil, "4001") {
		t.Error("nil error should not match")
	}
}

// ─── buildTacticalPrompt ─────────────────────────────────────

func TestBuildTacticalPrompt_NilPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: nil, Hint: "", AgentID: ""})
	if promptText == "" {
		t.Fatal("promptText should not be empty")
	}
	// nil physical 时注入默认物理状态（100/0/0/200 → 很高/精神饱满/良好），
	// 让 LLM 始终看到有效物理上下文（分段标签，非原始数值）
	if !strings.Contains(promptText, "物理状态") {
		t.Errorf("prompt should contain '物理状态' with default values for nil physical, got: %s", promptText)
	}
	if !strings.Contains(promptText, "电量 高") {
		t.Errorf("prompt should contain default band 电量 高 for nil physical, got: %s", promptText)
	}
	// slot 为空时不应有时长提示行
	if strings.Contains(promptText, "请让步骤总时长接近此时长") {
		t.Errorf("prompt should not contain slot duration hint when slot is empty, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_ZeroPhysical 验证全 0 物理状态（UE 已上报但值全 0）
// 也注入默认物理状态，与 nil physical 同等处理。
func TestBuildTacticalPrompt_ZeroPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{}, KB: nil, Hint: "", AgentID: ""})
	if !strings.Contains(promptText, "物理状态") {
		t.Errorf("prompt should contain '物理状态' with default values for all-zero physical, got: %s", promptText)
	}
	if !strings.Contains(promptText, "电量 高") {
		t.Errorf("prompt should contain default band 电量 高 for all-zero physical, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_WithPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "", AgentID: ""})
	// 数值以分段标签呈现：75→中等、30→精神饱满、5→良好
	if !strings.Contains(promptText, "电量 中等") {
		t.Errorf("prompt should contain '电量 中等' (75), got: %s", promptText)
	}
	if !strings.Contains(promptText, "疲劳 精神饱满") {
		t.Errorf("prompt should contain '疲劳 精神饱满' (30), got: %s", promptText)
	}
	// slot 有效时应包含时长提示
	if !strings.Contains(promptText, "当前时段 09:00-12:00，约 180 分钟") {
		t.Errorf("prompt should contain slot duration hint, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_InjectsKBContext(t *testing.T) {
	kb := loadTestKB(t)
	// KB 世界信息已迁至 system prompt（与战略层共享三模块）。
	promptText := prompt.BuildSharedSystemPrompt(kb, nil, "")
	// 应包含 KB 中所有 zone（assets/world_kb.yaml 当前是 7-zone 工业园区）
	for _, zID := range []string{"main_workshop", "central_plaza", "logistics_hub", "repair_bay", "residential_quarters", "archive_station", "recycling_yard"} {
		if !strings.Contains(promptText, zID) {
			t.Errorf("prompt should list zone %q, got: %s", zID, promptText)
		}
	}
	// 应包含所有 object 的 semantic_group（新 UE5 KB: charger/repair_table/sleep_pod/workbench）
	for _, sg := range []string{"workbench", "charger", "sleep_pod", "repair_table"} {
		if !strings.Contains(promptText, sg) {
			t.Errorf("prompt should list semantic_group %q, got: %s", sg, promptText)
		}
	}
	// 应包含设施详情段及交互动词
	if !strings.Contains(promptText, "设施详情") {
		t.Errorf("system prompt should contain '设施详情' section, got: %s", promptText)
	}
	if !strings.Contains(promptText, "assemble") || !strings.Contains(promptText, "charge") || !strings.Contains(promptText, "sleep") {
		t.Errorf("system prompt should list available interactions on objects, got: %s", promptText)
	}
	// 工具清单已迁至 user prompt 且仅从 registry 派生：nil actions 不出现。
	if strings.Contains(promptText, "（仅限以下") {
		t.Errorf("system prompt should not carry the tool list, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_ObjectStatusTemporarilyRemoved 验证【物体实时占用】
// 段已暂时移除：即使 ObjectStatus 非空、KB 存在，prompt 也不渲染该段。
// 恢复时删掉本测试并取消 BuildTactical 中 ObjectStatusContext 调用的注释。
func TestBuildTacticalPrompt_ObjectStatusTemporarilyRemoved(t *testing.T) {
	kb := loadTestKB(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"work":     {Total: 2, Idle: 1, Occupied: 1},
		"charging": {Total: 6, Idle: 6, Occupied: 0},
	}
	nearby := []protocol.NearbyObject{
		{ID: "WorkBench", Category: "work", State: "occupied"},
		{ID: "Charge-1", Category: "charging", State: "idle"},
	}
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:          "装配",
		Zone:          "main_workshop",
		TimeOfDay:     "09:00",
		Slot:          "09:00-12:00",
		Physical:      &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5},
		KB:            kb,
		Hint:          "",
		AgentID:       "",
		ObjectStatus:  status,
		NearbyObjects: nearby,
	})
	// 物体实时占用段暂时移除：不应出现段标题与段体。
	if strings.Contains(promptText, "物体实时占用") {
		t.Errorf("prompt should NOT contain '物体实时占用' (temporarily removed), got: %s", promptText)
	}
	if strings.Contains(promptText, "按 category 聚合") {
		t.Errorf("prompt should NOT render object status body, got: %s", promptText)
	}
	// 规则 2 的"日程不合理"引导仍保留（与物体占用段无关）。
	if !strings.Contains(prompt.TacticalRules, "半夜不睡觉而是跑步/工作") ||
		!strings.Contains(prompt.TacticalRules, "请下发更合理的动作") {
		t.Errorf("system prompt should guide LLM to avoid doomed occupancy actions")
	}
	if !strings.Contains(prompt.TacticalRules, "所有工种设备都可用 InteractSmartObject") {
		t.Error("system prompt should say InteractSmartObject works for any work device")
	}
}

// TestBuildTacticalPrompt_NilObjectStatusNoSection 验证 ObjectStatus 为空时
// 【物体实时占用】段整体省略，不污染 prompt（兼容 UE 未推送 object_status 的场景）。
// 注意：机制规则（提及"物体实时占用"字样）已移入 TacticalSystemPrompt，
// 用户消息里该词只在段实际渲染时出现；仍改用段体特征 "按 category 聚合" 判断。
func TestBuildTacticalPrompt_NilObjectStatusNoSection(t *testing.T) {
	kb := loadTestKB(t)
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:      "装配",
		Zone:      "main_workshop",
		TimeOfDay: "09:00",
		Slot:      "09:00-12:00",
		Physical:  &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5},
		KB:        kb,
		Hint:      "",
		AgentID:   "",
		// ObjectStatus / NearbyObjects 留空
	})
	if strings.Contains(promptText, "按 category 聚合") {
		t.Errorf("prompt should NOT render object status section body when ObjectStatus is nil, got: %s", promptText)
	}
	if strings.Contains(promptText, "你附近的实例状态") {
		t.Errorf("prompt should NOT render nearby instances section when NearbyObjects is nil, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_NilKB(t *testing.T) {
	// nil KB 时不应崩溃，也不应包含 KB 上下文段落
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: nil, Hint: "", AgentID: ""})
	// 不应出现 KB 段落标题（"可前往区域（..."、"可交互物体（..."）
	// 注意：示例 fallback 文本里会提到"上方可前往区域的 id"作为占位提示，
	// 这是引导文字而非 KB 内容，不应被此断言拦截——所以用更精确的段落标题匹配。
	if strings.Contains(promptText, "可前往区域（move_to") {
		t.Errorf("prompt should not contain '可前往区域' section when KB is nil, got: %s", promptText)
	}
	if strings.Contains(promptText, "可交互物体（interact") {
		t.Errorf("prompt should not contain '可交互物体' section when KB is nil, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_InjectsAgentRole 验证战术层 prompt 注入
// 【你的角色】段：传 kb + agentID="H-01" 后，prompt 应包含从
// buildAgentRoleContext 派生的角色画像（名字/职业/性格特质等）。
// 这是 C4 的核心——战术层分解动作时应体现 NPC 角色（如"老陈"的
// "沉稳"性格影响 action 选择与节奏），而非机械分解。
func TestBuildTacticalPrompt_InjectsAgentRole(t *testing.T) {
	kb := loadTestKB(t)
	// 角色画像已迁至 system prompt 的【人物背景】模块。
	promptText := prompt.BuildSharedSystemPrompt(kb, nil, "H-01")
	if !strings.Contains(promptText, "【人物背景】") {
		t.Errorf("prompt missing '【人物背景】' section header, got: %s", promptText)
	}
	for _, want := range []string{"老陈", "装配工人", "沉稳"} {
		if !strings.Contains(promptText, want) {
			t.Errorf("prompt missing role field %q, got: %s", want, promptText)
		}
	}
}

// TestBuildTacticalPrompt_NilKBNoRole 验证 kb==nil 时 prompt 不含
// 【你的角色】段（roleLine 降级为空串，prompt 中仅留空行）。
func TestBuildTacticalPrompt_NilKBNoRole(t *testing.T) {
	promptText := prompt.BuildSharedSystemPrompt(nil, nil, "")
	if strings.Contains(promptText, "【人物背景】\n") {
		t.Errorf("prompt should not contain '【人物背景】' when KB is nil, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_AgentNotFoundNoRole 验证 KB 存在但 agentID
// 不在 KB 中时也降级跳过【你的角色】段（buildAgentRoleContext 返回空串）。
func TestBuildTacticalPrompt_AgentNotFoundNoRole(t *testing.T) {
	kb := loadTestKB(t)
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: kb, Hint: "", AgentID: "NONEXISTENT-99"})
	if strings.Contains(promptText, "【你的角色】") {
		t.Errorf("prompt should not include '【你的角色】' for unknown agent, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_WithHint(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "fatigue=72 已突破警戒带，当前装配任务不合理", AgentID: ""})
	if !strings.Contains(promptText, "【上次中断原因】") {
		t.Errorf("prompt should contain '【上次中断原因】' when hint is non-empty, got: %s", promptText)
	}
	if !strings.Contains(promptText, "fatigue=72 已突破警戒带") {
		t.Errorf("prompt should contain the hint text, got: %s", promptText)
	}
	if !strings.Contains(promptText, "请据此调整本轮规划") {
		t.Errorf("prompt should contain adjustment guidance, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_NoHint(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "", AgentID: ""})
	if strings.Contains(promptText, "【上次中断原因】") {
		t.Errorf("prompt should not contain '【上次中断原因】' when hint is empty, got: %s", promptText)
	}
}

// ─── registry-aware tactical prompt / filtering ─────────────

func TestFilterValidActions_RegistryFiltersCmd(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		// CmdWorkShift / CmdChargeAtStation absent → composite tools filtered.
	})
	actions := []plannedAction{
		{Action: "move_to", Params: map[string]any{"target": "main_workshop"}},
		{Action: "work_shift", Params: map[string]any{"target": "workbench_01"}},
		{Action: "charge_at_station", Params: map[string]any{}},
	}
	got := filterValidActions(actions, reg, "H-01")
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1 (only move_to)", len(got))
	}
	if got[0].Action != "move_to" {
		t.Errorf("got action %q, want move_to", got[0].Action)
	}
}

func TestSlotDurationMinute(t *testing.T) {
	cases := []struct {
		slot string
		want int
	}{
		{"09:00-12:00", 180},
		{"06:00-07:00", 60},
		{"13:00-17:00", 240},
		{"18:00-22:00", 240},
		{"12:00-13:00", 60},
		// 跨午夜 slot：end <= start 时归一化到次日
		{"22:00-06:00", 480}, // 8 小时
		{"23:30-06:00", 390}, // 6.5 小时
		{"20:00-00:30", 270}, // 4.5 小时
		// 解析失败 / 非法
		{"", -1},
		{"09:00", -1},
		{"09:00-09:00", -1}, // end == start（零时长，非法）
		{"abc-xyz", -1},
	}
	for _, c := range cases {
		got := prompt.SlotDurationMinute(c.slot)
		if got != c.want {
			t.Errorf("prompt.SlotDurationMinute(%q) = %d, want %d", c.slot, got, c.want)
		}
	}
}

// TestSlotRangeMinute_CrossMidnight 验证跨午夜 slot 的起止解析。
func TestSlotRangeMinute_CrossMidnight(t *testing.T) {
	cases := []struct {
		slot      string
		wantStart int
		wantEnd   int
	}{
		{"22:00-06:00", 1320, 1800}, // end=360+1440=1800
		{"23:30-06:00", 1410, 1800},
		{"20:00-00:30", 1200, 1470}, // end=30+1440=1470
		// 非跨午夜不变
		{"09:00-12:00", 540, 720},
	}
	for _, c := range cases {
		s, e := prompt.SlotRangeMinute(c.slot)
		if s != c.wantStart || e != c.wantEnd {
			t.Errorf("prompt.SlotRangeMinute(%q) = (%d, %d), want (%d, %d)", c.slot, s, e, c.wantStart, c.wantEnd)
		}
	}
}

// TestSlotExpired_CrossMidnight 验证跨午夜 slot 的过期判断。
func TestSlotExpired_CrossMidnight(t *testing.T) {
	cases := []struct {
		name string
		slot string
		tod  string
		want bool
	}{
		{"cross-midnight in slot (before midnight)", "22:00-06:00", "23:30", false},
		{"cross-midnight in slot (after midnight)", "22:00-06:00", "01:00", false},
		{"cross-midnight in slot (near end)", "22:00-06:00", "05:59", false},
		{"cross-midnight expired (at end)", "22:00-06:00", "06:00", true},
		{"cross-midnight expired (after end)", "22:00-06:00", "06:30", true},
		{"cross-midnight before slot start", "22:00-06:00", "20:00", false}, // 20:00 < start 22:00，未进入
		// 非跨午夜
		{"normal in slot", "09:00-12:00", "10:00", false},
		{"normal expired", "09:00-12:00", "12:30", true},
		// 边界
		{"empty slot", "", "10:00", false},
		{"empty tod", "09:00-12:00", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := prompt.SlotExpired(c.slot, c.tod); got != c.want {
				t.Errorf("prompt.SlotExpired(%q, %q) = %v, want %v", c.slot, c.tod, got, c.want)
			}
		})
	}
}

func TestBuildSlotDurationHint_Remaining(t *testing.T) {
	cases := []struct {
		name      string
		slot      string
		timeOfDay string
		wantSub   string
		notWant   string
	}{
		{
			name:      "remaining less than total",
			slot:      "09:00-12:00",
			timeOfDay: "10:30",
			wantSub:   "剩余约 90 分钟（已过去 90 分钟）",
			notWant:   "约 180 分钟",
		},
		{
			name:      "slot expired",
			slot:      "09:00-12:00",
			timeOfDay: "12:30",
			wantSub:   "已过期",
			notWant:   "剩余约",
		},
		{
			name:      "timeOfDay before slot start",
			slot:      "09:00-12:00",
			timeOfDay: "08:00",
			wantSub:   "约 180 分钟",
			notWant:   "剩余约",
		},
		{
			name:      "timeOfDay equals slot start",
			slot:      "09:00-12:00",
			timeOfDay: "09:00",
			wantSub:   "约 180 分钟",
			notWant:   "剩余约",
		},
		{
			name:      "empty timeOfDay falls back to full duration",
			slot:      "09:00-12:00",
			timeOfDay: "",
			wantSub:   "约 180 分钟",
			notWant:   "剩余约",
		},
		{
			name:      "invalid timeOfDay falls back to full duration",
			slot:      "09:00-12:00",
			timeOfDay: "invalid",
			wantSub:   "约 180 分钟",
			notWant:   "剩余约",
		},
		{
			name:      "invalid slot returns empty",
			slot:      "abc-xyz",
			timeOfDay: "10:00",
			wantSub:   "",
			notWant:   "当前时段",
		},
		// 跨午夜 slot
		{
			name:      "cross-midnight slot mid-night remaining",
			slot:      "22:00-06:00",
			timeOfDay: "23:30", // 仍在上半夜
			wantSub:   "剩余约 390 分钟",
			notWant:   "约 480 分钟",
		},
		{
			name:      "cross-midnight slot after-midnight remaining",
			slot:      "22:00-06:00",
			timeOfDay: "01:00", // 已进入次日
			wantSub:   "剩余约 300 分钟",
			notWant:   "约 480 分钟",
		},
		{
			name:      "cross-midnight slot full duration before start",
			slot:      "22:00-06:00",
			timeOfDay: "20:00", // 在 slot 开始前
			wantSub:   "约 480 分钟",
			notWant:   "剩余约",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := prompt.SlotDurationHint(c.slot, c.timeOfDay)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("prompt.SlotDurationHint(%q, %q) = %q, want empty", c.slot, c.timeOfDay, got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("prompt.SlotDurationHint(%q, %q) = %q, want substring %q", c.slot, c.timeOfDay, got, c.wantSub)
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Errorf("prompt.SlotDurationHint(%q, %q) = %q, should NOT contain %q", c.slot, c.timeOfDay, got, c.notWant)
			}
		})
	}
}

// ─── 动态 cmd 派生（Phase 2） ────────────────────────────────

// TestTacticalActionAvailable_NewCmdAccepted verifies tacticalActionAvailable
// accepts a UE-pushed new cmd via registry lookup.
func TestTacticalActionAvailable_NewCmdAccepted(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: "WaveHand", Kind: "atomic"},
	})
	if !tacticalActionAvailable("wave_hand", "H-01", reg) {
		t.Error("wave_hand should be available when registry declares WaveHand")
	}
	if tacticalActionAvailable("fly_to", "H-01", reg) {
		t.Error("fly_to should not be available (not in registry)")
	}
}

// TestMapTacticalAction_NewCmdPassthrough verifies mapTacticalAction passes
// through params verbatim for a UE-pushed new cmd.
func TestMapTacticalAction_NewCmdPassthrough(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: "WaveHand", Kind: "atomic"},
	})
	pa := plannedAction{Action: "wave_hand", Params: map[string]any{"target_agent_id": "H-02"}}
	cmd, params, err := mapTacticalAction(pa, "H-01", nil, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "WaveHand" {
		t.Errorf("cmd=%q, want WaveHand", cmd)
	}
	if params["target_agent_id"] != "H-02" {
		t.Errorf("params=%v, want target_agent_id=H-02 passthrough", params)
	}
}

// TestMapTacticalAction_PassthroughStripsDuration 验证 passthrough 路径剔除
// duration——它是 MCP 侧控制字段（worker 按 game_time 定时打断消费），
// 不透传 UE。duration required 化（2026-09-03）后 LLM 会填，必须在此剥离。
func TestMapTacticalAction_PassthroughStripsDuration(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: "Exercise", Kind: "atomic", Params: []protocol.CapabilityParam{
			{Name: "exercise_type", Type: "enum", Required: true, EnumValues: []string{"stretch", "walk"}},
		}},
	})
	pa := plannedAction{Action: "exercise", Params: map[string]any{
		"exercise_type": "stretch",
		"duration":      float64(1800),
	}}
	cmd, params, err := mapTacticalAction(pa, "H-01", nil, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "Exercise" {
		t.Errorf("cmd=%q, want Exercise", cmd)
	}
	if _, ok := params["duration"]; ok {
		t.Errorf("params=%v, duration must be stripped before sending to UE", params)
	}
	if params["exercise_type"] != "stretch" {
		t.Errorf("params=%v, want exercise_type=stretch kept", params)
	}
}

// TestMapTacticalAction_NewCmdNilRegistryErrors verifies the default branch
// returns an error when registry is nil (backward compat — unknown action).
func TestMapTacticalAction_NewCmdNilRegistryErrors(t *testing.T) {
	pa := plannedAction{Action: "wave_hand", Params: map[string]any{}}
	if _, _, err := mapTacticalAction(pa, "", nil, nil); err == nil {
		t.Fatal("expected error for unknown action with nil registry")
	}
}

// ─── tacticalToolsFromRegistry (function calling) ────────────

func TestTacticalToolsFromRegistry_BuildsTools(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{
			Cmd:         protocol.CmdWorkShift,
			Kind:        "composite",
			Description: "去指定设施执行工作",
			Params: []protocol.CapabilityParam{
				{Name: "semantic_group", Type: "string", Description: "设施语义组", Required: true},
				{Name: "interaction", Type: "string", Description: "交互类型", Required: true},
			},
		},
		{
			Cmd:  protocol.CmdMoveTo,
			Kind: "atomic",
			Params: []protocol.CapabilityParam{
				{Name: "target_type", Type: "enum", Description: "目标类型", Required: true, EnumValues: []string{"agent", "zone"}},
			},
		},
	})
	// Register(system) 会自动注入 social_chat（MCP 侧对话工具），所以总数
	// = MoveTo + WorkShift + SocialChat。
	got := tacticalToolsFromRegistry(reg, "H-01")
	byName := map[string]venus.Tool{}
	for _, tool := range got {
		byName[tool.Function.Name] = tool
	}
	if len(got) != 3 {
		t.Fatalf("tools len = %d, want 3 (MoveTo + WorkShift + SocialChat)", len(got))
	}
	if _, ok := byName["move_to"]; !ok {
		t.Fatalf("move_to tool missing: %v", got)
	}
	if _, ok := byName["work_shift"]; !ok {
		t.Fatalf("work_shift tool missing: %v", got)
	}
	if _, ok := byName["social_chat"]; !ok {
		t.Fatalf("social_chat tool missing: %v", got)
	}
	if byName["work_shift"].Type != "function" {
		t.Errorf("tool type should be function")
	}
	if byName["work_shift"].Function.Description != "去指定设施执行工作" {
		t.Errorf("work_shift description = %q", byName["work_shift"].Function.Description)
	}
	// 校验 work_shift 的 parameters schema 含 semantic_group/interaction 且 required。
	var schema struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(byName["work_shift"].Function.Parameters, &schema); err != nil {
		t.Fatalf("parameters is not valid JSON: %v", err)
	}
	if schema.Type != "object" {
		t.Errorf("schema.type = %q, want object", schema.Type)
	}
	// duration 是 MCP 侧控制字段，2026-09-03 起 required 化（LLM 只填
	// required 参数，optional 从不被填导致动作时长失控）。
	if len(schema.Required) != 3 {
		t.Errorf("schema.required = %v, want [semantic_group interaction duration]", schema.Required)
	}
	hasDur := false
	for _, r := range schema.Required {
		if r == "duration" {
			hasDur = true
		}
	}
	if !hasDur {
		t.Errorf("schema.required = %v, want duration included", schema.Required)
	}
	// social_chat 不追加 duration（对话挂起直到结束，duration 会打断对话）。
	var sschema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(byName["social_chat"].Function.Parameters, &sschema); err != nil {
		t.Fatalf("social_chat parameters is not valid JSON: %v", err)
	}
	if _, ok := sschema.Properties["duration"]; ok {
		t.Errorf("social_chat should not have duration property")
	}
	for _, r := range sschema.Required {
		if r == "duration" {
			t.Errorf("social_chat duration must not be required")
		}
	}
	// 瞬时工具（speak/emote 等）不追加 duration 的验证见
	// TestCapabilityParamsSchema_DurationRequiredForNonInstant。
	// 校验 move_to 的 target_type enum。
	var mschema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(byName["move_to"].Function.Parameters, &mschema); err != nil {
		t.Fatalf("move_to parameters is not valid JSON: %v", err)
	}
	if len(mschema.Properties["target_type"].Enum) != 2 {
		t.Errorf("move_to target_type enum = %v, want 2 values", mschema.Properties["target_type"].Enum)
	}
}

func TestTacticalToolsFromRegistry_NilRegistryEmpty(t *testing.T) {
	if got := tacticalToolsFromRegistry(nil, "H-01"); got != nil {
		t.Fatalf("nil registry should return nil tools, got %v", got)
	}
}

func TestCapabilityParamsSchema_DurationRequiredForNonInstant(t *testing.T) {
	params := []protocol.CapabilityParam{
		{Name: "semantic_group", Type: "string", Required: true},
	}
	for _, name := range []string{"work_shift", "move_to", "interact", "exercise"} {
		raw := capabilityParamsSchema(params, name)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: invalid JSON: %v", name, err)
		}
		if _, ok := schema.Properties["duration"]; !ok {
			t.Errorf("%s: duration property missing", name)
		}
		required := false
		for _, r := range schema.Required {
			if r == "duration" {
				required = true
			}
		}
		if !required {
			t.Errorf("%s: duration not in required (got %v)", name, schema.Required)
		}
	}
	// 瞬时工具：立即完成，无时长概念——不追加 duration prop。
	for _, name := range []string{"speak", "emote", "turn_to", "generic_act"} {
		raw := capabilityParamsSchema(params, name)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: invalid JSON: %v", name, err)
		}
		if _, ok := schema.Properties["duration"]; ok {
			t.Errorf("%s: instant tool should not have duration property", name)
		}
		for _, r := range schema.Required {
			if r == "duration" {
				t.Errorf("%s: instant tool must not require duration", name)
			}
		}
	}
}

func TestTacticalToolsFromRegistry_SkipsNonQueueable(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	got := tacticalToolsFromRegistry(reg, "H-01")
	for _, tool := range got {
		if tool.Function.Name == "wait" {
			t.Errorf("wait is non-queueable and should be skipped")
		}
	}
}

// ─── physicalAlertOverrideGoal ────────────────────────────────

func TestPhysicalAlertOverrideGoal_NoAlert(t *testing.T) {
	origGoal := "车间装配作业"
	// hint 不含"物理状态告警"标记 → 不 override
	got, ok := physicalAlertOverrideGoal("上次中断原因：疲劳过高", origGoal, &protocol.PhysicalState{Fatigue: 80}, prompt.BandThresholds{})
	if ok {
		t.Errorf("non-alert hint should not override, got goal=%q override=true", got)
	}
	if got != origGoal {
		t.Errorf("non-alert hint should keep orig goal, got=%q want=%q", got, origGoal)
	}
}

func TestPhysicalAlertOverrideGoal_NilPhysical(t *testing.T) {
	origGoal := "车间装配作业"
	got, ok := physicalAlertOverrideGoal("物理状态告警自动升级(疲劳=62超过60)", origGoal, nil, prompt.BandThresholds{})
	if ok {
		t.Errorf("nil physical should not override, got goal=%q override=true", got)
	}
	if got != origGoal {
		t.Errorf("nil physical should keep orig goal, got=%q want=%q", got, origGoal)
	}
}

func TestPhysicalAlertOverrideGoal_FatigueAlert(t *testing.T) {
	origGoal := "车间装配作业"
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警自动升级(疲劳=82超过80)；原决策=observe/...",
		origGoal,
		&protocol.PhysicalState{Fatigue: 82, Energy: 80},
		prompt.BandThresholds{},
	)
	if !ok {
		t.Errorf("fatigue>80 should trigger override")
	}
	if !strings.Contains(got, "充电站") || !strings.Contains(got, "疲劳") {
		t.Errorf("override goal should mention 充电站 and 疲劳, got=%q", got)
	}
}

func TestPhysicalAlertOverrideGoal_EnergyAlert(t *testing.T) {
	origGoal := "车间装配"
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警自动升级(体力=35低于40)",
		origGoal,
		&protocol.PhysicalState{Fatigue: 30, Energy: 35},
		prompt.BandThresholds{},
	)
	if !ok {
		t.Errorf("energy<40 should trigger override")
	}
	if !strings.Contains(got, "充电站") || !strings.Contains(got, "体力") {
		t.Errorf("override goal should mention 充电站 and 体力, got=%q", got)
	}
}

func TestPhysicalAlertOverrideGoal_JointWearAlert(t *testing.T) {
	origGoal := "车间装配"
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警自动升级(关节磨损=75超过70)",
		origGoal,
		&protocol.PhysicalState{Fatigue: 30, Energy: 80, JointWear: 75},
		prompt.BandThresholds{},
	)
	if !ok {
		t.Errorf("joint_wear>70 should trigger override")
	}
	if !strings.Contains(got, "维护") || !strings.Contains(got, "关节磨损") {
		t.Errorf("override goal should mention 维护 and 关节磨损, got=%q", got)
	}
}

func TestPhysicalAlertOverrideGoal_FatigueTakesPrecedence(t *testing.T) {
	// 同时 fatigue 高 + energy 低时，fatigue 优先（switch 顺序）
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警",
		"工作",
		&protocol.PhysicalState{Fatigue: 85, Energy: 30},
		prompt.BandThresholds{},
	)
	if !ok {
		t.Errorf("should trigger override")
	}
	if !strings.Contains(got, "疲劳") {
		t.Errorf("fatigue should take precedence, got=%q", got)
	}
}

// ─── buildTacticalPrompt 物理告警强约束段 ─────────────────────

func TestBuildTacticalPrompt_PhysicalAlertConstraint(t *testing.T) {
	kb := loadTestKB(t)
	hint := "物理状态告警自动升级(疲劳=82超过80)；原决策=observe/..."
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "前往充电站休息", Zone: "main_workshop", TimeOfDay: "13:15", Slot: "13:00-17:00", Physical: &protocol.PhysicalState{Energy: 88, Fatigue: 82, JointWear: 0}, KB: kb, Hint: hint, AgentID: "H-01"})

	if !strings.Contains(promptText, "【物理告警强制约束】") {
		t.Errorf("prompt should contain physical alert constraint section, got: %s", promptText)
	}
	if !strings.Contains(promptText, "work_shift（消耗体力）") {
		t.Errorf("prompt should forbid work_shift, got: %s", promptText)
	}
	if !strings.Contains(promptText, "优先 charge_at_station") {
		t.Errorf("prompt should prioritize charge_at_station, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_PhysicalAlertJointWearConstraint 验证关节磨损告警时
// 强约束段要求 self_maintenance 且不禁 self_maintenance（那是恢复动作）。
func TestBuildTacticalPrompt_PhysicalAlertJointWearConstraint(t *testing.T) {
	kb := loadTestKB(t)
	hint := "物理状态告警自动升级(关节磨损=75超过70)；原决策=observe/..."
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "车间装配", Zone: "main_workshop", TimeOfDay: "14:00", Slot: "13:00-17:00", Physical: &protocol.PhysicalState{Energy: 88, Fatigue: 30, JointWear: 75}, KB: kb, Hint: hint, AgentID: "H-01"})

	if !strings.Contains(promptText, "【物理告警强制约束】") {
		t.Errorf("prompt should contain constraint section, got: %s", promptText)
	}
	if !strings.Contains(promptText, "self_maintenance") {
		t.Errorf("prompt should require self_maintenance for joint_wear alert, got: %s", promptText)
	}
	// 关节磨损告警不禁 self_maintenance（那是恢复动作）
	if strings.Contains(promptText, "self_maintenance（无助于恢复）") {
		t.Errorf("prompt should NOT forbid self_maintenance for joint_wear-only alert, got: %s", promptText)
	}
	// 关节磨损告警不禁 work_shift（仅疲劳告警才禁）
	if strings.Contains(promptText, "work_shift（消耗体力）") {
		t.Errorf("prompt should NOT forbid work_shift for joint_wear-only alert, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_NoPhysicalAlertConstraint(t *testing.T) {
	kb := loadTestKB(t)
	// 普通 hint（无"物理状态告警"标记）不应插入强约束段
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "车间装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 90, Fatigue: 20, JointWear: 0}, KB: kb, Hint: "上次中断原因：zone 变化", AgentID: "H-01"})

	if strings.Contains(promptText, "【物理告警强制约束】") {
		t.Errorf("non-physical-alert hint should NOT contain constraint section, got: %s", promptText)
	}
}

// ─── fillDefaultDurationForRest ────────────────────────────

func TestFillDefaultTimeToStopForRest_MidQueueRestGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench", "duration": 3600}},
	}
	got := fillDefaultDurationForRest(actions)
	if v, ok := got[1].Params["duration"]; !ok || v != defaultRestDurationSec {
		t.Fatalf("mid-queue rest should get default duration=%d, got %v", defaultRestDurationSec, got[1].Params["duration"])
	}
}

func TestFillDefaultTimeToStopForRest_KeepsExisting(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench", "duration": 900}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultDurationForRest(actions)
	if v, ok := got[1].Params["duration"]; !ok || v != 900 {
		t.Fatalf("existing duration should be preserved, got %v", got[1].Params["duration"])
	}
}

func TestFillDefaultTimeToStopForRest_TailRestUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultDurationForRest(actions)
	if _, ok := got[2].Params["duration"]; ok {
		t.Fatalf("tail rest should stay without duration, got %v", got[2].Params)
	}
}

func TestFillDefaultTimeToStopForRest_NonRestUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "charge", "semantic_group": "charger"}},
	}
	got := fillDefaultDurationForRest(actions)
	for i, a := range got {
		if _, ok := a.Params["duration"]; ok {
			t.Fatalf("non-rest action %d should not get duration, got %v", i, a.Params)
		}
	}
}

func TestFillDefaultTimeToStopForRest_SingleActionNoop(t *testing.T) {
	actions := []plannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultDurationForRest(actions)
	if _, ok := got[0].Params["duration"]; ok {
		t.Fatalf("single-action queue should be a no-op, got %v", got[0].Params)
	}
}

// ─── fillDefaultDurationForWork ────────────────────────────

func TestFillDefaultTimeToStopForWork_MidQueueWorkGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench", "duration": 900}},
	}
	got := fillDefaultDurationForWork(actions)
	if v, ok := got[1].Params["duration"]; !ok || v != defaultWorkDurationSec {
		t.Fatalf("mid-queue work should get default duration=%d, got %v", defaultWorkDurationSec, got[1].Params["duration"])
	}
}

func TestFillDefaultTimeToStopForWork_InteractWorkGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "sort_cargo", "semantic_group": "sorting_conveyor"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultDurationForWork(actions)
	if v, ok := got[1].Params["duration"]; !ok || v != defaultWorkDurationSec {
		t.Fatalf("mid-queue InteractSmartObject work should get default duration, got %v", got[1].Params["duration"])
	}
}

func TestFillDefaultTimeToStopForWork_KeepsExisting(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench", "duration": 7200}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultDurationForWork(actions)
	if v, ok := got[1].Params["duration"]; !ok || v != 7200 {
		t.Fatalf("existing duration should be preserved, got %v", got[1].Params["duration"])
	}
}

func TestFillDefaultTimeToStopForWork_TailWorkUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultDurationForWork(actions)
	if _, ok := got[2].Params["duration"]; ok {
		t.Fatalf("tail work should stay without duration, got %v", got[2].Params)
	}
}

func TestFillDefaultTimeToStopForWork_NonWorkUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "surf_internet", Params: map[string]any{"interaction": "surf_internet", "semantic_group": "computer"}},
	}
	got := fillDefaultDurationForWork(actions)
	for i, a := range got {
		if _, ok := a.Params["duration"]; ok {
			t.Fatalf("non-work action %d should not get duration, got %v", i, a.Params)
		}
	}
}

// ─── fallbackRetryActions ────────────────────────────────────

func TestFallbackRetryActions(t *testing.T) {
	acts := fallbackRetryActions()
	if len(acts) != 2 {
		t.Fatalf("got %d actions, want 2 (speak + look_around)", len(acts))
	}
	if acts[0].Action != "speak" {
		t.Errorf("first action should be speak, got %q", acts[0].Action)
	}
	if _, ok := acts[0].Params["content"]; !ok {
		t.Errorf("speak should carry content, got %v", acts[0].Params)
	}
	if acts[1].Action != "generic_act" {
		t.Errorf("second action should be generic_act, got %q", acts[1].Action)
	}
	if acts[1].Params["behavior"] != "look_around" {
		t.Errorf("generic_act behavior should be look_around, got %v", acts[1].Params["behavior"])
	}
	if v, ok := acts[1].Params["duration"]; !ok || v != 30 {
		t.Errorf("generic_act should have 30s duration, got %v", acts[1].Params["duration"])
	}
}

// TestMapTacticalAction_InteractZonePassthrough 验证 InteractSmartObject 的
// zone 参数会透传给 UE（否则"去中央广场长椅"的日程会落到 NPC 所在 zone）。
func TestMapTacticalAction_InteractZonePassthrough(t *testing.T) {
	kb := loadTestKB(t)
	// 填了 zone → 透传
	pa := plannedAction{Action: "InteractSmartObject", Params: map[string]any{
		"semantic_group": "bench", "interaction": "rest", "zone": "central_plaza",
	}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdInteractSmartObject {
		t.Fatalf("cmd=%q, want InteractSmartObject", cmd)
	}
	if params["zone"] != "central_plaza" {
		t.Errorf("zone should pass through to UE, got %v", params["zone"])
	}
	// 未填 zone → 不附加 zone 字段
	pa2 := plannedAction{Action: "InteractSmartObject", Params: map[string]any{
		"semantic_group": "bench", "interaction": "rest",
	}}
	_, params2, err := mapTacticalAction(pa2, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, has := params2["zone"]; has {
		t.Errorf("zone should be absent when not provided: %+v", params2)
	}
}
