package main

// P3-7 安全点 drain 测试：事件队列 → 战术层【发生的事件】段。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
)

// enqueueTwoEvents 入队两条典型非 force 事件，返回渲染要点断言串。
func enqueueTwoEvents(t *testing.T, ac *agentContext) (first, second string) {
	t.Helper()
	ev1 := nonForceTestEvent("evt_drain_1") // K-03 故障
	if res := ac.as.EnqueueWorldEvent(ev1); res != agentstate.EnqueueOK {
		t.Fatalf("enqueue 1: %v", res)
	}
	ev2 := nonForceTestEvent("evt_drain_2")
	ev2.Category = "physical_threshold"
	ev2.EventType = "energy_below"
	ev2.Data = []byte(`{"attribute":"energy","value":19.8,"threshold":20,"direction":"below"}`)
	if res := ac.as.EnqueueWorldEvent(ev2); res != agentstate.EnqueueOK {
		t.Fatalf("enqueue 2: %v", res)
	}
	return "K-03 关节锁死", "能量向下跌破阈值 20"
}

// TestGenerateTacticalPlan_EventsDrainedOnSuccess verifies the full
// consumption contract: the drained events appear in the tactical prompt
// (【发生的事件】), and a SUCCESSFUL decomposition consumes the queue.
func TestGenerateTacticalPlan_EventsDrainedOnSuccess(t *testing.T) {
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac := tacticalCtxForTest(fake)
	first, second := enqueueTwoEvents(t, ac)

	_, err := generateTacticalPlan(context.Background(), ac, "H-01", "车间装配", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("generateTacticalPlan: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	for _, want := range []string{"【发生的事件】", first, second} {
		if !strings.Contains(user, want) {
			t.Fatalf("tactical prompt missing %q:\n%s", want, user)
		}
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("successful decomposition must consume the event queue, got len=%d", got)
	}
}

// TestGenerateTacticalPlan_FailureKeepsEvents verifies a failed LLM call
// leaves the queue intact — the events re-enter the next decomposition
// (events are decision inputs, not ledger entries to burn on one failure).
func TestGenerateTacticalPlan_FailureKeepsEvents(t *testing.T) {
	fake := &fakeLoopLLM{err: errors.New("network down")}
	ac := tacticalCtxForTest(fake)
	enqueueTwoEvents(t, ac)

	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "车间装配", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil); err == nil {
		t.Fatal("expected error")
	}
	if got := ac.as.WorldEventQueueLen(); got != 2 {
		t.Fatalf("failed decomposition must keep the event queue, got len=%d", got)
	}

	// 重试成功后消费（同一批事件重新进入 prompt）。
	fake.err = nil
	fake.resp = speakToolCallResp()
	if _, err := generateTacticalPlan(context.Background(), ac, "H-01", "车间装配", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	if !strings.Contains(user, "【发生的事件】") {
		t.Fatalf("retry prompt must re-inject the kept events:\n%s", user)
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("successful retry must consume the queue, got len=%d", got)
	}
}

// TestForceReplan_PromptCarriesQueuedEvents verifies the interrupt replan
// also sees the queued events: force event in the emergency hint + the
// other queued events in 【发生的事件】 — one LLM call sees the whole world.
func TestForceReplan_PromptCarriesQueuedEvents(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, "")
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac.tacticalHc = fake
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.as.RecordActionStarted("act-1", "WorkShift", nil, agentstate.SourceTactical, "")
	// force 到达前队列里攒了两条非 force 事件。
	enqueueTwoEvents(t, ac)

	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_drain_f"))

	// force replan 成功（fake 返回 tool_calls）→ prompt 同时含紧急 hint
	// 与队列事件，且队列被消费。轮询 mutex 保护的队列长度（ClearWorldEvents
	// 在 prompt 写入之后才执行——经 AgentState.mu 形成 happens-before，
	// 此后读 capturedMsgs 是安全的；不直接轮询无锁的 capturedMsgs）。
	waitFor(t, 2*time.Second, func() bool {
		return ac.as.WorldEventQueueLen() == 0 && replanIdle(ac)
	})
	user := lastUserPromptOf(t, fake.capturedMsgs)
	for _, want := range []string{
		"【紧急事件】玩家互动：被玩家 player_1 攻击", // force 事件本身（hint 渲染）
		"【发生的事件】", // 队列攒下的事件
		"K-03 关节锁死",
		"能量向下跌破阈值 20",
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("force replan prompt missing %q:\n%s", want, user)
		}
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("force replan must consume the queued events, got len=%d", got)
	}
	// stop 已同步发出（force 硬保证不因 drain 改变）。
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("force stop must still fire, got %v", stops)
	}
}
