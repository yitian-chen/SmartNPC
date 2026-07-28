package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
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
		{"move_to", protocol.CmdMoveTo, true},
		{"speak", protocol.CmdSpeak, true},
		{"interact", protocol.CmdInteractSmartObject, true},
		{"wait", protocol.CmdWait, true},
		{"charge_at", protocol.CmdExecuteComposite, true},
		{"work_assemble", protocol.CmdExecuteComposite, true},
		{"archive_research", protocol.CmdExecuteComposite, true},
		{"rest_idle", protocol.CmdExecuteComposite, true},
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

func TestResolveDebugMoveTo_Valid(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"target": "workbench_01"}
	out, err := resolveDebugMoveTo(params, kb)
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
	if out["target"] != "workbench_01" {
		t.Errorf("target=%v, want workbench_01", out["target"])
	}
	if out["kind"] == "" {
		t.Error("kind should not be empty for valid target")
	}
	if out["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", out["speed"])
	}
}

func TestResolveDebugMoveTo_Zone(t *testing.T) {
	kb := loadTestKB(t)
	// main_workshop 是 zone，应返回 kind="zone"
	out, err := resolveDebugMoveTo(map[string]any{"target": "main_workshop"}, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["kind"] != "zone" {
		t.Errorf("kind=%v, want zone", out["kind"])
	}
}

func TestResolveDebugMoveTo_UnknownTarget(t *testing.T) {
	kb := loadTestKB(t)
	_, err := resolveDebugMoveTo(map[string]any{"target": "nonexistent_place"}, kb)
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestResolveDebugMoveTo_EmptyTarget(t *testing.T) {
	kb := loadTestKB(t)
	_, err := resolveDebugMoveTo(map[string]any{"target": ""}, kb)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestResolveDebugMoveTo_NilKB(t *testing.T) {
	_, err := resolveDebugMoveTo(map[string]any{"target": "workbench_01"}, nil)
	if err == nil {
		t.Fatal("expected error when kb is nil")
	}
}

// ─── move_to 坐标直传（v2 新功能） ──────────────────────────

func TestResolveDebugMoveTo_DestCoords(t *testing.T) {
	kb := loadTestKB(t)
	// 直接传 dest 坐标，不走 kb 解析
	params := map[string]any{"dest": []any{10000.0, 20000.0, 0.0}}
	out, err := resolveDebugMoveTo(params, kb)
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
	if out["kind"] != "coord" {
		t.Errorf("kind=%v, want coord", out["kind"])
	}
	if out["target"] != "" {
		t.Errorf("target should be empty for coord mode, got %v", out["target"])
	}
	if out["speed"] != "walk" {
		t.Errorf("speed=%v, want walk", out["speed"])
	}
}

func TestResolveDebugMoveTo_DestWithIntCoords(t *testing.T) {
	// JSON 解码整数常会变 float64，但也支持 int / int64
	kb := loadTestKB(t)
	params := map[string]any{"dest": []any{10000, 20000, 0}}
	out, err := resolveDebugMoveTo(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dest, _ := out["dest"].([]float64)
	if len(dest) != 3 || dest[0] != 10000 || dest[1] != 20000 {
		t.Fatalf("dest=%v, want [10000 20000 0]", dest)
	}
}

func TestResolveDebugMoveTo_DestWithTargetLabel(t *testing.T) {
	// dest + target 同时传：dest 优先，target 仅作日志标签
	kb := loadTestKB(t)
	params := map[string]any{
		"dest":   []any{15000.0, 11000.0, 0.0},
		"target": "custom_spot",
	}
	out, err := resolveDebugMoveTo(params, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["kind"] != "coord" {
		t.Errorf("kind=%v, want coord (dest 优先)", out["kind"])
	}
	if out["target"] != "custom_spot" {
		t.Errorf("target=%v, want custom_spot (保留标签)", out["target"])
	}
}

func TestResolveDebugMoveTo_DestWrongLength(t *testing.T) {
	kb := loadTestKB(t)
	cases := [][]any{
		{1.0, 2.0},           // 太少
		{1.0, 2.0, 3.0, 4.0}, // 太多
	}
	for i, arr := range cases {
		_, err := resolveDebugMoveTo(map[string]any{"dest": arr}, kb)
		if err == nil {
			t.Errorf("[%d] expected error for wrong-length dest, got nil", i)
		}
	}
}

func TestResolveDebugMoveTo_DestNonNumeric(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"dest": []any{"foo", 2.0, 3.0}}
	_, err := resolveDebugMoveTo(params, kb)
	if err == nil {
		t.Fatal("expected error for non-numeric dest element")
	}
}

func TestResolveDebugMoveTo_DestNotArray(t *testing.T) {
	kb := loadTestKB(t)
	params := map[string]any{"dest": "not an array"}
	_, err := resolveDebugMoveTo(params, kb)
	if err == nil {
		t.Fatal("expected error when dest is not an array")
	}
}

func TestResolveDebugMoveTo_NoDestNoTarget(t *testing.T) {
	kb := loadTestKB(t)
	// 既没 dest 也没 target，应报错提示两种模式
	_, err := resolveDebugMoveTo(map[string]any{}, kb)
	if err == nil {
		t.Fatal("expected error when neither dest nor target is provided")
	}
	if !strings.Contains(err.Error(), "dest") || !strings.Contains(err.Error(), "target") {
		t.Errorf("error should mention both dest and target options, got: %v", err)
	}
}

func TestResolveDebugMoveTo_DestNilKB(t *testing.T) {
	// dest 模式不应依赖 kb，nil kb 也能正常工作
	out, err := resolveDebugMoveTo(map[string]any{"dest": []any{1.0, 2.0, 3.0}}, nil)
	if err != nil {
		t.Fatalf("dest mode should work without kb: %v", err)
	}
	if out["kind"] != "coord" {
		t.Errorf("kind=%v, want coord", out["kind"])
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

func TestBuildDebugParams_CompositeAddsName(t *testing.T) {
	kb := loadTestKB(t)
	// charge_at 应在 params 里加 name=charge_at
	out, err := buildDebugParams("charge_at", map[string]any{
		"station_id":   "charging_station_01",
		"duration_min": 30,
	}, kb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["name"] != "charge_at" {
		t.Errorf("name=%v, want charge_at", out["name"])
	}
	if out["station_id"] != "charging_station_01" {
		t.Errorf("station_id=%v, want charging_station_01", out["station_id"])
	}
	if out["duration_min"] != 30 {
		t.Errorf("duration_min=%v, want 30", out["duration_min"])
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
