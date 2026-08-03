package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// ─── 战术层队列辅助与 completion 路由 ──────────────────────────

// setQueueForTest 在测试中直接设置队列（绕过 mu 的 tacticalRefill 流程）。
func setQueueForTest(ac *agentContext, actions []plannedAction) {
	ac.mu.Lock()
	ac.actionQueue = actions
	ac.mu.Unlock()
}

func TestRecordActionCompletion_SignalsWorkerAndClearsInFlight(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	ac.mu.Lock()
	ac.currentActionID = "act_t1"
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	// 排空 wake 通道
	select {
	case <-ac.wake:
	default:
	}

	queued, _, _ := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act_t1", Result: protocol.ResultSuccess, Progress: 1,
	})
	if !queued {
		t.Fatal("completion should return true (handled)")
	}
	// 应 signal worker（wake 通道有值）
	select {
	case <-ac.wake:
		// good
	default:
		t.Fatal("completion should signal worker via wake channel")
	}
	// currentActionSrc / currentActionID 应已清空
	ac.mu.Lock()
	src := ac.currentActionSrc
	id := ac.currentActionID
	ac.mu.Unlock()
	if src != "" {
		t.Fatalf("currentActionSrc should be cleared, got %q", src)
	}
	if id != "" {
		t.Fatalf("currentActionID should be cleared, got %q", id)
	}
}

// TestRecordActionCompletion_SuccessNoTrigger 验证成功完成的 action 不触发
// 反应层（成功是常态，无需评估）。
func TestRecordActionCompletion_SuccessNoTrigger(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	queued, trigger, detail := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act_ok_1", Result: protocol.ResultSuccess, Progress: 1,
	})
	if !queued {
		t.Fatal("queued should be true")
	}
	if trigger != "" {
		t.Errorf("success should not trigger, got trigger=%q", trigger)
	}
	if detail != "" {
		t.Errorf("success should return empty detail, got %q", detail)
	}
}

// TestRecordActionCompletion_FailureTriggers 验证异常完成的 action 触发反应层，
// 且 detail 用 result 作为去抖维度（不含 action_id，避免去抖失效）。
func TestRecordActionCompletion_FailureTriggers(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	queued, trigger, detail := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act_fail_1", Result: protocol.ResultFailed, Progress: 0.3,
	})
	if !queued {
		t.Fatal("queued should be true")
	}
	if trigger != TriggerActionDone {
		t.Errorf("trigger: got %q, want %q", trigger, TriggerActionDone)
	}
	if !strings.Contains(detail, "failed") {
		t.Errorf("detail should mention result=failed: %q", detail)
	}
	if strings.Contains(detail, "act_fail_1") {
		t.Errorf("detail should NOT contain action_id (breaks dedup): %q", detail)
	}
}

func TestRecordEventNotification_ReturnsTrigger(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	setQueueForTest(ac, []plannedAction{
		{Action: "move_to", Params: map[string]any{"target": "main_workshop"}},
		{Action: "wait", Params: map[string]any{"duration_sec": 30}},
	})
	ac.mu.Lock()
	ac.currentSlot = "08:00-12:00"
	ac.redecomposeCount = 1
	ac.mu.Unlock()

	// 反应层 P0：recordEventNotification 返回 (TriggerEventNotify, detail)
	// 供 WS handler 异步触发 reactiveRunner。本测试验证签名 + 队列不被改动。
	trigger, detail := ac.recordEventNotification(protocol.EventNotificationPayload{
		EventID:         "evt_001",
		PerceptionLevel: "audible",
		Event:           map[string]any{"type": "alert"},
	})
	if trigger != TriggerEventNotify {
		t.Fatalf("trigger=%q, want %q", trigger, TriggerEventNotify)
	}
	if detail == "" {
		t.Error("detail should not be empty")
	}
	if !strings.Contains(detail, "evt_001") {
		t.Errorf("detail should contain event_id, got %q", detail)
	}
	if !strings.Contains(detail, "alert") {
		t.Errorf("detail should contain event type, got %q", detail)
	}

	// 队列应原样保留（recordEventNotification 不再触碰战术队列）
	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	slot := ac.currentSlot
	count := ac.redecomposeCount
	ac.mu.Unlock()
	if queueLen != 2 {
		t.Fatalf("queue should be preserved, got %d items", queueLen)
	}
	if slot != "08:00-12:00" {
		t.Errorf("currentSlot should be preserved, got %q", slot)
	}
	if count != 1 {
		t.Errorf("redecomposeCount should be preserved, got %d", count)
	}
}

func TestPopAndSendQueueAction_RefillOnBusyRejection(t *testing.T) {
	// 模拟 UE 拒绝（busy with composite）：SendAction 在未连接 ws 上一定失败。
	// 当 currentActionSrc == sourceTactical（有在途 composite）时，被拒 action
	// 应回填到队首，而不是 signal → 整队消耗光。
	ac, _ := newAgentContext(context.Background())
	ws := wsserver.New(wsserver.Options{}) // 未连接 → SendAction 失败
	logger := slog.Default()
	kb := loadTestKB(t)

	// 队列 3 个 action
	setQueueForTest(ac, []plannedAction{
		{Action: "wait", Params: map[string]any{"duration_sec": 30}},
		{Action: "wait", Params: map[string]any{"duration_sec": 60}},
		{Action: "wait", Params: map[string]any{"duration_sec": 90}},
	})

	// 有在途战术 action（最后一个已 pop 但未完成）
	ac.mu.Lock()
	ac.currentActionSrc = sourceTactical
	ac.mu.Unlock()

	ac.popAndSendQueueAction(context.Background(), "H-01", ws, kb, logger)

	// 回填后队列仍为 3，且队首仍是第一个 action
	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	firstAction := ""
	if queueLen > 0 {
		firstAction = ac.actionQueue[0].Action
	}
	ac.mu.Unlock()
	if queueLen != 3 {
		t.Fatalf("queue should be refilled to 3 after busy rejection, got %d", queueLen)
	}
	if firstAction != "wait" {
		t.Fatalf("first action should be 'wait' after refill, got %q", firstAction)
	}
}

func TestRecordActionStarted_SetsSource(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	ac.recordActionStarted("act_1", "MoveTo", map[string]any{"target": "main_workshop"}, 1, sourceTactical)
	ac.mu.Lock()
	src := ac.currentActionSrc
	id := ac.currentActionID
	ac.mu.Unlock()
	if src != sourceTactical {
		t.Fatalf("currentActionSrc=%q, want tactical", src)
	}
	if id != "act_1" {
		t.Fatalf("currentActionID=%q, want act_1", id)
	}

	ac.recordActionStarted("act_2", "Wait", map[string]any{"duration_sec": 30}, 2, sourceHermes)
	ac.mu.Lock()
	src = ac.currentActionSrc
	id = ac.currentActionID
	ac.mu.Unlock()
	if src != sourceHermes {
		t.Fatalf("currentActionSrc=%q, want hermes", src)
	}
	if id != "act_2" {
		t.Fatalf("currentActionID=%q, want act_2", id)
	}
}

// ─── /debug/action ─────────────────────────────────────────────

func TestMapDebugCmd(t *testing.T) {
	cases := []struct {
		cmd      string
		wantCmd  string
		wantOK   bool
	}{
		{"move_to_location", protocol.CmdMoveToLocation, true},
		{"move_to_agent", protocol.CmdMoveToAgent, true},
		{"turn_to", protocol.CmdTurnTo, true},
		{"play_montage", protocol.CmdPlayMontage, true},
		{"speak", protocol.CmdSpeak, true},
		{"emote", protocol.CmdEmote, true},
		{"interact", protocol.CmdInteractSmartObject, true},
		{"wait", protocol.CmdWait, true},
		{"work_at_workbench", protocol.CmdWorkAtWorkbench, true},
		{"work_at_workshop", protocol.CmdWorkAtWorkshop, true},
		{"chat_with", protocol.CmdChatWith, true},
		{"repair_target", protocol.CmdRepairTarget, true},
		{"charge_at_station", protocol.CmdChargeAtStation, true},
		{"patrol_zone", protocol.CmdPatrolZone, true},
		{"unknown_cmd", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := mapDebugCmd(c.cmd)
		if ok != c.wantOK {
			t.Errorf("mapDebugCmd(%q) ok=%v, want %v", c.cmd, ok, c.wantOK)
			continue
		}
		if ok && got != c.wantCmd {
			t.Errorf("mapDebugCmd(%q) = %q, want %q", c.cmd, got, c.wantCmd)
		}
	}
}

func TestResolveDebugMoveToLocation_Valid(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"target": "workbench_01"}
	out, err := resolveDebugMoveToLocation(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, ok := out["dest"].([]float64)
	if !ok {
		t.Fatalf("dest should be []float64, got %T", out["dest"])
	}
	if len(dest) != 3 {
		t.Fatalf("dest should have 3 coords, got %d", len(dest))
	}
	if out["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", out["speed"])
	}
}

func TestResolveDebugMoveToLocation_Zone(t *testing.T) {
	kb := loadTestKB(t)
	// main_workshop 是 zone，应解析成功
	out, err := resolveDebugMoveToLocation(map[string]any{"target": "main_workshop"}, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, ok := out["dest"].([]float64)
	if !ok || len(dest) != 3 {
		t.Fatalf("dest should be [3]float64, got %T len %d", out["dest"], len(dest))
	}
}

func TestResolveDebugMoveToLocation_UnknownTarget(t *testing.T) {
	kb := loadTestKB(t)
	_, err := resolveDebugMoveToLocation(map[string]any{"target": "nonexistent_place"}, kb)
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestResolveDebugMoveToLocation_EmptyTarget(t *testing.T) {
	kb := loadTestKB(t)
	_, err := resolveDebugMoveToLocation(map[string]any{"target": ""}, kb)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestResolveDebugMoveToLocation_NilKB(t *testing.T) {
	_, err := resolveDebugMoveToLocation(map[string]any{"target": "workbench_01"}, nil)
	if err == nil {
		t.Fatal("expected error when kb is nil")
	}
}

// ─── move_to_location 坐标直传 ──────────────────────────

func TestResolveDebugMoveToLocation_DestCoords(t *testing.T) {
	kb := loadTestKB(t)
	// 直接传 dest 坐标，不走 kb 解析
	params := map[string]any{"dest": []any{10000.0, 20000.0, 0.0}}
	out, err := resolveDebugMoveToLocation(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, ok := out["dest"].([]float64)
	if !ok {
		t.Fatalf("dest should be []float64, got %T", out["dest"])
	}
	if len(dest) != 3 || dest[0] != 10000 || dest[1] != 20000 || dest[2] != 0 {
		t.Fatalf("dest=%v, want [10000 20000 0]", dest)
	}
	if out["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", out["speed"])
	}
}

func TestResolveDebugMoveToLocation_DestWithIntCoords(t *testing.T) {
	// JSON 解码整数常会变 float64，但也支持 int / int64
	kb := loadTestKB(t)
	params := map[string]any{"dest": []any{10000, 20000, 0}}
	out, err := resolveDebugMoveToLocation(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, _ := out["dest"].([]float64)
	if len(dest) != 3 || dest[0] != 10000 || dest[1] != 20000 {
		t.Fatalf("dest=%v, want [10000 20000 0]", dest)
	}
}

func TestResolveDebugMoveTo_DestWithTargetLabel(t *testing.T) {
	// dest + target 同时传：dest 优先，target 在 dest 模式下被忽略
	kb := loadTestKB(t)
	params := map[string]any{
		"dest":   []any{15000.0, 11000.0, 0.0},
		"target": "custom_spot",
	}
	out, err := resolveDebugMoveToLocation(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, _ := out["dest"].([]float64)
	if len(dest) != 3 || dest[0] != 15000 || dest[1] != 11000 {
		t.Errorf("dest=%v, want [15000 11000 0] (dest 优先)", dest)
	}
	if _, ok := out["target"]; ok {
		t.Errorf("target should not be preserved in dest mode, got: %v", out["target"])
	}
}

func TestResolveDebugMoveTo_DestWrongLength(t *testing.T) {
	kb := loadTestKB(t)
	cases := [][]any{
		{1.0, 2.0},           // 太少
		{1.0, 2.0, 3.0, 4.0}, // 太多
	}
	for i, arr := range cases {
		_, err := resolveDebugMoveToLocation(map[string]any{"dest": arr}, kb)
		if err == nil {
			t.Errorf("[%d] expected error for wrong-length dest, got nil", i)
		}
	}
}

func TestResolveDebugMoveTo_DestNonNumeric(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"dest": []any{"foo", 2.0, 3.0}}
	_, err := resolveDebugMoveToLocation(params, kb)
	if err == nil {
		t.Fatal("expected error for non-numeric dest element")
	}
}

func TestResolveDebugMoveTo_DestNotArray(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"dest": "not an array"}
	_, err := resolveDebugMoveToLocation(params, kb)
	if err == nil {
		t.Fatal("expected error when dest is not an array")
	}
}

func TestResolveDebugMoveTo_NoDestNoTarget(t *testing.T) {
	kb := loadTestKB(t)
	// 既没 dest 也没 target，应报错提示两种模式
	_, err := resolveDebugMoveToLocation(map[string]any{}, kb)
	if err == nil {
		t.Fatal("expected error when neither dest nor target is provided")
	}
	if !strings.Contains(err.Error(), "dest") || !strings.Contains(err.Error(), "target") {
		t.Errorf("error should mention both dest and target options, got: %v", err)
	}
}

func TestResolveDebugMoveTo_DestNilKB(t *testing.T) {
	// dest 模式不应依赖 kb，nil kb 也能正常工作
	out, err := resolveDebugMoveToLocation(map[string]any{"dest": []any{1.0, 2.0, 3.0}}, nil)
	if err != nil {
		t.Fatalf("dest mode should work without kb: %v", err)
	}
	dest, _ := out["dest"].([]float64)
	if len(dest) != 3 || dest[0] != 1 || dest[1] != 2 || dest[2] != 3 {
		t.Errorf("dest=%v, want [1 2 3]", dest)
	}
}

func TestParseDestCoords_FloatSlice(t *testing.T) {
	out, err := parseDestCoords([]float64{1.5, 2.5, 3.5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 3 || out[0] != 1.5 || out[2] != 3.5 {
		t.Fatalf("got %v", out)
	}
}

func TestParseDestCoords_StringNumbers(t *testing.T) {
	// 字符串数字也应支持（容错）
	out, err := parseDestCoords([]any{"100", "200", "0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out[0] != 100 || out[1] != 200 || out[2] != 0 {
		t.Fatalf("got %v", out)
	}
}

func TestToFloat64_Types(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		err  bool
	}{
		{float64(1.5), 1.5, false},
		{int(10), 10, false},
		{int64(20), 20, false},
		{float32(0.5), 0.5, false},
		{json.Number("3.14"), 3.14, false},
		{"42", 42, false},
		{"not a number", 0, true},
		{nil, 0, true},
		{[]int{1}, 0, true},
	}
	for i, c := range cases {
		got, err := toFloat64(c.in)
		if c.err {
			if err == nil {
				t.Errorf("[%d] expected error for %v", i, c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("[%d] unexpected error: %v", i, err)
		}
		if got != c.want {
			t.Errorf("[%d] got %v, want %v", i, got, c.want)
		}
	}
}

func TestBuildDebugParams_CompositePassthrough(t *testing.T) {
	kb := loadTestKB(t)
	// charge_at_station 应直接透传 params，不再注入 name 字段
	out, err := buildDebugParams("charge_at_station", map[string]any{
		"target_object_id": "charging_station_01",
		"duration_sec":     1800,
	}, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := out["name"]; ok {
		t.Errorf("name should not be injected for composite cmds, got: %v", out["name"])
	}
	if out["target_object_id"] != "charging_station_01" {
		t.Errorf("target_object_id=%v, want charging_station_01", out["target_object_id"])
	}
	if out["duration_sec"] != 1800 {
		t.Errorf("duration_sec=%v, want 1800", out["duration_sec"])
	}
}

func TestBuildDebugParams_PassthroughForSimple(t *testing.T) {
	kb := loadTestKB(t)
	// speak 应直接透传，不加 name
	in := map[string]any{"content": "hello", "target": ""}
	out, err := buildDebugParams("speak", in, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["content"] != "hello" {
		t.Errorf("content=%v, want hello", out["content"])
	}
	if _, hasName := out["name"]; hasName {
		t.Error("speak should not have name field")
	}
}

// ─── debugOverride / force 逻辑 ────────────────────────────────

// TestDebugActionRequest_ForceDefault 验证 Force 字段 JSON 解码：
// 缺省 → nil（handleDebugAction 视为 true）；显式 false → false；显式 true → true。
func TestDebugActionRequest_ForceDefault(t *testing.T) {
	cases := []struct {
		json string
		want *bool
	}{
		{`{"agent_id":"H-01","cmd":"wait","params":{}}`, nil},
		{`{"agent_id":"H-01","cmd":"wait","params":{},"force":false}`, boolPtr(false)},
		{`{"agent_id":"H-01","cmd":"wait","params":{},"force":true}`, boolPtr(true)},
	}
	for i, c := range cases {
		var req debugActionRequest
		if err := json.Unmarshal([]byte(c.json), &req); err != nil {
			t.Fatalf("[%d] unmarshal error: %v", i, err)
		}
		if (req.Force == nil) != (c.want == nil) {
			t.Errorf("[%d] Force=nil mismatch: got %v, want %v", i, req.Force, c.want)
			continue
		}
		if req.Force != nil && *req.Force != *c.want {
			t.Errorf("[%d] Force value mismatch: got %v, want %v", i, *req.Force, *c.want)
		}
	}
}

// TestAgentContext_DebugOverrideLifecycle 验证 debugOverride 字段的
// set/clear/signal 生命周期，确保 handleDebugAction 的 defer 模式正确。
func TestAgentContext_DebugOverrideLifecycle(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	// 初始 false
	ac.mu.Lock()
	if ac.debugOverride {
		t.Error("debugOverride should start false")
	}
	ac.mu.Unlock()

	// set true
	ac.mu.Lock()
	ac.debugOverride = true
	ac.mu.Unlock()

	ac.mu.Lock()
	if !ac.debugOverride {
		t.Error("debugOverride should be true after set")
	}
	ac.mu.Unlock()

	// defer 模式：set true → ... → clear + signal
	func() {
		ac.mu.Lock()
		ac.debugOverride = true
		ac.mu.Unlock()
		defer func() {
			ac.mu.Lock()
			ac.debugOverride = false
			ac.mu.Unlock()
			ac.signal()
		}()
	}()

	ac.mu.Lock()
	if ac.debugOverride {
		t.Error("debugOverride should be false after defer clear")
	}
	ac.mu.Unlock()

	// signal 应该投递到 wake
	select {
	case <-ac.wake:
	default:
		t.Error("signal should have delivered to wake channel")
	}
}

func boolPtr(b bool) *bool { return &b }

// ─── tacticalRefillForReplan 测试 ──────────────────────────────

func TestTacticalRefillForReplan_NoTacticalHc(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// tacticalHc 默认 nil
	ok := ac.tacticalRefillForReplan(context.Background(), "H-01", nil, nil, slog.Default(), "test hint")
	if ok {
		t.Error("should return false when tacticalHc is nil")
	}
}

func TestTacticalRefillForReplan_NoGoal(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// 设置 tacticalHc 但不设 dailyPlan → selectCurrentGoal 返回 ""
	ac.mu.Lock()
	ac.tacticalHc = newFailedHermesClient()
	ac.dailyPlan = ""
	ac.mu.Unlock()
	ok := ac.tacticalRefillForReplan(context.Background(), "H-01", nil, nil, slog.Default(), "test hint")
	if ok {
		t.Error("should return false when no current goal")
	}
}

func TestTacticalRefillForReplan_LLMFail(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// 设置 tacticalHc 指向无效端口 → LLM 调用必然失败
	// 保留旧队列：失败时不应清空
	oldQueue := []plannedAction{{Action: "wait", Params: map[string]any{"duration_sec": 30}}}
	setQueueForTest(ac, oldQueue)
	// 构造一个含 time_of_day 的 perception，使 selectCurrentGoal 能匹配到 slot
	percJSON, _ := json.Marshal(protocol.PerceptionPayload{
		Environment: protocol.Environment{TimeOfDay: "09:00"},
	})
	ac.mu.Lock()
	ac.tacticalHc = newFailedHermesClient()
	ac.dailyPlan = "06:00-12:00: 上午装配\n12:00-13:00: 午休"
	ac.latestPerception = percJSON
	ac.mu.Unlock()
	ok := ac.tacticalRefillForReplan(context.Background(), "H-01", nil, nil, slog.Default(), "test hint")
	if ok {
		t.Error("should return false when LLM call fails")
	}
	// 验证旧队列保留
	ac.mu.Lock()
	queueLen := len(ac.actionQueue)
	ac.mu.Unlock()
	if queueLen != 1 {
		t.Errorf("old queue should be preserved on failure, got len=%d", queueLen)
	}
}

// newFailedHermesClient 构造一个指向无效端口的 hermes.Client，
// 任何 LLM 调用都会因连接失败而返回 error。
func newFailedHermesClient() *hermes.Client {
	return hermes.New(hermes.Config{URL: "http://127.0.0.1:1"})
}

// ─── /debug/schedule 端点测试 ──────────────────────────────────
//
// 这些测试覆盖 handleDebugSchedule 的早期校验路径（method/JSON/字段/schedule
// 格式），这些校验在 ws.IsConnected() 检查之前触发，因此不需要真实 UE 连接。
// 需要 ws + LLM 的路径（happy path / 409 / 502 / force stop）依赖 ws 测试
// 夹具，与现有 handleDebugAction 一致地留作集成测试。

// newDebugScheduleRecorder 构造一个 POST /debug/schedule 的 httptest 请求 +
// recorder，body 为给定 JSON 字符串。复用 wsserver.New 创建的未连接 server
// （IsConnected 返回 false，但早期校验在此之前返回）。
func newDebugScheduleRecorder(t *testing.T, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/debug/schedule", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	return req, rec
}

func TestHandleDebugSchedule_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/schedule", nil)
	rec := httptest.NewRecorder()
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
	var resp debugScheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == "" {
		t.Error("error message should be non-empty")
	}
}

func TestHandleDebugSchedule_InvalidJSON(t *testing.T) {
	req, rec := newDebugScheduleRecorder(t, "{not json")
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var resp debugScheduleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(resp.Error, "invalid JSON") {
		t.Errorf("error=%q, want contain 'invalid JSON'", resp.Error)
	}
}

func TestHandleDebugSchedule_MissingAgentID(t *testing.T) {
	req, rec := newDebugScheduleRecorder(t, `{"schedule":"07:00-11:00: 装配"}`)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var resp debugScheduleResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "agent_id") {
		t.Errorf("error=%q, want contain 'agent_id'", resp.Error)
	}
}

func TestHandleDebugSchedule_MissingSchedule(t *testing.T) {
	req, rec := newDebugScheduleRecorder(t, `{"agent_id":"H-01"}`)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var resp debugScheduleResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "schedule") {
		t.Errorf("error=%q, want contain 'schedule'", resp.Error)
	}
}

// TestHandleDebugSchedule_MultiLineRejected 验证多行 schedule 被拒。
// 多行语义不明（分解哪行？），强制单行。
func TestHandleDebugSchedule_MultiLineRejected(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"07:00-11:00: 装配\n13:00-17:00: 巡检"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	var resp debugScheduleResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "single line") {
		t.Errorf("error=%q, want contain 'single line'", resp.Error)
	}
}

// TestHandleDebugSchedule_PureGoalAccepted 验证纯 goal 形态（无时间段）被接受。
// "车间装配作业" 不含时间段，parseScheduleText 返回 ("", "车间装配作业")，
// 校验通过到达 ws 检查返回 503（而非 400）。
func TestHandleDebugSchedule_PureGoalAccepted(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"车间装配作业"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	// 纯 goal 合法，应到达 ws 检查返回 503（而非 400）
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (pure goal valid, ws not connected)", rec.Code)
	}
}

// TestHandleDebugSchedule_BadSlotFallsBackToPureGoal 验证 slot 格式非法时
// 降级为纯 goal。"07-11: 装配作业" 缺少分钟，parseFormattedPlan 解析失败
// 返回 0 条 → parseScheduleText 当作纯 goal 返回 ("", "07-11: 装配作业")，
// 校验通过到达 ws 检查返回 503（而非 400）。
func TestHandleDebugSchedule_BadSlotFallsBackToPureGoal(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"07-11: 装配作业"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	// 格式非法的 slot → 降级为纯 goal → 合法 → 503
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (bad slot → fallback to pure goal)", rec.Code)
	}
}

// TestHandleDebugSchedule_UENotConnected 验证 schedule 校验通过后，
// UE 未连接时返回 503（确认校验顺序：parse → ws check）。
func TestHandleDebugSchedule_UENotConnected(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"07:00-11:00: 车间装配作业"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{}) // 未连接
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	var resp debugScheduleResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "no mock ue") {
		t.Errorf("error=%q, want contain 'no mock ue'", resp.Error)
	}
}

// TestHandleDebugSchedule_AgentNotFound 验证 lookupAgent 返回 nil 时 404。
// 需要 ws.IsConnected 返回 true 才能到达此分支，故此用例验证 lookupAgent==nil
// 路径（nil lookupAgent 函数）—— 但 ws 检查在前，所以此用例实际验证的是
// ws 未连接时返回 503 而非 404（agent 检查在 ws 之后）。
// 保留此用例确认顺序：ws check → lookupAgent check。
func TestHandleDebugSchedule_OrderWSBeforeLookup(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"07:00-11:00: 车间装配作业"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{}) // 未连接
	// lookupAgent 返回 nil，但 ws 检查在前，应返回 503 而非 404
	lookup := func(string) *agentContext { return nil }
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, lookup, nil, rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (ws check before lookup)", rec.Code)
	}
}

// TestDebugScheduleRequest_ForceDefault 验证 Force 字段 JSON 解码：
// 缺省 → nil（handler 视为 true）；显式 false → false；显式 true → true。
func TestDebugScheduleRequest_ForceDefault(t *testing.T) {
	cases := []struct {
		json string
		want *bool
	}{
		{`{"agent_id":"H-01","schedule":"07:00-11:00: 装配"}`, nil},
		{`{"agent_id":"H-01","schedule":"07:00-11:00: 装配","force":false}`, boolPtr(false)},
		{`{"agent_id":"H-01","schedule":"07:00-11:00: 装配","force":true}`, boolPtr(true)},
	}
	for i, c := range cases {
		var req debugScheduleRequest
		if err := json.Unmarshal([]byte(c.json), &req); err != nil {
			t.Fatalf("[%d] unmarshal error: %v", i, err)
		}
		if (req.Force == nil) != (c.want == nil) {
			t.Errorf("[%d] Force=nil mismatch: got %v, want %v", i, req.Force, c.want)
			continue
		}
		if req.Force != nil && *req.Force != *c.want {
			t.Errorf("[%d] Force value mismatch: got %v, want %v", i, *req.Force, *c.want)
		}
	}
}

// TestHandleDebugSchedule_SingleLineParseValid 验证合法单行 schedule 能通过
// parseFormattedPlan + splitPlanRange 校验（即不会在 400 阶段被拒），
// 到达 ws.IsConnected() 检查返回 503。覆盖 "07:00-11:00: 车间装配作业" 示例。
func TestHandleDebugSchedule_SingleLineParseValid(t *testing.T) {
	body := `{"agent_id":"H-01","schedule":"07:00-11:00: 车间装配作业"}`
	req, rec := newDebugScheduleRecorder(t, body)
	ws := wsserver.New(wsserver.Options{})
	handleDebugSchedule(context.Background(), slog.Default(), ws, nil, nil, nil, rec, req)

	// 应通过 schedule 校验，到达 ws 检查返回 503（而非 400）
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (schedule valid, ws not connected)", rec.Code)
	}
}

// TestParseScheduleText 验证 parseScheduleText 的两种形态解析。
func TestParseScheduleText(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantSlot string
		wantGoal string
	}{
		{
			name:     "带时间段",
			input:    "07:00-11:00: 车间装配作业",
			wantSlot: "07:00-11:00",
			wantGoal: "车间装配作业",
		},
		{
			name:     "纯 goal 无时间段",
			input:    "车间装配作业",
			wantSlot: "",
			wantGoal: "车间装配作业",
		},
		{
			name:     "纯 goal 带冒号但非时段格式",
			input:    "去车间: 装配作业",
			wantSlot: "",
			wantGoal: "去车间: 装配作业",
		},
		{
			name:     "slot 格式非法降级为纯 goal",
			input:    "07-11: 装配作业",
			wantSlot: "",
			wantGoal: "07-11: 装配作业",
		},
		{
			name:     "带前后空白的纯 goal 被 trim",
			input:    "  车间装配作业  ",
			wantSlot: "",
			wantGoal: "车间装配作业",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slot, goal := parseScheduleText(c.input)
			if slot != c.wantSlot {
				t.Errorf("slot: got %q, want %q", slot, c.wantSlot)
			}
			if goal != c.wantGoal {
				t.Errorf("goal: got %q, want %q", goal, c.wantGoal)
			}
		})
	}
}

// ─── world_kb handler (worldKBSwap) ─────────────────────────────

// buildWorldKBPayload 构造一个合法的 world_kb payload（最小 generated+authored）。
func buildWorldKBPayload(t *testing.T) []byte {
	t.Helper()
	gen := map[string]any{
		"$schema": "agenttown-world-generated/v1", "schema_version": "1.0",
		"zones": []map[string]any{
			{"id": "zone1", "bounds": map[string]any{"center": []int{0, 0, 0}, "extent": []int{1, 1, 1}},
				"entry_point": []int{0, 0, 0}, "entry_facing": []int{1, 0, 0}},
		},
		"objects": []map[string]any{}, "agents": []map[string]any{},
	}
	auth := map[string]any{
		"version": "1.0", "narrative": map[string]any{"setting": "测试", "theme": "t"},
		"zones":   map[string]any{"zone1": map[string]any{"display_name": "Z1"}},
		"objects": map[string]any{}, "agents": map[string]any{},
	}
	p := protocol.WorldKBPayload{
		PushedAt:  "2026-07-31T03:00:00Z",
		Generated: mustJSON(t, gen),
		Authored:  mustJSON(t, auth),
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestWorldKBSwap_AcceptedBeforeAgentRegistered 验证启动窗口内接受 world_kb：
// 返回非 nil KB + 无错误 + YAML 落盘可重载。
func TestWorldKBSwap_AcceptedBeforeAgentRegistered(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"
	manifestPath := dir + "/world_kb.manifest.json"
	payload := buildWorldKBPayload(t)

	newKB, err := worldKBSwap(false, payload, outPath, manifestPath)
	if err != nil {
		t.Fatalf("worldKBSwap: %v", err)
	}
	if newKB == nil {
		t.Fatal("newKB should not be nil on success")
	}
	if len(newKB.Zones) != 1 || newKB.Zones[0].ID != "zone1" {
		t.Errorf("zone mismatch: %+v", newKB.Zones)
	}
	if newKB.GetZone("zone1") == nil {
		t.Error("index not built — GetZone returned nil")
	}
	// YAML 落盘可重载。
	reloaded, err := worldkb.Load(outPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Narrative.Setting != "测试" {
		t.Errorf("narrative.setting = %q, want 测试", reloaded.Narrative.Setting)
	}
	// Manifest 落盘。
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest not written: %v", err)
	}
}

// TestWorldKBSwap_RejectedAfterAgentRegistered 验证首个 agent_registered
// 之后到达的 world_kb 被拒绝：返回 errAgentWindowClosed + 不写盘。
func TestWorldKBSwap_RejectedAfterAgentRegistered(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"
	payload := buildWorldKBPayload(t)

	_, err := worldKBSwap(true, payload, outPath, "")
	if !errors.Is(err, errAgentWindowClosed) {
		t.Fatalf("expected errAgentWindowClosed, got: %v", err)
	}
	// 不应写盘。
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Error("out file should NOT exist after rejection")
	}
}

// TestWorldKBSwap_BadPayloadPreservesOldKB 验证 payload 损坏时返回错误
// 且不写盘（调用方据此保留旧 KB）。
func TestWorldKBSwap_BadPayloadPreservesOldKB(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	_, err := worldKBSwap(false, json.RawMessage("{not json"), outPath, "")
	if err == nil {
		t.Fatal("expected parse error for malformed payload")
	}
	if errors.Is(err, errAgentWindowClosed) {
		t.Fatal("parse error should not be masked as window-closed")
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Error("out file should NOT exist after parse failure")
	}
}

// TestWorldKBSwap_MergeErrorPreservesOldKB 验证 merge 失败（schema 不匹配）
// 时返回错误且不写盘。
func TestWorldKBSwap_MergeErrorPreservesOldKB(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	// schema_version=9.9 vs authored version=1.0 → merge error.
	gen := map[string]any{
		"schema_version": "9.9",
		"zones":          []map[string]any{}, "objects": []map[string]any{}, "agents": []map[string]any{},
	}
	auth := map[string]any{
		"version": "1.0", "narrative": map[string]any{"setting": "x"},
		"zones": map[string]any{}, "objects": map[string]any{}, "agents": map[string]any{},
	}
	p := protocol.WorldKBPayload{
		Generated: mustJSON(t, gen),
		Authored:  mustJSON(t, auth),
	}
	payload, _ := json.Marshal(p)

	_, err := worldKBSwap(false, payload, outPath, "")
	if err == nil {
		t.Fatal("expected merge error for schema mismatch")
	}
	if errors.Is(err, errAgentWindowClosed) {
		t.Fatal("merge error should not be masked as window-closed")
	}
	if _, statErr := os.Stat(outPath); statErr == nil {
		t.Error("out file should NOT exist after merge failure")
	}
}
