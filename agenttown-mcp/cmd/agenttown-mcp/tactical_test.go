package main

import (
	"context"
	"errors"
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

// ─── parseTacticalPlan ───────────────────────────────────────

func TestParseTacticalPlan_ValidJSON(t *testing.T) {
	raw := `{"inner_thought":"先去车间再装配","actions":[{"action":"move_to","params":{"target":"main_workshop"}},{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}]}`
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.InnerThought != "先去车间再装配" {
		t.Errorf("inner_thought=%q", plan.InnerThought)
	}
	if len(plan.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(plan.Actions))
	}
	if plan.Actions[0].Action != "move_to" {
		t.Errorf("action[0]=%q", plan.Actions[0].Action)
	}
}

func TestParseTacticalPlan_JSONFence(t *testing.T) {
	raw := "```json\n{\"inner_thought\":\"充电\",\"actions\":[{\"action\":\"charge_at\",\"params\":{\"station_id\":\"charging_station_01\",\"duration_min\":60}}]}\n```"
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Action != "charge_at" {
		t.Errorf("actions=%+v", plan.Actions)
	}
}

func TestParseTacticalPlan_NarrativePrefix(t *testing.T) {
	raw := `好的，我来分解这个任务：` + "\n" +
		`{"inner_thought":"开始工作","actions":[{"action":"wait","params":{"duration_sec":30}}]}` + "\n" +
		`以上就是我的计划。`
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Action != "wait" {
		t.Errorf("actions=%+v", plan.Actions)
	}
}

func TestParseTacticalPlan_Malformed(t *testing.T) {
	raw := "我今天打算去车间看看，然后再去充电。"
	if _, err := parseTacticalPlan(raw); err == nil {
		t.Fatal("expected error for narrative without JSON object")
	}
}

func TestParseTacticalPlan_Empty(t *testing.T) {
	if _, err := parseTacticalPlan(""); err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestParseTacticalPlan_EmptyActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么","actions":[]}`
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Actions) != 0 {
		t.Errorf("got %d actions, want 0 after filter", len(plan.Actions))
	}
}

func TestParseTacticalPlan_FiltersScanAreaAndStop(t *testing.T) {
	raw := `{"inner_thought":"扫描一下","actions":[{"action":"scan_area","params":{}},{"action":"move_to","params":{"target":"main_workshop"}},{"action":"stop","params":{}}]}`
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1 (scan_area/stop filtered)", len(plan.Actions))
	}
	if plan.Actions[0].Action != "move_to" {
		t.Errorf("remaining action=%q, want move_to", plan.Actions[0].Action)
	}
}

func TestParseTacticalPlan_DurationMinInt(t *testing.T) {
	// LLM 可能输出 duration_min 为 int 而非 float（JSON 里 240 而非 240.0）
	raw := `{"inner_thought":"装配","actions":[{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}]}`
	plan, err := parseTacticalPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(plan.Actions))
	}
	// 验证 toFloat 能处理 int
	dur := toFloat(plan.Actions[0].Params["duration_min"])
	if dur != 240 {
		t.Errorf("duration_min toFloat=%v, want 240", dur)
	}
}

// ─── mapTacticalAction ───────────────────────────────────────

func TestMapTacticalAction_Composite(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "work_assemble", Params: map[string]any{"target": "workbench_01", "duration_min": float64(240)}}
	cmd, params, err := mapTacticalAction(pa, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdExecuteComposite {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdExecuteComposite)
	}
	if params["name"] != "work_assemble" {
		t.Errorf("name=%v", params["name"])
	}
	if params["target"] != "workbench_01" {
		t.Errorf("target=%v", params["target"])
	}
	if params["duration_sec"] != float64(14400) {
		t.Errorf("duration_sec=%v, want 14400", params["duration_sec"])
	}
}

func TestMapTacticalAction_MoveToResolvesKB(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "move_to", Params: map[string]any{"target": "main_workshop"}}
	cmd, params, err := mapTacticalAction(pa, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdMoveTo {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdMoveTo)
	}
	dest, ok := params["dest"].([]float64)
	if !ok || len(dest) != 3 {
		t.Fatalf("dest=%v, want []float64 len 3", params["dest"])
	}
	if params["target"] != "main_workshop" {
		t.Errorf("target=%v", params["target"])
	}
	if params["kind"] != "zone" {
		t.Errorf("kind=%v, want zone", params["kind"])
	}
	if params["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", params["speed"])
	}
}

func TestMapTacticalAction_MoveToUnknownTarget(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "move_to", Params: map[string]any{"target": "nonexistent_place"}}
	if _, _, err := mapTacticalAction(pa, kb); err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestMapTacticalAction_Speak(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "speak", Params: map[string]any{"content": "你好", "target": "H-02"}}
	cmd, params, err := mapTacticalAction(pa, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != protocol.CmdSpeak {
		t.Errorf("cmd=%q, want %q", cmd, protocol.CmdSpeak)
	}
	if params["content"] != "你好" {
		t.Errorf("content=%v", params["content"])
	}
	if params["target"] != "H-02" {
		t.Errorf("target=%v", params["target"])
	}
	if params["audio_url"] != nil {
		t.Errorf("audio_url=%v, want nil", params["audio_url"])
	}
}

func TestMapTacticalAction_EmoteDefaultMode(t *testing.T) {
	kb := loadTestKB(t)
	pa := plannedAction{Action: "emote", Params: map[string]any{"emotion": "happy"}}
	cmd, params, err := mapTacticalAction(pa, kb)
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
	_, params, err := mapTacticalAction(pa, kb)
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
	if _, _, err := mapTacticalAction(pa, kb); err == nil {
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
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, slog.Default())
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
	raw := `{"inner_thought":"先移动再装配","actions":[{"action":"move_to","params":{"target":"main_workshop"}},{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}]}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, slog.Default())
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
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", nil, slog.Default()); err == nil {
		t.Fatal("expected error on parse failure")
	}
}

func TestGenerateTacticalPlan_EmptyActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么","actions":[]}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", nil, slog.Default()); err == nil {
		t.Fatal("expected error when all actions filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	raw := `{"inner_thought":"开始","actions":[{"action":"wait","params":{"duration_sec":30}}]}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	_, _, _ = generateTacticalPlan(context.Background(), tc, "H-01", "等待", "main_workshop", "09:00", nil, slog.Default())
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
	}
}

// ─── buildTacticalPrompt ─────────────────────────────────────

func TestBuildTacticalPrompt_NilPhysical(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", nil)
	if prompt == "" {
		t.Fatal("prompt should not be empty")
	}
	// nil physical 时各值应为 0
	if !strings.Contains(prompt, "能量 0") {
		t.Errorf("prompt should contain '能量 0' for nil physical, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_WithPhysical(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90})
	if !strings.Contains(prompt, "能量 75") {
		t.Errorf("prompt should contain '能量 75', got: %s", prompt)
	}
	if !strings.Contains(prompt, "疲劳 30") {
		t.Errorf("prompt should contain '疲劳 30'")
	}
}
