package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
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

// ─── parseTacticalNDJSON ─────────────────────────────────────

func TestParseTacticalNDJSON_Valid(t *testing.T) {
	// inner_thought 行被静默忽略（向后兼容）；speak 由 LLM 显式输出为首个 action
	raw := `{"action":"speak","params":{"content":"先去车间再装配"}}` + "\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n" +
		`{"action":"work_shift","params":{"semantic_group":"workbench_01","interaction":"assemble"}}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("got %d actions, want 3 (speak + move + work)", len(actions))
	}
	if actions[0].Action != "speak" {
		t.Errorf("action[0]=%q, want speak", actions[0].Action)
	}
	if actions[0].Params["content"] != "先去车间再装配" {
		t.Errorf("speak content=%v", actions[0].Params["content"])
	}
	if actions[1].Action != "move_to" {
		t.Errorf("action[1]=%q, want move_to", actions[1].Action)
	}
}

func TestParseTacticalNDJSON_WithFence(t *testing.T) {
	raw := "```json\n" +
		`{"action":"speak","params":{"content":"充电"}}` + "\n" +
		`{"action":"charge_at_station","params":{"semantic_group":"charging_station_01","interaction":"assemble"}}` + "\n" +
		"```"
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 || actions[0].Action != "speak" || actions[1].Action != "charge_at_station" {
		t.Errorf("actions=%+v, want [speak, charge_at_station]", actions)
	}
}

func TestParseTacticalNDJSON_BlankLines(t *testing.T) {
	raw := `{"action":"speak","params":{"content":"开始"}}` + "\n\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n" +
		"\n"
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 || actions[0].Action != "speak" || actions[1].Action != "move_to" {
		t.Errorf("actions=%+v, want [speak, move_to]", actions)
	}
}

func TestParseTacticalNDJSON_MalformedLine(t *testing.T) {
	// 单行 parse 失败应跳过，不影响其他行
	raw := `{"action":"speak","params":{"content":"计划"}}` + "\n" +
		`这不是JSON` + "\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 || actions[0].Action != "speak" || actions[1].Action != "move_to" {
		t.Errorf("actions=%+v, want [speak, move_to]", actions)
	}
}

func TestParseTacticalNDJSON_Empty(t *testing.T) {
	actions, err := parseTacticalNDJSON("", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("got %d actions, want 0", len(actions))
	}
}

// TestParseTacticalNDJSON_LegacyInnerThoughtIgnored 验证旧 LLM 输出仍含
// inner_thought 行时被静默忽略（不报错、不转 speak、不污染 actions）。
// 向后兼容路径：prompt 已要求首个 action 为 speak，但模型偶尔可能仍输出
// inner_thought 字段，解析层需优雅降级。
func TestParseTacticalNDJSON_LegacyInnerThoughtIgnored(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}` + "\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// inner_thought 行被忽略，只剩 move_to
	if len(actions) != 1 || actions[0].Action != "move_to" {
		t.Errorf("actions=%+v, want [move_to] (inner_thought silently dropped)", actions)
	}
}

// TestParseTacticalNDJSON_ThoughtOnlyNoActions 验证只有 inner_thought、
// 无任何 action 行时返回空 actions（不注入 speak）。
func TestParseTacticalNDJSON_ThoughtOnlyNoActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("got %d actions, want 0", len(actions))
	}
}

func TestParseTacticalNDJSON_FiltersScanAreaAndStop(t *testing.T) {
	raw := `{"action":"speak","params":{"content":"扫描一下"}}` + "\n" +
		`{"action":"scan_area","params":{}}` + "\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n" +
		`{"action":"stop","params":{}}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// speak + move_to（scan_area/stop 被过滤）
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2 (speak + move_to)", len(actions))
	}
	if actions[0].Action != "speak" {
		t.Errorf("actions[0]=%q, want speak", actions[0].Action)
	}
	if actions[1].Action != "move_to" {
		t.Errorf("actions[1]=%q, want move_to", actions[1].Action)
	}
}

// TestParseTacticalNDJSON_NoSpeakNoInjection 验证 LLM 未输出 speak 时
// 不会自动注入 speak（speak 现在是 LLM 显式输出的 action，不再由 thought 转换）。
func TestParseTacticalNDJSON_NoSpeakNoInjection(t *testing.T) {
	raw := `{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}`
	actions, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0].Action != "move_to" {
		t.Errorf("actions=%+v, want [move_to] (no auto speak injection)", actions)
	}
}

// ─── streamAccumulator ───────────────────────────────────────

func TestStreamAccumulator_Feed(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}

	// 第一行 speak 完整到达，立即触发 onComplete
	acc.feed(`{"action":"speak","params":{"content":"开工"}}` + "\n")
	if len(collected) != 1 {
		t.Fatalf("after first feed: collected=%d, want 1 (speak immediate)", len(collected))
	}
	if collected[0].Action != "speak" {
		t.Errorf("collected[0]=%q, want speak", collected[0].Action)
	}
	if collected[0].Params["content"] != "开工" {
		t.Errorf("speak content=%v, want 开工", collected[0].Params["content"])
	}

	// 第二行被拆成两个 delta
	acc.feed(`{"action":"move_to","params":{"targ`)
	acc.feed(`et":"main_workshop"}}` + "\n")
	if len(collected) != 2 {
		t.Fatalf("after second line: collected=%d, want 2 (speak + move_to)", len(collected))
	}
	if collected[1].Action != "move_to" {
		t.Errorf("collected[1]=%q, want move_to", collected[1].Action)
	}

	// 第三行不完整（无 \n），不应触发 onComplete
	acc.feed(`{"action":"interact","params":{"semantic_group":"workbench_01","interaction":"assemble"}}`)
	if len(collected) != 2 {
		t.Errorf("incomplete line should not trigger: collected=%d, want 2", len(collected))
	}

	// flush 处理残余
	acc.flush()
	if len(collected) != 3 {
		t.Fatalf("after flush: collected=%d, want 3 (speak + move + interact)", len(collected))
	}
	if collected[2].Action != "interact" {
		t.Errorf("collected[2]=%q, want interact", collected[2].Action)
	}
}

func TestStreamAccumulator_FiltersInvalidAction(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}
	acc.feed(`{"action":"scan_area","params":{}}` + "\n")
	acc.feed(`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n")
	acc.flush()
	if len(collected) != 1 {
		t.Fatalf("collected=%d, want 1 (scan_area filtered)", len(collected))
	}
	if collected[0].Action != "move_to" {
		t.Errorf("collected[0]=%q, want move_to", collected[0].Action)
	}
}

// TestStreamAccumulator_LegacyInnerThoughtIgnored 验证流式路径收到
// inner_thought 行时静默忽略（不报错、不触发 onComplete）。
func TestStreamAccumulator_LegacyInnerThoughtIgnored(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}
	// inner_thought 行应被忽略
	acc.feed(`{"inner_thought":"开工"}` + "\n")
	acc.feed(`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n")
	acc.flush()
	if len(collected) != 1 {
		t.Fatalf("collected=%d, want 1 (inner_thought ignored, only move_to)", len(collected))
	}
	if collected[0].Action != "move_to" {
		t.Errorf("collected[0]=%q, want move_to", collected[0].Action)
	}
}

// TestStreamAccumulator_ThoughtOnlyNoAction 验证流式路径只有 inner_thought、
// 无任何 action 行时不触发 onComplete。
func TestStreamAccumulator_ThoughtOnlyNoAction(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}
	acc.feed(`{"inner_thought":"不知道做什么"}` + "\n")
	acc.flush()
	if len(collected) != 0 {
		t.Errorf("collected=%d, want 0 (inner_thought ignored, no actions)", len(collected))
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

func TestGenerateTacticalPlan_HTTPError(t *testing.T) {
	tc := &fakeStrategicCaller{err: errors.New("network down")}
	actions, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil)
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
	raw := `{"action":"speak","params":{"content":"先移动再装配"}}` + "\n" +
		`{"action":"move_to","params":{"target_type":"zone","target_id":"main_workshop"}}` + "\n" +
		`{"action":"work_shift","params":{"semantic_group":"workbench_01","interaction":"assemble"}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	actions, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10}, nil, nil, slog.Default(), "", "", "", nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// LLM 显式输出 speak 作为首个 action
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

func TestGenerateTacticalPlan_ParseFail(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeStrategicResponse("我今天打算去车间转转。")}
	if _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil); err == nil {
		t.Fatal("expected error on parse failure (no actions)")
	}
}

func TestGenerateTacticalPlan_EmptyActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	if _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil); err == nil {
		t.Fatal("expected error when all actions filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	raw := `{"action":"speak","params":{"content":"开始"}}` + "\n" +
		`{"action":"wait","params":{"duration_sec":30}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	_, _ = generateTacticalPlan(context.Background(), tc, "H-01", "等待", "main_workshop", "09:00", "09:00-12:00", nil, nil, nil, slog.Default(), "", "", "", nil, nil, nil)
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
	}
}

// ─── buildTacticalPrompt ─────────────────────────────────────

func TestBuildTacticalPrompt_NilPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: nil, Hint: "", Actions: nil, AgentID: ""})
	if promptText == "" {
		t.Fatal("promptText should not be empty")
	}
	// nil physical 时跳过物理状态行（UE 未实现物理状态，不传 0 值避免 LLM 误判）
	if strings.Contains(promptText, "物理状态") {
		t.Errorf("prompt should not contain '物理状态' for nil physical, got: %s", promptText)
	}
	// slot 为空时不应有时长提示行
	if strings.Contains(promptText, "请让步骤总时长接近此时长") {
		t.Errorf("prompt should not contain slot duration hint when slot is empty, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_ZeroPhysical 验证全 0 物理状态（UE 已上报但值全 0）
// 也跳过物理状态行注入，与 nil physical 同等处理。
func TestBuildTacticalPrompt_ZeroPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{}, KB: nil, Hint: "", Actions: nil, AgentID: ""})
	if strings.Contains(promptText, "物理状态") {
		t.Errorf("prompt should not contain '物理状态' for all-zero physical, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_WithPhysical(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "", Actions: nil, AgentID: ""})
	if !strings.Contains(promptText, "能量 75") {
		t.Errorf("prompt should contain '能量 75', got: %s", promptText)
	}
	if !strings.Contains(promptText, "疲劳 30") {
		t.Errorf("prompt should contain '疲劳 30'")
	}
	// slot 有效时应包含时长提示
	if !strings.Contains(promptText, "当前时段 09:00-12:00，约 180 分钟") {
		t.Errorf("prompt should contain slot duration hint, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_InjectsKBContext(t *testing.T) {
	kb := loadTestKB(t)
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: kb, Hint: "", Actions: nil, AgentID: ""})
	// 应包含 KB 中所有 zone（assets/world_kb.yaml 当前是 7-zone 工业园区）
	for _, zID := range []string{"main_workshop", "central_plaza", "logistics_hub", "repair_bay", "residential_quarters", "archive_station", "recycling_yard"} {
		if !strings.Contains(promptText, zID) {
			t.Errorf("prompt should list zone %q, got: %s", zID, promptText)
		}
	}
	// 应包含所有 object（新 UE5 KB: charge/repairtable/sleeppod/workbench）
	for _, oID := range []string{"workbench", "charge", "sleeppod", "repairtable"} {
		if !strings.Contains(promptText, oID) {
			t.Errorf("prompt should list object %q, got: %s", oID, promptText)
		}
	}
	// 应包含"可交互物体"段落标题及交互动词
	if !strings.Contains(promptText, "可交互物体") {
		t.Errorf("prompt should contain '可交互物体' section, got: %s", promptText)
	}
	if !strings.Contains(promptText, "assemble") || !strings.Contains(promptText, "charge") || !strings.Contains(promptText, "sleep") {
		t.Errorf("prompt should list available interactions on objects, got: %s", promptText)
	}
	// 验证新格式：每个 object 单独一行，明确分离 id/zone/interaction
	// 不应再出现旧的 "id|zone[interactions]" 拼接格式
	if strings.Contains(promptText, "workbench|main_workshop[") {
		t.Errorf("prompt should not contain legacy 'id|zone[interactions]' format, got: %s", promptText)
	}
	// 应包含明确的 semantic_group/zone/interaction 标注
	if !strings.Contains(promptText, "semantic_group=workbench") {
		t.Errorf("prompt should contain 'semantic_group=workbench' label, got: %s", promptText)
	}
	if !strings.Contains(promptText, "位于 zone=main_workshop") {
		t.Errorf("prompt should contain '位于 zone=main_workshop', got: %s", promptText)
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
		Actions:       nil,
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
	// 应包含"避免直接重试"或"全部占用"相关的引导文本
	if !strings.Contains(promptText, "禁止规划必然失败") {
		t.Errorf("prompt should guide LLM to avoid doomed occupancy actions, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_NilObjectStatusNoSection 验证 ObjectStatus 为空时
// 【物体实时占用】段整体省略，不污染 prompt（兼容 UE 未推送 object_status 的场景）。
// 注意：要求 #9 模板里固定提及"物体实时占用"字样，故不能 grep 该词；改用段体特征
// "按 category 聚合"判断段是否实际渲染。
func TestBuildTacticalPrompt_NilObjectStatusNoSection(t *testing.T) {
	kb := loadTestKB(t)
	promptText := prompt.BuildTactical(prompt.TacticalInput{
		Goal:     "装配",
		Zone:     "main_workshop",
		TimeOfDay: "09:00",
		Slot:     "09:00-12:00",
		Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5},
		KB:       kb,
		Hint:     "",
		Actions:  nil,
		AgentID:  "",
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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: nil, Hint: "", Actions: nil, AgentID: ""})
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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: kb, Hint: "", Actions: nil, AgentID: "H-01"})
	if !strings.Contains(promptText, "【你的角色】") {
		t.Errorf("prompt missing '【你的角色】' section header, got: %s", promptText)
	}
	for _, want := range []string{"老陈", "supervisor、worker、maintainer", "沉稳"} {
		if !strings.Contains(promptText, want) {
			t.Errorf("prompt missing role field %q, got: %s", want, promptText)
		}
	}
}

// TestBuildTacticalPrompt_NilKBNoRole 验证 kb==nil 时 prompt 不含
// 【你的角色】段（roleLine 降级为空串，prompt 中仅留空行）。
func TestBuildTacticalPrompt_NilKBNoRole(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: nil, Hint: "", Actions: nil, AgentID: ""})
	if strings.Contains(promptText, "【你的角色】") {
		t.Errorf("prompt should not contain '【你的角色】' when KB is nil, got: %s", promptText)
	}
}

// TestBuildTacticalPrompt_AgentNotFoundNoRole 验证 KB 存在但 agentID
// 不在 KB 中时也降级跳过【你的角色】段（buildAgentRoleContext 返回空串）。
func TestBuildTacticalPrompt_AgentNotFoundNoRole(t *testing.T) {
	kb := loadTestKB(t)
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "", Physical: nil, KB: kb, Hint: "", Actions: nil, AgentID: "NONEXISTENT-99"})
	if strings.Contains(promptText, "【你的角色】") {
		t.Errorf("prompt should not include '【你的角色】' for unknown agent, got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_WithHint(t *testing.T) {
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "fatigue=72 已突破警戒带，当前装配任务不合理", Actions: nil, AgentID: ""})
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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "", Actions: nil, AgentID: ""})
	if strings.Contains(promptText, "【上次中断原因】") {
		t.Errorf("prompt should not contain '【上次中断原因】' when hint is empty, got: %s", promptText)
	}
}

// ─── registry-aware tactical prompt / filtering ─────────────

func TestBuildTacticalPrompt_RegistryFiltersTools(t *testing.T) {
	// Registry with only CmdMoveTo + CmdWait available. CmdWait is now
	// filtered from the tactical tool list (long composites run until slot
	// transition, queue empty triggers tacticalRefill, no idle wait), so only
	// move_to should appear in the prompt.
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5}, KB: nil, Hint: "", Actions: reg.EffectiveActions("H-01"), AgentID: "H-01"})
	// Tool bullet list should contain move_to.
	if !strings.Contains(promptText, "- move_to [原子]:") {
		t.Errorf("prompt should list move_to as [原子] bullet, got: %s", promptText)
	}
	// wait should NOT appear as a tool bullet (filtered from tactical prompt).
	if strings.Contains(promptText, "- wait [") {
		t.Errorf("prompt should NOT list wait as a bullet (filtered from tactical prompt), got: %s", promptText)
	}
	// Tool bullet list should NOT contain composite tools (composite cmds unavailable).
	// Check the bullet prefix specifically — the hardcoded example section
	// mentions work_shift regardless, which is a separate prompt-quality concern.
	if strings.Contains(promptText, "- work_shift [复合]:") || strings.Contains(promptText, "- charge_at_station [复合]:") {
		t.Errorf("prompt should NOT list composite tools as [复合] bullets (composite cmds unavailable), got: %s", promptText)
	}
	// Count in header should match available tools (1 — only move_to, wait filtered).
	if !strings.Contains(promptText, "仅限以下 1 个") {
		t.Errorf("prompt header should say '仅限以下 1 个', got: %s", promptText)
	}
}

func TestBuildTacticalPrompt_PerAgentOverride(t *testing.T) {
	// Global has CmdMoveTo + CmdWorkShift; per-agent H-02 only
	// has CmdMoveTo. H-02's prompt should NOT list composite tools.
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWorkShift, Kind: "composite"},
	})
	reg.Register("H-02", []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	promptH01 := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75}, KB: nil, Hint: "", Actions: reg.EffectiveActions("H-01"), AgentID: "H-01"})
	promptH02 := prompt.BuildTactical(prompt.TacticalInput{Goal: "装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 75}, KB: nil, Hint: "", Actions: reg.EffectiveActions("H-02"), AgentID: "H-02"})
	// Check bullet prefix — example section is hardcoded and not registry-aware.
	if !strings.Contains(promptH01, "- work_shift [复合]:") {
		t.Errorf("H-01 prompt should list composite tools as [复合] bullets (global default), got: %s", promptH01)
	}
	if strings.Contains(promptH02, "- work_shift [复合]:") {
		t.Errorf("H-02 prompt should NOT list composite tools as [复合] bullets (per-agent override), got: %s", promptH02)
	}
}

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
		{"22:00-06:00", 480},  // 8 小时
		{"23:30-06:00", 390},  // 6.5 小时
		{"20:00-00:30", 270},  // 4.5 小时
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

// TestBuildTacticalToolList_NewCmdDerived verifies that a UE-pushed new cmd
// (not in BuiltinToolSpecs) appears in the tactical prompt tool list with
// Desc/Params derived from CapabilityAction metadata.
func TestBuildTacticalToolList_NewCmdDerived(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{
			Cmd:         "WaveHand",
			Kind:        "atomic",
			Description: "挥手致意",
			Params: []protocol.CapabilityParam{
				{Name: "target_agent_id", Type: "string", Required: true},
				{Name: "duration_sec", Type: "number"},
			},
		},
	})
	list, count := prompt.BuildTacticalToolList(reg.EffectiveActions("H-01"))
	if count != 2 {
		t.Fatalf("tool count=%d, want 2 (move_to + wave_hand)", count)
	}
	if !strings.Contains(list, "- wave_hand [原子]:") {
		t.Errorf("tool list should contain wave_hand bullet with [原子] kind label, got: %s", list)
	}
	if !strings.Contains(list, "挥手致意") {
		t.Errorf("tool list should contain derived Desc '挥手致意', got: %s", list)
	}
	// Params hint should include both param names
	if !strings.Contains(list, "target_agent_id") || !strings.Contains(list, "duration_sec") {
		t.Errorf("tool list should include param names, got: %s", list)
	}
}

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

// TestBuildTacticalToolEntries_NilRegistryBuiltinFullSet verifies the nil
// registry fallback returns all 11 built-in tools (minus scan_area/stop/wait).
func TestBuildTacticalToolEntries_NilRegistryBuiltinFullSet(t *testing.T) {
	entries := prompt.ToolEntries(nil)
	// 14 built-in specs - scan_area - stop - wait = 11
	if len(entries) != 11 {
		t.Fatalf("nil registry entry count=%d, want 11 (all built-in minus scan_area/stop/wait)", len(entries))
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.Name] = true
	}
	if seen["scan_area"] || seen["stop"] || seen["wait"] {
		t.Errorf("scan_area/stop/wait should not appear in tactical tool list")
	}
	for _, name := range []string{
		"generic_act", "move_to", "turn_to",
		"speak", "emote", "interact",
		"work_shift", "charge_at_station", "self_maintenance",
		"rest_at_residence", "surf_internet",
	} {
		if !seen[name] {
			t.Errorf("missing built-in tool %q in nil-registry fallback", name)
		}
	}
}

// ─── buildTacticalExample (category-aware) ──────────────────

func TestBuildTacticalExample_ChargingStationFirst(t *testing.T) {
	// 回归测试：当前 KB 第一个 object（按 ID 排序）是 charge（category=charging），
	// 示例必须用 charge_at_station，不能用 work_shift 配 charging object。
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "")
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("example should use charge_at_station for charging category: %q", got)
	}
	if !strings.Contains(got, "charge") {
		t.Errorf("example should reference charge: %q", got)
	}
	if strings.Contains(got, "work_shift") {
		t.Errorf("example must NOT use work_shift for charging object: %q", got)
	}
}

func TestBuildTacticalExample_WorkbenchOnly(t *testing.T) {
	// KB 只含 workbench 时示例应用 work_shift。
	kb := &worldkb.KB{
		Zones:  []worldkb.Zone{{ID: "main_workshop", DisplayName: "车间"}},
		Objects: []worldkb.Object{{
			ID:                    "wb_01",
			DisplayName:           "工作台",
			Category:              "workbench",
			ZoneID:                "main_workshop",
			AvailableInteractions: []string{"assemble", "inspect"},
		}},
	}
	got := prompt.TacticalExample(kb, "")
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
		Zones:  []worldkb.Zone{{ID: "rest_area", DisplayName: "休息区"}},
		Objects: []worldkb.Object{{
			ID:                    "bench_01",
			DisplayName:           "长椅",
			Category:              "rest_bench",
			ZoneID:                "rest_area",
			AvailableInteractions: []string{"rest"},
		}},
	}
	got := prompt.TacticalExample(kb, "")
	if !strings.Contains(got, `"action":"interact"`) {
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
	got := prompt.TacticalExample(nil, "")
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
	got := prompt.TacticalExample(kb, "")
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

func TestBuildTacticalExample_GoalAssembly(t *testing.T) {
	// goal 含"装配"应选 work_shift 示例，即使 KB 首个 object 是 charge。
	// P0-2 修复前：示例固定按首个 object（charge）显示"去充电"，
	// 与"装配作业"goal 错配，LLM 模仿后会把 goal 和示例机械拼接。
	// 注意：KB 中可能有多个 category=work 的物体（sortingconveyor、workbench），
	// findObjectByCategory 取首个匹配，故任一出现都算通过。
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "前往主生产车间进行装配作业，严控工艺")
	if !strings.Contains(got, "work_shift") {
		t.Errorf("goal=装配 should pick work_shift example: %q", got)
	}
	// 示例应引用某个 work category 物体（sortingconveyor 或 workbench）。
	hasWorkObj := strings.Contains(got, "sortingconveyor") || strings.Contains(got, "workbench")
	if !hasWorkObj {
		t.Errorf("example should reference a work-category object (sortingconveyor or workbench): %q", got)
	}
	if strings.Contains(got, "charge_at_station") {
		t.Errorf("assembly goal must NOT fall back to charge example: %q", got)
	}
	// 2026-08-11 修复：复合动作示例不应含 move_to（复合动作自带移动）。
	if strings.Contains(got, `"action":"move_to"`) {
		t.Errorf("work_shift example must NOT contain move_to (composite includes movement): %q", got)
	}
}

func TestBuildTacticalExample_GoalCharge(t *testing.T) {
	// goal 含"充电"应选 charge_at_station 示例。
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "午间停工，前往充电站补电休息")
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("goal=充电 should pick charge_at_station example: %q", got)
	}
	if !strings.Contains(got, "charge") {
		t.Errorf("example should reference charge: %q", got)
	}
	// 2026-08-11 修复：复合动作示例不应含 move_to（复合动作自带移动）。
	if strings.Contains(got, `"action":"move_to"`) {
		t.Errorf("charge_at_station example must NOT contain move_to (composite includes movement): %q", got)
	}
}

func TestBuildTacticalExample_GoalPatrol(t *testing.T) {
	// goal 含"巡视"应选 move_to + generic_act 示例，不引用任何 object。
	// 新 12 cmd 体系无 patrol_zone，巡视用 generic_act(behavior=look_around) 兜底。
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "巡视主生产车间，记录设备运行日志")
	if !strings.Contains(got, "generic_act") {
		t.Errorf("goal=巡视 should pick generic_act example: %q", got)
	}
	if strings.Contains(got, "work_shift") || strings.Contains(got, "charge_at_station") {
		t.Errorf("patrol goal must NOT fall back to object examples: %q", got)
	}
}

func TestBuildTacticalExample_GoalInspect(t *testing.T) {
	// goal 含"检查"应选 interact inspect 示例。
	// 真 KB 物体没有 inspect interaction，用 inline KB 验证此分支。
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
	got := prompt.TacticalExample(kb, "启动自检，检查关节磨损情况")
	if !strings.Contains(got, `"action":"interact"`) {
		t.Errorf("goal=检查 should pick interact example: %q", got)
	}
	if !strings.Contains(got, `"interaction":"inspect"`) {
		t.Errorf("inspect goal should use interaction=inspect: %q", got)
	}
}

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
	got := prompt.TacticalExample(kb, "上午装配作业")
	// 应降级到 charge_at_station（首个 object 的 category）
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("assembly goal with no workbench should fall back to charge example: %q", got)
	}
}

func TestBuildTacticalExample_GoalEmptyFallback(t *testing.T) {
	// 空 goal 应降级到默认示例（首个 object 的 category）
	kb := loadTestKB(t)
	got := prompt.TacticalExample(kb, "")
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("empty goal should fall back to first-object example: %q", got)
	}
}

// ─── physicalAlertOverrideGoal ────────────────────────────────

func TestPhysicalAlertOverrideGoal_NoAlert(t *testing.T) {
	origGoal := "车间装配作业"
	// hint 不含"物理状态告警"标记 → 不 override
	got, ok := physicalAlertOverrideGoal("上次中断原因：疲劳过高", origGoal, &protocol.PhysicalState{Fatigue: 80})
	if ok {
		t.Errorf("non-alert hint should not override, got goal=%q override=true", got)
	}
	if got != origGoal {
		t.Errorf("non-alert hint should keep orig goal, got=%q want=%q", got, origGoal)
	}
}

func TestPhysicalAlertOverrideGoal_NilPhysical(t *testing.T) {
	origGoal := "车间装配作业"
	got, ok := physicalAlertOverrideGoal("物理状态告警自动升级(疲劳=62超过60)", origGoal, nil)
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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "前往充电站休息", Zone: "main_workshop", TimeOfDay: "13:15", Slot: "13:00-17:00", Physical: &protocol.PhysicalState{Energy: 88, Fatigue: 82, JointWear: 0}, KB: kb, Hint: hint, Actions: nil, AgentID: "H-01"})

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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "车间装配", Zone: "main_workshop", TimeOfDay: "14:00", Slot: "13:00-17:00", Physical: &protocol.PhysicalState{Energy: 88, Fatigue: 30, JointWear: 75}, KB: kb, Hint: hint, Actions: nil, AgentID: "H-01"})

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
	promptText := prompt.BuildTactical(prompt.TacticalInput{Goal: "车间装配", Zone: "main_workshop", TimeOfDay: "09:00", Slot: "09:00-12:00", Physical: &protocol.PhysicalState{Energy: 90, Fatigue: 20, JointWear: 0}, KB: kb, Hint: "上次中断原因：zone 变化", Actions: nil, AgentID: "H-01"})

	if strings.Contains(promptText, "【物理告警强制约束】") {
		t.Errorf("non-physical-alert hint should NOT contain constraint section, got: %s", promptText)
	}
}
