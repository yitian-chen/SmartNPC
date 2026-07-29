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

// ─── parseTacticalNDJSON ─────────────────────────────────────

func TestParseTacticalNDJSON_Valid(t *testing.T) {
	raw := `{"inner_thought":"先去车间再装配"}` + "\n" +
		`{"action":"move_to","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}`
	actions, thought, err := parseTacticalNDJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if thought != "先去车间再装配" {
		t.Errorf("inner_thought=%q", thought)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(actions))
	}
	if actions[0].Action != "move_to" {
		t.Errorf("action[0]=%q", actions[0].Action)
	}
}

func TestParseTacticalNDJSON_WithFence(t *testing.T) {
	raw := "```json\n" +
		`{"inner_thought":"充电"}` + "\n" +
		`{"action":"charge_at","params":{"station_id":"charging_station_01","duration_min":60}}` + "\n" +
		"```"
	actions, _, err := parseTacticalNDJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0].Action != "charge_at" {
		t.Errorf("actions=%+v", actions)
	}
}

func TestParseTacticalNDJSON_BlankLines(t *testing.T) {
	raw := `{"inner_thought":"开始"}` + "\n\n" +
		`{"action":"wait","params":{"duration_sec":30}}` + "\n" +
		"\n"
	actions, thought, err := parseTacticalNDJSON(raw)
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
	actions, _, err := parseTacticalNDJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 || actions[0].Action != "wait" {
		t.Errorf("actions=%+v, want 1 wait", actions)
	}
}

func TestParseTacticalNDJSON_Empty(t *testing.T) {
	actions, thought, err := parseTacticalNDJSON("")
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
	actions, thought, err := parseTacticalNDJSON(raw)
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
		`{"action":"move_to","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"stop","params":{}}`
	actions, _, err := parseTacticalNDJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1 (scan_area/stop filtered)", len(actions))
	}
	if actions[0].Action != "move_to" {
		t.Errorf("remaining action=%q, want move_to", actions[0].Action)
	}
}

func TestParseTacticalNDJSON_DurationMinInt(t *testing.T) {
	// LLM 可能输出 duration_min 为 int 而非 float（JSON 里 240 而非 240.0）
	raw := `{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}`
	actions, _, err := parseTacticalNDJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(actions))
	}
	// 验证 toFloat 能处理 int
	dur := toFloat(actions[0].Params["duration_min"])
	if dur != 240 {
		t.Errorf("duration_min toFloat=%v, want 240", dur)
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
	acc.feed(`{"action":"move_to","params":{"targ`)
	acc.feed(`et":"main_workshop"}}` + "\n")
	if len(collected) != 1 {
		t.Fatalf("after second line: collected=%d, want 1", len(collected))
	}
	if collected[0].Action != "move_to" {
		t.Errorf("collected[0]=%q, want move_to", collected[0].Action)
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
	acc.feed(`{"action":"move_to","params":{"target":"main_workshop"}}` + "\n")
	acc.flush()
	if len(collected) != 1 {
		t.Fatalf("collected=%d, want 1 (scan_area filtered)", len(collected))
	}
	if collected[0].Action != "move_to" {
		t.Errorf("collected[0]=%q, want move_to", collected[0].Action)
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
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, nil, slog.Default(), "")
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
		`{"action":"move_to","params":{"target":"main_workshop"}}` + "\n" +
		`{"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	actions, thought, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 80, Fatigue: 20, JointWear: 10, Health: 100}, nil, slog.Default(), "")
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
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), ""); err == nil {
		t.Fatal("expected error on parse failure (no actions)")
	}
}

func TestGenerateTacticalPlan_EmptyActions(t *testing.T) {
	raw := `{"inner_thought":"不知道做什么"}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	if _, _, err := generateTacticalPlan(context.Background(), tc, "H-01", "装配", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), ""); err == nil {
		t.Fatal("expected error when all actions filtered out")
	}
}

func TestGenerateTacticalPlan_ResetSessionCalled(t *testing.T) {
	raw := `{"inner_thought":"开始"}` + "\n" +
		`{"action":"wait","params":{"duration_sec":30}}`
	tc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	_, _, _ = generateTacticalPlan(context.Background(), tc, "H-01", "等待", "main_workshop", "09:00", "09:00-12:00", nil, nil, slog.Default(), "")
	if !tc.resetCalled {
		t.Error("ResetSession should be called after successful tactical generation")
	}
}

// ─── buildTacticalPrompt ─────────────────────────────────────

func TestBuildTacticalPrompt_NilPhysical(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "", nil, nil, "")
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
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00", &protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "")
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
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, kb, "")
	// 应包含所有区域
	if !strings.Contains(prompt, "main_workshop") || !strings.Contains(prompt, "central_plaza") ||
		!strings.Contains(prompt, "charging_station") || !strings.Contains(prompt, "rest_area") {
		t.Errorf("prompt should list all 4 zones, got: %s", prompt)
	}
	// 应包含所有地点
	if !strings.Contains(prompt, "workbench_01") || !strings.Contains(prompt, "charging_station_01") ||
		!strings.Contains(prompt, "rest_bench_01") {
		t.Errorf("prompt should list all 3 locations, got: %s", prompt)
	}
	// 应包含可交互物体及其动作
	if !strings.Contains(prompt, "可交互物体") {
		t.Errorf("prompt should contain '可交互物体' section, got: %s", prompt)
	}
	if !strings.Contains(prompt, "assemble") || !strings.Contains(prompt, "charge") || !strings.Contains(prompt, "rest") {
		t.Errorf("prompt should list available actions on objects, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_NilKB(t *testing.T) {
	// nil KB 时不应崩溃，也不应包含 KB 上下文段落
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "", nil, nil, "")
	if strings.Contains(prompt, "可前往区域") {
		t.Errorf("prompt should not contain '可前往区域' when KB is nil, got: %s", prompt)
	}
	if strings.Contains(prompt, "可交互物体") {
		t.Errorf("prompt should not contain '可交互物体' when KB is nil, got: %s", prompt)
	}
}

func TestBuildTacticalPrompt_WithHint(t *testing.T) {
	prompt := buildTacticalPrompt("装配", "main_workshop", "09:00", "09:00-12:00",
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil,
		"fatigue=72 已突破警戒带，当前装配任务不合理")
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
		&protocol.PhysicalState{Energy: 75, Fatigue: 30, JointWear: 5, Health: 90}, nil, "")
	if strings.Contains(prompt, "【上次中断原因】") {
		t.Errorf("prompt should not contain '【上次中断原因】' when hint is empty, got: %s", prompt)
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
