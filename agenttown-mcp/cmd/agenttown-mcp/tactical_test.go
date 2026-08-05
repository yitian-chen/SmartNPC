package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

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
	raw := `{"inner_thought":"先去车间再装配"}` + "\n" +
		`{"action":"move_to_location","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"work_at_workbench","params":{"target_object_id":"workbench_01","duration_sec":14400}}`
	actions, thought, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thought != "先去车间再装配" {
		t.Errorf("inner_thought=%q", thought)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if actions[0].Action != "move_to_location" {
		t.Errorf("action[0]=%q", actions[0].Action)
	}
}

func TestParseTacticalNDJSON_WithFence(t *testing.T) {
	raw := "```json\n" +
		`{"inner_thought":"充电"}` + "\n" +
		`{"action":"charge_at_station","params":{"target_object_id":"charging_station_01","duration_sec":3600}}` + "\n" +
		"```"
	actions, _, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0].Action != "charge_at_station" {
		t.Errorf("actions=%+v", actions)
	}
}

func TestParseTacticalNDJSON_BlankLines(t *testing.T) {
	raw := `{"inner_thought":"开始"}` + "\n\n" +
		`{"action":"wait","params":{"duration_sec":30}}` + "\n" +
		"\n"
	actions, thought, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thought != "开始" {
		t.Errorf("thought=%q", thought)
	}
	if len(actions) != 1 || actions[0].Action != "wait" {
		t.Errorf("actions=%+v", actions)
	}
}

func TestParseTacticalNDJSON_MalformedLine(t *testing.T) {
	// 单行 parse 失败应跳过，不影响其他行
	raw := `{"inner_thought":"计划"}` + "\n" +
		`这不是JSON` + "\n" +
		`{"action":"wait","params":{"duration_sec":30}}`
	actions, _, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0].Action != "wait" {
		t.Errorf("actions=%+v, want 1 wait", actions)
	}
}

func TestParseTacticalNDJSON_Empty(t *testing.T) {
	actions, thought, err := parseTacticalNDJSON("", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("got %d actions, want 0", len(actions))
	}
	if thought != "" {
		t.Errorf("thought=%q, want empty", thought)
	}
}

func TestParseTacticalNDJSON_ThoughtOnly(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}`
	actions, thought, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("got %d actions, want 0", len(actions))
	}
	if thought != "不知道做什么" {
		t.Errorf("thought=%q", thought)
	}
}

func TestParseTacticalNDJSON_FiltersScanAreaAndStop(t *testing.T) {
	raw := `{"inner_thought":"扫描一下"}` + "\n" +
		`{"action":"scan_area","params":{}}` + "\n" +
		`{"action":"move_to_location","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"stop","params":{}}`
	actions, _, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1 (scan_area/stop filtered)", len(actions))
	}
	if actions[0].Action != "move_to_location" {
		t.Errorf("remaining action=%q, want move_to_location", actions[0].Action)
	}
}

func TestParseTacticalNDJSON_DurationMinInt(t *testing.T) {
	// LLM 可能输出 duration_sec 为 int 而非 float（JSON 里 14400 而非 14400.0）
	raw := `{"action":"work_at_workbench","params":{"target_object_id":"workbench_01","duration_sec":14400}}`
	actions, _, err := parseTacticalNDJSON(raw, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	// 验证 toFloat 能处理 int
	dur := toFloat(actions[0].Params["duration_sec"])
	if dur != 14400 {
		t.Errorf("duration_sec toFloat=%v, want 14400", dur)
	}
}

// ─── streamAccumulator ───────────────────────────────────────

func TestStreamAccumulator_Feed(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}

	// 模拟流式 delta：第一行 inner_thought 完整到达，第二行 action 分两次到达
	acc.feed(`{"inner_thought":"开工"}` + "\n")
	if acc.thought != "开工" {
		t.Errorf("after first feed: thought=%q, want 开工", acc.thought)
	}
	if len(collected) != 0 {
		t.Errorf("after first feed: collected=%d, want 0", len(collected))
	}

	// 第二行被拆成两个 delta
	acc.feed(`{"action":"move_to_location","params":{"targ`)
	acc.feed(`et":"main_workshop"}}` + "\n")
	if len(collected) != 1 {
		t.Fatalf("after second line: collected=%d, want 1", len(collected))
	}
	if collected[0].Action != "move_to_location" {
		t.Errorf("collected[0]=%q, want move_to_location", collected[0].Action)
	}

	// 第三行不完整（无 \n），不应触发 onComplete
	acc.feed(`{"action":"wait","params":{"duration_sec":30}}`)
	if len(collected) != 1 {
		t.Errorf("incomplete line should not trigger: collected=%d, want 1", len(collected))
	}

	// flush 处理残余
	acc.flush()
	if len(collected) != 2 {
		t.Fatalf("after flush: collected=%d, want 2", len(collected))
	}
	if collected[1].Action != "wait" {
		t.Errorf("collected[1]=%q, want wait", collected[1].Action)
	}
}

func TestStreamAccumulator_FiltersInvalidAction(t *testing.T) {
	var collected []plannedAction
	acc := &streamAccumulator{
		onComplete: func(pa plannedAction) { collected = append(collected, pa) },
	}
	acc.feed(`{"action":"scan_area","params":{}}` + "\n")
	acc.feed(`{"action":"move_to_location","params":{"target":"main_workshop"}}` + "\n")
	acc.flush()
	if len(collected) != 1 {
		t.Fatalf("collected=%d, want 1 (scan_area filtered)", len(collected))
	}
	if collected[0].Action != "move_to_location" {
		t.Errorf("collected[0]=%q, want move_to", collected[0].Action)
	}
}

// ─── mapTacticalAction ───────────────────────────────────────

func TestMapTacticalAction_Composite(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "work_at_workbench", Params: map[string]any{"target_object_id": "workbench_01", "duration_sec": float64(14400)}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdWorkAtWorkbench {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdWorkAtWorkbench)
	}
	if params["target_object_id"] != "workbench_01" {
		t.Errorf("target_object_id=%v", params["target_object_id"])
	}
	if params["duration_sec"] != float64(14400) {
		t.Errorf("duration_sec=%v, want 14400", params["duration_sec"])
	}
}

func TestMapTacticalAction_MoveToResolvesKB(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "move_to_location", Params: map[string]any{"target": "main_workshop"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdMoveToLocation {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdMoveToLocation)
	}
	dest, ok := params["dest"].([]float64)
	if !ok || len(dest) != 3 {
		t.Fatalf("dest=%v, want []float64 len 3", params["dest"])
	}
	if params["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", params["speed"])
	}
}

func TestMapTacticalAction_MoveToUnknownTarget(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "move_to_location", Params: map[string]any{"target": "nonexistent_place"}}
	if _, _, err := mapTacticalAction(pa, "", kb, nil); err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestMapTacticalAction_Speak(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "speak", Params: map[string]any{"content": "你好", "target_agent_id": "H-02"}}
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
	if params["target_agent_id"] != "H-02" {
		t.Errorf("target_agent_id=%v", params["target_agent_id"])
	}
}

func TestMapTacticalAction_EmoteDefaultMode(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "emote", Params: map[string]any{"emotion": "happy"}}
	cmd, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdEmote {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdEmote)
	}
	if params["mode"] != "oneshot" {
		t.Errorf("mode=%v, want oneshot (default)", params["mode"])
	}
}

func TestMapTacticalAction_EmoteSustainedMode(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "emote", Params: map[string]any{"emotion": "sad", "mode": "sustained"}}
	_, params, err := mapTacticalAction(pa, "", kb, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params["mode"] != "sustained" {
		t.Errorf("mode=%v, want sustained", params["mode"])
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

// ─── generateTacticalPlan ────────────────────────────────────

func TestGenerateTacticalPlan_HTTPError(t *testing.T) {
	tc := &fakeStrategicCaller{err: errors.New("network down")}
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, nil, slog.Default(), "", nil)
	if err == nil {
		t.Fatal("expected error on HTTP failure")
	}
	if actions != nil || thought != "" {
		t.Errorf("got actions=%v thought=%q, want nil/empty", actions, thought)
	}
	if tc.resetCalled {
		t.Error("ResetSession should not be called when SendWithSummary fails")
	}
}

func TestGenerateTacticalPlan_ValidResponse(t *testing.T) {
	raw := `{"inner_thought":"先移动再装配"}` + "\n" +
		`{"action":"move_to_location","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"work_at_workbench","params":{"target_object_id":"workbench_01","duration_sec":14400}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, nil, slog.Default(), "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if thought != "先移动再装配" {
		t.Errorf("thought=%q", thought)
	}
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful generation")
	}
}

func TestGenerateTacticalPlan_ParseFail(t *testing.T) {
	tc := &fakeStrategicCaller{resp: makeStrategicResponse("我今天打算去车间转转。")}
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), "", nil); err == nil {
		t.Fatal("expected error on parse failure (no actions)")
	}
}

func TestGenerateTacticalPlan_EmptyActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), "", nil); err == nil {
		t.Fatal("expected error when all actions filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	raw := `{"inner_thought":"开始"}` + "\n" +
		`{"action":"wait","params":{"duration_sec":30}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	_, _, _ = generateTacticalPlan(context.Background(), tc, "H-01", "等待", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), "", nil)
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
	}
}

// ─── buildTacticalPrompt ─────────────────────────────────────

func TestBuildTacticalPrompt_NilPhysical(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "", nil, nil, "", nil, "")
	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}
	// nil physical 时各值应为 0
	if !strings.Contains(prompt, "能量 0") {
		t.Errorf("prompt should contain '能量 0' for nil physical, got: %s", prompt)
	}
	// slot 为空时不应有时长提示行
	if strings.Contains(prompt, "请让步骤总时长接近此时长") {
		t.Errorf("prompt should not contain slot duration hint when slot is empty, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_WithPhysical(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "", nil, "")
	if !strings.Contains(prompt, "能量 75") {
		t.Errorf("prompt should contain '能量 75', got: %s", prompt)
	}
	if !strings.Contains(prompt, "疲劳 30") {
		t.Errorf("prompt should contain '疲劳 30'")
	}
	// slot 有效时应包含时长提示
	if !strings.Contains(prompt, "当前时段 09:00-12:00，约 180 分钟") {
		t.Errorf("prompt should contain slot duration hint, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_InjectsKBContext(t *testing.T) {
	kb := loadTestKB(t)
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, kb, "", nil, "")
	// 应包含 KB 中所有 zone（assets/world_kb.yaml 当前是 7-zone 工业园区）
	for _, zID := range []string{"main_workshop", "central_plaza", "logistics_hub", "repair_bay", "residential_quarters", "archive_station", "recycling_yard"} {
		if !strings.Contains(prompt, zID) {
			t.Errorf("prompt should list zone %q, got: %s", zID, prompt)
		}
	}
	// 应包含所有 object
	for _, oID := range []string{"workbench_01", "charging_station_01", "rest_bench_01"} {
		if !strings.Contains(prompt, oID) {
			t.Errorf("prompt should list object %q, got: %s", oID, prompt)
		}
	}
	// 应包含"可交互物体"段落标题及交互动词
	if !strings.Contains(prompt, "可交互物体") {
		t.Errorf("prompt should contain '可交互物体' section, got: %s", prompt)
	}
	if !strings.Contains(prompt, "assemble") || !strings.Contains(prompt, "charge") || !strings.Contains(prompt, "rest") {
		t.Errorf("prompt should list available interactions on objects, got: %s", prompt)
	}
	// 验证新格式：每个 object 单独一行，明确分离 id/zone/interaction
	// 不应再出现旧的 "id|zone[interactions]" 拼接格式
	if strings.Contains(prompt, "workbench_01|main_workshop[") {
		t.Errorf("prompt should not contain legacy 'id|zone[interactions]' format, got: %s", prompt)
	}
	// 应包含明确的 id/zone/interaction 标注
	if !strings.Contains(prompt, "id=workbench_01") {
		t.Errorf("prompt should contain 'id=workbench_01' label, got: %s", prompt)
	}
	if !strings.Contains(prompt, "位于 zone=main_workshop") {
		t.Errorf("prompt should contain '位于 zone=main_workshop', got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_NilKB(t *testing.T) {
	// nil KB 时不应崩溃，也不应包含 KB 上下文段落
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "", nil, nil, "", nil, "")
	// 不应出现 KB 段落标题（"可前往区域（..."、"可交互物体（..."）
	// 注意：示例 fallback 文本里会提到"上方可前往区域的 id"作为占位提示，
	// 这是引导文字而非 KB 内容，不应被此断言拦截——所以用更精确的段落标题匹配。
	if strings.Contains(prompt, "可前往区域（move_to_location") {
		t.Errorf("prompt should not contain '可前往区域' section when KB is nil, got: %s", prompt)
	}
	if strings.Contains(prompt, "可交互物体（interact") {
		t.Errorf("prompt should not contain '可交互物体' section when KB is nil, got: %s", prompt)
	}
}

// TestBuildTacticalPrompt_InjectsAgentRole 验证战术层 prompt 注入
// 【你的角色】段：传 kb + agentID="H-01" 后，prompt 应包含从
// buildAgentRoleContext 派生的角色画像（名字/职业/性格特质等）。
// 这是 C4 的核心——战术层分解动作时应体现 NPC 角色（如"老陈"的
// "沉稳"性格影响 action 选择与节奏），而非机械分解。
func TestBuildTacticalPrompt_InjectsAgentRole(t *testing.T) {
	kb := loadTestKB(t)
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90},
		kb, "", nil, "H-01")
	if !strings.Contains(prompt, "【你的角色】") {
		t.Errorf("prompt missing '【你的角色】' section header, got: %s", prompt)
	}
	for _, want := range []string{"老陈", "车间主管", "沉稳"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing role field %q, got: %s", want, prompt)
		}
	}
}

// TestBuildTacticalPrompt_NilKBNoRole 验证 kb==nil 时 prompt 不含
// 【你的角色】段（roleLine 降级为空串，prompt 中仅留空行）。
func TestBuildTacticalPrompt_NilKBNoRole(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "", nil, nil, "", nil, "")
	if strings.Contains(prompt, "【你的角色】") {
		t.Errorf("prompt should not contain '【你的角色】' when KB is nil, got: %s", prompt)
	}
}

// TestBuildTacticalPrompt_AgentNotFoundNoRole 验证 KB 存在但 agentID
// 不在 KB 中时也降级跳过【你的角色】段（buildAgentRoleContext 返回空串）。
func TestBuildTacticalPrompt_AgentNotFoundNoRole(t *testing.T) {
	kb := loadTestKB(t)
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "",
		nil, kb, "", nil, "NONEXISTENT-99")
	if strings.Contains(prompt, "【你的角色】") {
		t.Errorf("prompt should not include '【你的角色】' for unknown agent, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_WithHint(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil,
		"fatigue=72 已突破警戒带，当前装配任务不合理", nil, "")
	if !strings.Contains(prompt, "【上次中断原因】") {
		t.Errorf("prompt should contain '【上次中断原因】' when hint is non-empty, got: %s", prompt)
	}
	if !strings.Contains(prompt, "fatigue=72 已突破警戒带") {
		t.Errorf("prompt should contain the hint text, got: %s", prompt)
	}
	if !strings.Contains(prompt, "请据此调整本轮规划") {
		t.Errorf("prompt should contain adjustment guidance, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_NoHint(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "", nil, "")
	if strings.Contains(prompt, "【上次中断原因】") {
		t.Errorf("prompt should not contain '【上次中断原因】' when hint is empty, got: %s", prompt)
	}
}

// TestBuildTacticalPrompt_ZoneObjectDependency 验证 prompt 包含明确的
// zone-object 依赖约束，引导 LLM 在调用 interact/work_at_workbench/charge_at_station
// 前先 move_to_location 到 object 所在 zone（Fix A）。
func TestBuildTacticalPrompt_ZoneObjectDependency(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "", nil, "")
	if !strings.Contains(prompt, "interact / work_at_workbench / charge_at_station 必须在 object 所在 zone 调用") {
		t.Errorf("prompt should contain zone-object dependency rule, got: %s", prompt)
	}
	if !strings.Contains(prompt, "必须先 move_to_location 到该 zone") {
		t.Errorf("prompt should instruct to move_to_location first, got: %s", prompt)
	}
}

// ─── registry-aware tactical prompt / filtering ─────────────

func TestBuildTacticalPrompt_RegistryFiltersTools(t *testing.T) {
	// Registry with only CmdMoveToLocation + CmdWait available — composite tools
	// (which depend on composite cmds like CmdWorkAtWorkbench) should be filtered out.
	reg := NewCapabilityRegistry()
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "", reg, "H-01")
	// Tool bullet list should contain move_to_location and wait.
	if !strings.Contains(prompt, "- move_to_location [原子]:") || !strings.Contains(prompt, "- wait [原子]:") {
		t.Errorf("prompt should list move_to_location and wait as [原子] bullets, got: %s", prompt)
	}
	// Tool bullet list should NOT contain composite tools (composite cmds unavailable).
	// Check the bullet prefix specifically — the hardcoded example section
	// mentions work_at_workbench regardless, which is a separate prompt-quality concern.
	if strings.Contains(prompt, "- work_at_workbench [复合]:") || strings.Contains(prompt, "- charge_at_station [复合]:") {
		t.Errorf("prompt should NOT list composite tools as [复合] bullets (composite cmds unavailable), got: %s", prompt)
	}
	// Count in header should match available tools (2).
	if !strings.Contains(prompt, "仅限以下 2 个") {
		t.Errorf("prompt header should say '仅限以下 2 个', got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_PerAgentOverride(t *testing.T) {
	// Global has CmdMoveToLocation + CmdWorkAtWorkbench; per-agent H-02 only
	// has CmdMoveToLocation. H-02's prompt should NOT list composite tools.
	reg := NewCapabilityRegistry()
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
		{Cmd: protocol.CmdWorkAtWorkbench, Kind: "composite"},
	})
	reg.Register("H-02", []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
	})
	promptH01 := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75}, nil, "", reg, "H-01")
	promptH02 := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75}, nil, "", reg, "H-02")
	// Check bullet prefix — example section is hardcoded and not registry-aware.
	if !strings.Contains(promptH01, "- work_at_workbench [复合]:") {
		t.Errorf("H-01 prompt should list composite tools as [复合] bullets (global default), got: %s", promptH01)
	}
	if strings.Contains(promptH02, "- work_at_workbench [复合]:") {
		t.Errorf("H-02 prompt should NOT list composite tools as [复合] bullets (per-agent override), got: %s", promptH02)
	}
}

func TestFilterValidActions_RegistryFiltersCmd(t *testing.T) {
	reg := NewCapabilityRegistry()
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
		// CmdWorkAtWorkbench / CmdChargeAtStation absent → composite tools filtered.
	})
	actions := []plannedAction{
		{Action: "move_to_location", Params: map[string]any{"target": "main_workshop"}},
		{Action: "work_at_workbench", Params: map[string]any{"target": "workbench_01"}},
		{Action: "charge_at_station", Params: map[string]any{}},
	}
	got := filterValidActions(actions, reg, "H-01")
	if len(got) != 1 {
		t.Fatalf("got %d actions, want 1 (only move_to_location)", len(got))
	}
	if got[0].Action != "move_to_location" {
		t.Errorf("got action %q, want move_to_location", got[0].Action)
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
		// 解析失败 / 非法
		{"", -1},
		{"09:00", -1},
		{"09:00-09:00", -1}, // end == start
		{"09:00-08:00", -1}, // end < start
		{"abc-xyz", -1},
	}
	for _, c := range cases {
		got := slotDurationMinute(c.slot)
		if got != c.want {
			t.Errorf("slotDurationMinute(%q) = %d, want %d", c.slot, got, c.want)
		}
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildSlotDurationHint(c.slot, c.timeOfDay)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("buildSlotDurationHint(%q, %q) = %q, want empty", c.slot, c.timeOfDay, got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("buildSlotDurationHint(%q, %q) = %q, want substring %q", c.slot, c.timeOfDay, got, c.wantSub)
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Errorf("buildSlotDurationHint(%q, %q) = %q, should NOT contain %q", c.slot, c.timeOfDay, got, c.notWant)
			}
		})
	}
}

// ─── 动态 cmd 派生（Phase 2） ────────────────────────────────

// TestBuildTacticalToolList_NewCmdDerived verifies that a UE-pushed new cmd
// (not in BuiltinToolSpecs) appears in the tactical prompt tool list with
// Desc/Params derived from CapabilityAction metadata.
func TestBuildTacticalToolList_NewCmdDerived(t *testing.T) {
	reg := NewCapabilityRegistry()
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
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
	list, count := buildTacticalToolList("H-01", reg)
	if count != 2 {
		t.Fatalf("tool count=%d, want 2 (move_to_location + wave_hand)", count)
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
	reg := NewCapabilityRegistry()
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
	reg := NewCapabilityRegistry()
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
// registry fallback returns all 14 built-in tools (minus scan_area/stop).
func TestBuildTacticalToolEntries_NilRegistryBuiltinFullSet(t *testing.T) {
	entries := buildTacticalToolEntries("", nil)
	// 16 built-in specs - scan_area - stop = 14
	if len(entries) != 14 {
		t.Fatalf("nil registry entry count=%d, want 14 (all built-in minus scan_area/stop)", len(entries))
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		seen[e.Name] = true
	}
	if seen["scan_area"] || seen["stop"] {
		t.Errorf("scan_area/stop should not appear in tactical tool list")
	}
	for _, name := range []string{
		"move_to_location", "move_to_agent", "turn_to", "play_montage",
		"speak", "emote", "interact", "wait",
		"work_at_workbench", "work_at_workshop", "chat_with", "repair_target",
		"charge_at_station", "patrol_zone",
	} {
		if !seen[name] {
			t.Errorf("missing built-in tool %q in nil-registry fallback", name)
		}
	}
}

// ─── buildTacticalExample (category-aware) ──────────────────

func TestBuildTacticalExample_ChargingStationFirst(t *testing.T) {
	// 回归测试：当前 KB 第一个 object 是 charging_station_01（category=charging_station），
	// 示例必须用 charge_at_station，不能用 work_at_workbench 配 charging_station。
	kb := loadTestKB(t)
	got := buildTacticalExample(kb)
	if !strings.Contains(got, "charge_at_station") {
		t.Errorf("example should use charge_at_station for charging_station category: %q", got)
	}
	if !strings.Contains(got, "charging_station_01") {
		t.Errorf("example should reference charging_station_01: %q", got)
	}
	if strings.Contains(got, "work_at_workbench") {
		t.Errorf("example must NOT use work_at_workbench for charging station: %q", got)
	}
}

func TestBuildTacticalExample_WorkbenchOnly(t *testing.T) {
	// KB 只含 workbench 时示例应用 work_at_workbench。
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
	got := buildTacticalExample(kb)
	if !strings.Contains(got, "work_at_workbench") {
		t.Errorf("example should use work_at_workbench for workbench category: %q", got)
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
	got := buildTacticalExample(kb)
	if !strings.Contains(got, `"action":"interact"`) {
		t.Errorf("example should use interact for rest_bench category: %q", got)
	}
	if !strings.Contains(got, `"interaction":"rest"`) {
		t.Errorf("example should use interaction=rest: %q", got)
	}
	if strings.Contains(got, "work_at_workbench") || strings.Contains(got, "charge_at_station") {
		t.Errorf("example must NOT use composite tools for rest_bench: %q", got)
	}
}

func TestBuildTacticalExample_NilKB(t *testing.T) {
	// nil KB 返回通用占位示例，不引用任何具体 id。
	got := buildTacticalExample(nil)
	if !strings.Contains(got, "inner_thought") {
		t.Errorf("nil KB example should still contain inner_thought: %q", got)
	}
	if strings.Contains(got, "work_at_workbench") || strings.Contains(got, "charge_at_station") {
		t.Errorf("nil KB example should not reference specific composite tools: %q", got)
	}
}

func TestBuildTacticalExample_ZoneObjectPairing(t *testing.T) {
	// 回归测试：示例中 move_to_location 的 target 必须与示例 object 的 ZoneID
	// 一致。旧版取 ListZones()[0] 作示例 zone，但 ListZones()[0]=archive_station
	// 与 ListObjects()[0]=charging_station_01（在 central_plaza）不在同一 zone，
	// 示例本身错配，LLM 模仿后产生 zone-object 错配。
	kb := loadTestKB(t)
	objs := kb.ListObjects()
	if len(objs) == 0 {
		t.Skip("KB has no objects, pairing test not applicable")
	}
	firstObj := objs[0]
	wantZone := firstObj.ZoneID
	if wantZone == "" {
		t.Skip("first object has no ZoneID, cannot verify pairing")
	}
	got := buildTacticalExample(kb)
	// 示例应包含 move_to_location 到 wantZone，且引用 firstObj.ID。
	moveLine := fmt.Sprintf(`{"action":"move_to_location","params":{"target":"%s"}}`, wantZone)
	if !strings.Contains(got, moveLine) {
		t.Errorf("example should move_to_location(%s) to match object's ZoneID, got: %q", wantZone, got)
	}
	if !strings.Contains(got, firstObj.ID) {
		t.Errorf("example should reference first object %q, got: %q", firstObj.ID, got)
	}
	// 反向验证：不应出现 ListZones()[0]（若它与 object 的 ZoneID 不同）。
	firstZone := kb.ListZones()[0]
	if firstZone.ID != wantZone {
		badLine := fmt.Sprintf(`{"action":"move_to_location","params":{"target":"%s"}}`, firstZone.ID)
		if strings.Contains(got, badLine) {
			t.Errorf("example must NOT move_to_location(%s) — object %s is in %s, got: %q",
				firstZone.ID, firstObj.ID, wantZone, got)
		}
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
		&protocol.PhysicalState{Fatigue: 82, Energy: 80, Health: 100},
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
		&protocol.PhysicalState{Fatigue: 30, Energy: 35, Health: 100},
	)
	if !ok {
		t.Errorf("energy<40 should trigger override")
	}
	if !strings.Contains(got, "充电站") || !strings.Contains(got, "体力") {
		t.Errorf("override goal should mention 充电站 and 体力, got=%q", got)
	}
}

func TestPhysicalAlertOverrideGoal_HealthAlert(t *testing.T) {
	origGoal := "车间装配"
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警自动升级(健康=45低于50)",
		origGoal,
		&protocol.PhysicalState{Fatigue: 30, Energy: 80, Health: 45},
	)
	if !ok {
		t.Errorf("health<50 should trigger override")
	}
	if !strings.Contains(got, "维修") {
		t.Errorf("override goal should mention 维修, got=%q", got)
	}
}

func TestPhysicalAlertOverrideGoal_FatigueTakesPrecedence(t *testing.T) {
	// 同时 fatigue 高 + energy 低时，fatigue 优先（switch 顺序）
	got, ok := physicalAlertOverrideGoal(
		"物理状态告警",
		"工作",
		&protocol.PhysicalState{Fatigue: 85, Energy: 30, Health: 100},
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
	prompt := buildTacticalPrompt("前往充电站休息", "main_workshop", "13:15", "13:00-17:00",
		&protocol.PhysicalState{Energy: 88, Fatigue: 82, JointWear: 0, Health: 100},
		kb, hint, nil, "H-01")

	if !strings.Contains(prompt, "【物理告警强制约束】") {
		t.Errorf("prompt should contain physical alert constraint section, got: %s", prompt)
	}
	if !strings.Contains(prompt, "禁止规划 work_at_workbench") {
		t.Errorf("prompt should forbid work_at_workbench, got: %s", prompt)
	}
	if !strings.Contains(prompt, "优先 charge_at_station") {
		t.Errorf("prompt should prioritize charge_at_station, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_NoPhysicalAlertConstraint(t *testing.T) {
	kb := loadTestKB(t)
	// 普通 hint（无"物理状态告警"标记）不应插入强约束段
	prompt := buildTacticalPrompt("车间装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 90, Fatigue: 20, JointWear: 0, Health: 100},
		kb, "上次中断原因：zone 变化", nil, "H-01")

	if strings.Contains(prompt, "【物理告警强制约束】") {
		t.Errorf("non-physical-alert hint should NOT contain constraint section, got: %s", prompt)
	}
}
