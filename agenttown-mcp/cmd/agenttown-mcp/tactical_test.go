package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
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
	actions, _, err := generateTacticalPlan(context.Background(), tc, nil, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
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
	actions, _, err := generateTacticalPlan(context.Background(), tc, nil, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
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
	if _, _, err := generateTacticalPlan(context.Background(), tc, nil, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error when no tool calls returned")
	}
}

func TestGenerateTacticalPlan_AllFiltered(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "scan_area", Arguments: `{}`}},
	})}
	if _, _, err := generateTacticalPlan(context.Background(), tc, nil, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", "07:00-09:00: 上午准备\n09:00-12:00: 车间装配", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error when all tool calls filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeToolCallResponse([]llmtypes.ToolCall{
		{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"开始"}`}},
	})}
	_, _, _ = generateTacticalPlan(context.Background(), tc, nil, "H-01", "等待", "main_workshop", "09:00", "09:00-12:00", "", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil, nil)
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
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
	promptText := prompt.BuildTacticalSystemPrompt(kb, nil, "")
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

// TestBuildTacticalPrompt_InjectsObjectStatus (Fix B) 验证战术层 prompt 注入
// 【物体实时占用】段：当 ObjectStatus 非空且 KB 存在时，prompt 应包含按 category
// 聚合的占用摘要 + 附近物体实例状态。
func TestBuildTacticalPrompt_InjectsObjectStatus(t *testing.T) {
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
	// 应包含段落标题
	if !strings.Contains(promptText, "物体实时占用") {
		t.Errorf("prompt should contain '物体实时占用' section, got: %s", promptText)
	}
	// 应包含 work category 的占用摘要（1 空闲 / 1 占用）
	if !strings.Contains(promptText, "1 空闲") || !strings.Contains(promptText, "1 占用") {
		t.Errorf("prompt should show work category 1 idle / 1 occupied, got: %s", promptText)
	}
	// 应包含 charging category 全空闲
	if !strings.Contains(promptText, "6 空闲") {
		t.Errorf("prompt should show charging category 6 idle, got: %s", promptText)
	}
	// 应包含附近实例状态
	if !strings.Contains(promptText, "WorkBench") {
		t.Errorf("prompt should mention nearby WorkBench, got: %s", promptText)
	}
	// 应包含"日程不合理"相关的引导文本（规则 2：日程不合理/设施占用时
	// 鼓励安排其他更合理的动作；含夜间工作反例）。
	if !strings.Contains(prompt.TacticalRules, "半夜不睡觉而是跑步/工作") ||
		!strings.Contains(prompt.TacticalRules, "请下发更合理的动作") {
		t.Errorf("system prompt should guide LLM to avoid doomed occupancy actions")
	}
	// 所有工种设备都可用 InteractSmartObject 直接工作（process/debug/dismantle
	// 等无复合动作的工种依据）；同时锚定 action 字段名 interact 防止 LLM 写错工具名。
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
	promptText := prompt.BuildTacticalSystemPrompt(kb, nil, "H-01")
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
	promptText := prompt.BuildTacticalSystemPrompt(nil, nil, "")
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
	if len(schema.Required) != 2 {
		t.Errorf("schema.required = %v, want [semantic_group interaction]", schema.Required)
	}
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

// ─── buildTacticalExample (category-aware) ──────────────────

func TestBuildTacticalExample_ChargingStationFirst(t *testing.T) {
	// 回归测试：当前 KB 第一个 object（按 ID 排序）是 bench-1（category=rest），
	// 示例走 default 分支：move_to + interact。
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "", "")
	if !strings.Contains(got, "InteractSmartObject") {
		t.Errorf("example should use interact for rest category: %q", got)
	}
	if !strings.Contains(got, "bench") {
		t.Errorf("example should reference bench semantic_group: %q", got)
	}
}

func TestBuildTacticalExample_WorkbenchOnly(t *testing.T) {
	// KB 只含 workbench 时示例应用 work_shift。
	kb := &worldkb.KB{
		Zones: []worldkb.Zone{{ID: "main_workshop", DisplayName: "车间"}},
		Objects: []worldkb.Object{{
			ID:                    "wb_01",
			DisplayName:           "工作台",
			Category:              "workbench",
			ZoneID:                "main_workshop",
			AvailableInteractions: []string{"assemble", "inspect"},
		}},
	}
	got := prompt.TacticalExample(kb, "", "")
	if !strings.Contains(got, "work_shift") {
		t.Errorf("example should use work_shift for workbench category: %q", got)
	}
	if !strings.Contains(got, "wb_01") {
		t.Errorf("example should reference wb_01: %q", got)
	}
}

func TestBuildTacticalExample_RestBenchOnly(t *testing.T) {
	// KB 只含 rest_bench（category 无专用复合工具）时示例应用 interact + rest。
	kb := &worldkb.KB{
		Zones: []worldkb.Zone{{ID: "rest_area", DisplayName: "休息区"}},
		Objects: []worldkb.Object{{
			ID:                    "bench_01",
			DisplayName:           "长椅",
			Category:              "rest_bench",
			ZoneID:                "rest_area",
			AvailableInteractions: []string{"rest"},
		}},
	}
	got := prompt.TacticalExample(kb, "", "")
	if !strings.Contains(got, `"action":"InteractSmartObject"`) {
		t.Errorf("example should use interact for rest_bench category: %q", got)
	}
	if !strings.Contains(got, `"interaction":"rest"`) {
		t.Errorf("example should use interaction=rest: %q", got)
	}
	if strings.Contains(got, "work_shift") || strings.Contains(got, "charge_at_station") {
		t.Errorf("example must NOT use composite tools for rest_bench: %q", got)
	}
}

func TestBuildTacticalExample_NilKB(t *testing.T) {
	// nil KB 返回通用占位示例，不引用任何具体 id。
	got := prompt.TacticalExample(nil, "", "")
	// 示例应包含 speak 作为首个 action（prompt 要求 NPC 执行动作前先 speak）
	if !strings.Contains(got, `"action":"speak"`) {
		t.Errorf("nil KB example should start with speak action: %q", got)
	}
	if strings.Contains(got, "inner_thought") {
		t.Errorf("nil KB example should NOT contain deprecated inner_thought: %q", got)
	}
	if strings.Contains(got, "work_shift") || strings.Contains(got, "charge_at_station") {
		t.Errorf("nil KB example should not reference specific composite tools: %q", got)
	}
}

// TestBuildTacticalExample_ZoneObjectPairing 回归测试：示例中 move_to
// 的 target_id 必须与示例 object 的 ZoneID 一致。旧版取 ListZones()[0] 作示例 zone，
// 但 ListZones()[0]=archive_station 与 ListObjects()[0]=charge（在 central_plaza）
// 不在同一 zone，示例本身错配，LLM 模仿后产生 zone-object 错配。
//
// 2026-08-11 修复后：复合动作示例（work_shift/charge_at_station）去掉 move_to 前置
// （复合动作自带移动），只有 default 分支（interact 原子组合）才有 move_to。
// 本测试改用 inline KB 构造一个 default 分支 object（category=rest_bench）验证配对。
func TestBuildTacticalExample_ZoneObjectPairing(t *testing.T) {
	kb := &worldkb.KB{
		Zones: []worldkb.Zone{
			{ID: "archive_station", DisplayName: "档案馆"},
			{ID: "rest_area", DisplayName: "休息区"},
		},
		Objects: []worldkb.Object{{
			ID:                    "bench_01",
			DisplayName:           "长椅",
			Category:              "rest_bench",
			ZoneID:                "rest_area",
			AvailableInteractions: []string{"rest"},
		}},
	}
	objs := kb.ListObjects()
	if len(objs) == 0 {
		t.Skip("KB has no objects, pairing test not applicable")
	}
	firstObj := objs[0]
	wantZone := firstObj.ZoneID
	if wantZone == "" {
		t.Skip("first object has no ZoneID, cannot verify pairing")
	}
	got := prompt.TacticalExample(kb, "", "")
	// default 分支示例应包含 move_to 到 wantZone，且引用 firstObj.ID。
	moveLine := fmt.Sprintf(`{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}`, wantZone)
	if !strings.Contains(got, moveLine) {
		t.Errorf("example should move_to(%s) to match object's ZoneID, got: %q", wantZone, got)
	}
	if !strings.Contains(got, firstObj.ID) {
		t.Errorf("example should reference first object %q, got: %q", firstObj.ID, got)
	}
	// 反向验证：不应出现 ListZones()[0]（若它与 object 的 ZoneID 不同）。
	firstZone := kb.ListZones()[0]
	if firstZone.ID != wantZone {
		badLine := fmt.Sprintf(`{"action":"move_to","params":{"target_type":"zone","target_id":"%s"}}`, firstZone.ID)
		if strings.Contains(got, badLine) {
			t.Errorf("example must NOT move_to(%s) — object %s is in %s, got: %q",
				firstZone.ID, firstObj.ID, wantZone, got)
		}
	}
}

// ─── buildTacticalExample (goal-aware, P0-2) ──────────────────
// 2026-08-24 停用：exampleForGoal 的 goal 关键词→示例路由已注释（"长椅/休息"
// 分支硬编码 move_to central_plaza 是午休扎堆中央广场的根因）。goal 示例测试
// 一并移除；剩余 fallback 测试（category-aware）继续覆盖 TacticalExample。

func TestBuildTacticalExample_GoalFallbackOnMissingObject(t *testing.T) {
	// goal=装配 但 KB 无 workbench object → 降级到默认示例（首个 object 的 category）
	kb := &worldkb.KB{
		Zones: []worldkb.Zone{{ID: "charging_station", DisplayName: "充电站"}},
		Objects: []worldkb.Object{{
			ID:                    "cs_01",
			DisplayName:           "充电桩",
			Category:              "charging_station",
			ZoneID:                "charging_station",
			AvailableInteractions: []string{"charge", "inspect"},
		}},
	}
	got := prompt.TacticalExample(kb, "上午装配作业", "")
	// 应降级到 charge_at_station（首个 object 的 category）
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("assembly goal with no workbench should fall back to charge example: %q", got)
	}
}

func TestBuildTacticalExample_GoalEmptyFallback(t *testing.T) {
	// 空 goal 应降级到默认示例（首个 object 是 bench-1，走 default interact 分支）
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "", "")
	if !strings.Contains(got, "InteractSmartObject") {
		t.Errorf("empty goal should fall back to first-object example: %q", got)
	}
	if !strings.Contains(got, "bench") {
		t.Errorf("empty goal example should reference bench: %q", got)
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

// ─── truncateConversationRounds ───────────────────────────────

// testConvRound 构造一轮完整对话：assistant(带 tool_calls) + 对应 tool 结果。
func testConvRound(id string) []llmtypes.Message {
	return []llmtypes.Message{
		{Role: "assistant", Content: "speak " + id, ToolCalls: []llmtypes.ToolCall{{ID: id, Type: "function", Function: llmtypes.ToolFunction{Name: "speak", Arguments: "{}"}}}},
		{Role: "tool", Content: "result=success", ToolCallID: id},
	}
}

func TestTruncateConversationRounds_Empty(t *testing.T) {
	if got := truncateConversationRounds(nil, maxTacticalHistoryRounds); got != nil {
		t.Errorf("empty input should stay nil, got %v", got)
	}
	if got := truncateConversationRounds([]llmtypes.Message{}, maxTacticalHistoryRounds); len(got) != 0 {
		t.Errorf("empty slice should stay empty, got %d items", len(got))
	}
}

func TestTruncateConversationRounds_NoopWhenBelowLimit(t *testing.T) {
	conv := []llmtypes.Message{}
	for i := 0; i < 5; i++ {
		conv = append(conv, testConvRound(fmt.Sprintf("c%d", i))...)
	}
	got := truncateConversationRounds(conv, maxTacticalHistoryRounds)
	if len(got) != len(conv) {
		t.Errorf("below-limit history should be unchanged: got %d want %d", len(got), len(conv))
	}
	for i := range conv {
		if got[i].Role != conv[i].Role || got[i].Content != conv[i].Content {
			t.Errorf("below-limit history should preserve order/content: got[%d]=%+v want %+v", i, got[i], conv[i])
		}
	}
}

func TestTruncateConversationRounds_KeepsRecentRounds(t *testing.T) {
	conv := []llmtypes.Message{}
	for i := 0; i < 20; i++ {
		conv = append(conv, testConvRound(fmt.Sprintf("c%d", i))...)
	}
	got := truncateConversationRounds(conv, 8)
	if len(got) != 16 { // 8 轮 × 2 条
		t.Fatalf("got %d messages, want 16 (8 rounds)", len(got))
	}
	// 起点必须是 assistant（tool 消息不能成为头部孤儿）。
	if got[0].Role != "assistant" {
		t.Errorf("truncated head should be an assistant message, got role=%q", got[0].Role)
	}
	// 应保留 c12..c19 这最后 8 轮。
	if !strings.Contains(got[0].Content, "c12") {
		t.Errorf("head should be round c12, got %q", got[0].Content)
	}
	// 每一对 assistant/tool 必须保持配对与顺序。
	for i := 0; i+1 < len(got); i += 2 {
		if got[i].Role != "assistant" || got[i+1].Role != "tool" {
			t.Fatalf("pair %d broken: %+v / %+v", i, got[i], got[i+1])
		}
		if got[i+1].ToolCallID != got[i].ToolCalls[0].ID {
			t.Errorf("pair %d: tool_call_id %q does not match assistant tool_calls id %q",
				i, got[i+1].ToolCallID, got[i].ToolCalls[0].ID)
		}
	}
}

func TestTruncateConversationRounds_NonPositiveLimit(t *testing.T) {
	conv := testConvRound("c0")
	if got := truncateConversationRounds(conv, 0); got != nil {
		t.Errorf("maxRounds=0 should return nil, got %v", got)
	}
	if got := truncateConversationRounds(conv, -3); got != nil {
		t.Errorf("negative maxRounds should return nil, got %v", got)
	}
}

func TestTruncateConversationRounds_OrphanToolHead(t *testing.T) {
	conv := []llmtypes.Message{
		{Role: "tool", Content: "result=failed", ToolCallID: "orphan"},
	}
	for i := 0; i < 3; i++ {
		conv = append(conv, testConvRound(fmt.Sprintf("c%d", i))...)
	}
	got := truncateConversationRounds(conv, 8)
	// 头部孤儿 tool 应被丢弃（不足上限，但头部非 assistant）。
	if len(got) != 6 {
		t.Fatalf("got %d messages, want 6 (orphan dropped)", len(got))
	}
	if got[0].Role != "assistant" {
		t.Errorf("head should be first assistant, got role=%q", got[0].Role)
	}
}

// ─── fillDefaultTimeToStopForRest ────────────────────────────

func TestFillDefaultTimeToStopForRest_MidQueueRestGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench", "time_to_stop": 3600}},
	}
	got := fillDefaultTimeToStopForRest(actions)
	if v, ok := got[1].Params["time_to_stop"]; !ok || v != defaultRestTimeToStopSec {
		t.Fatalf("mid-queue rest should get default time_to_stop=%d, got %v", defaultRestTimeToStopSec, got[1].Params["time_to_stop"])
	}
}

func TestFillDefaultTimeToStopForRest_KeepsExisting(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench", "time_to_stop": 900}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultTimeToStopForRest(actions)
	if v, ok := got[1].Params["time_to_stop"]; !ok || v != 900 {
		t.Fatalf("existing time_to_stop should be preserved, got %v", got[1].Params["time_to_stop"])
	}
}

func TestFillDefaultTimeToStopForRest_TailRestUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultTimeToStopForRest(actions)
	if _, ok := got[2].Params["time_to_stop"]; ok {
		t.Fatalf("tail rest should stay without time_to_stop, got %v", got[2].Params)
	}
}

func TestFillDefaultTimeToStopForRest_NonRestUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "charge", "semantic_group": "charger"}},
	}
	got := fillDefaultTimeToStopForRest(actions)
	for i, a := range got {
		if _, ok := a.Params["time_to_stop"]; ok {
			t.Fatalf("non-rest action %d should not get time_to_stop, got %v", i, a.Params)
		}
	}
}

func TestFillDefaultTimeToStopForRest_SingleActionNoop(t *testing.T) {
	actions := []plannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultTimeToStopForRest(actions)
	if _, ok := got[0].Params["time_to_stop"]; ok {
		t.Fatalf("single-action queue should be a no-op, got %v", got[0].Params)
	}
}

// ─── fillDefaultTimeToStopForWork ────────────────────────────

func TestFillDefaultTimeToStopForWork_MidQueueWorkGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench", "time_to_stop": 900}},
	}
	got := fillDefaultTimeToStopForWork(actions)
	if v, ok := got[1].Params["time_to_stop"]; !ok || v != defaultWorkTimeToStopSec {
		t.Fatalf("mid-queue work should get default time_to_stop=%d, got %v", defaultWorkTimeToStopSec, got[1].Params["time_to_stop"])
	}
}

func TestFillDefaultTimeToStopForWork_InteractWorkGetsDefault(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "sort_cargo", "semantic_group": "sorting_conveyor"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
	}
	got := fillDefaultTimeToStopForWork(actions)
	if v, ok := got[1].Params["time_to_stop"]; !ok || v != defaultWorkTimeToStopSec {
		t.Fatalf("mid-queue InteractSmartObject work should get default time_to_stop, got %v", got[1].Params["time_to_stop"])
	}
}

func TestFillDefaultTimeToStopForWork_KeepsExisting(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench", "time_to_stop": 7200}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultTimeToStopForWork(actions)
	if v, ok := got[1].Params["time_to_stop"]; !ok || v != 7200 {
		t.Fatalf("existing time_to_stop should be preserved, got %v", got[1].Params["time_to_stop"])
	}
}

func TestFillDefaultTimeToStopForWork_TailWorkUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "work_shift", Params: map[string]any{"interaction": "assemble", "semantic_group": "workbench"}},
	}
	got := fillDefaultTimeToStopForWork(actions)
	if _, ok := got[2].Params["time_to_stop"]; ok {
		t.Fatalf("tail work should stay without time_to_stop, got %v", got[2].Params)
	}
}

func TestFillDefaultTimeToStopForWork_NonWorkUntouched(t *testing.T) {
	actions := []plannedAction{
		{Action: "speak", Params: map[string]any{"content": "hi"}},
		{Action: "InteractSmartObject", Params: map[string]any{"interaction": "rest", "semantic_group": "bench"}},
		{Action: "surf_internet", Params: map[string]any{"interaction": "surf_internet", "semantic_group": "computer"}},
	}
	got := fillDefaultTimeToStopForWork(actions)
	for i, a := range got {
		if _, ok := a.Params["time_to_stop"]; ok {
			t.Fatalf("non-work action %d should not get time_to_stop, got %v", i, a.Params)
		}
	}
}
