package main

import (
	"context"
	"log/slog"
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

	queued := ac.recordActionCompletion(protocol.ActionCompletedPayload{
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

func TestRecordEventNotification_NoOpPreservesQueue(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	setQueueForTest(ac, []plannedAction{
		{Action: "move_to", Params: map[string]any{"target": "main_workshop"}},
		{Action: "wait", Params: map[string]any{"duration_sec": 30}},
	})
	ac.mu.Lock()
	ac.currentSlot = "08:00-12:00"
	ac.redecomposeCount = 1
	ac.mu.Unlock()

	// 反应层移除后：事件通知不再打断战术队列，返回 false（未触发决策）
	queued := ac.recordEventNotification(protocol.EventNotificationPayload{
		EventID:         "evt_001",
		PerceptionLevel: "audible",
		Event:           map[string]any{"type": "alert"},
	})
	if queued {
		t.Fatal("event notification should not queue a decision after reactive layer removal")
	}
	// 队列应原样保留
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
