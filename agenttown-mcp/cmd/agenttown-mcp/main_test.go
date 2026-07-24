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
