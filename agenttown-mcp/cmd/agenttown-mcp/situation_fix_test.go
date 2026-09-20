package main

// P3-9 后续修复 A+B+C 测试：威胁情境状态（A）、事件回声（B）、
// 反应窗口内的 refill 不发"长动作收尾"自动 hint（C）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
)

// TestCombatSituation_RegisterAndResolve verifies fix A end to end at the
// dispatch entry: attacked/targeted registers the ongoing threat, a repeat
// keeps the earliest start, combat_exit resolves it.
func TestCombatSituation_RegisterAndResolve(t *testing.T) {
	rt, _, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac)

	// 被攻击（force）→ 登记。
	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_s1"))
	registered := ac.as.ActiveSituations(ac.as.LatestGameTimeSec(), situationTTLGameSec)
	if len(registered) != 1 || registered[0].Kind != situationKindCombat {
		t.Fatalf("attacked must register the combat situation, got %+v", registered)
	}
	start := registered[0].StartGameSec

	// 再被瞄准（同 kind）→ 保留最早起点。
	targeted := forceTestEvent("evt_s2")
	targeted.EventType = protocol.EventTypePlayerTargeted
	dispatchTestEvent(t, rt, "H-01", targeted)
	if got := ac.as.ActiveSituations(ac.as.LatestGameTimeSec(), situationTTLGameSec); len(got) != 1 || got[0].StartGameSec != start {
		t.Fatalf("repeat combat start must keep the earliest start, got %+v", got)
	}

	// combat_exit（非 force）→ 解除。
	exit := nonForceTestEvent("evt_s3")
	exit.Category = protocol.CategoryPlayerInteraction
	exit.EventType = protocol.EventTypeCombatExit
	dispatchTestEvent(t, rt, "H-01", exit)
	if got := ac.as.ActiveSituations(ac.as.LatestGameTimeSec(), situationTTLGameSec); len(got) != 0 {
		t.Fatalf("combat_exit must resolve the situation, got %+v", got)
	}
}

// TestCombatSituation_TTLExpiry verifies the fallback: a situation with no
// resolution event auto-degrades after the TTL.
func TestCombatSituation_TTLExpiry(t *testing.T) {
	_, _, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac)
	now := ac.as.LatestGameTimeSec()
	ac.as.BeginSituation(situationKindCombat, "被玩家攻击", now)
	if got := agentstate.FilterSituations([]agentstate.ActiveSituation{{Kind: situationKindCombat, StartGameSec: now}}, now+situationTTLGameSec+1, situationTTLGameSec); len(got) != 0 {
		t.Fatalf("situation past TTL must be filtered, got %+v", got)
	}
	if got := agentstate.FilterSituations([]agentstate.ActiveSituation{{Kind: situationKindCombat, StartGameSec: now}}, now+situationTTLGameSec-1, situationTTLGameSec); len(got) != 1 {
		t.Fatalf("situation within TTL must stay, got %+v", got)
	}
}

// TestSituation_InjectedIntoTacticalPrompt is the core regression of the
// user's report: the FIRST refill after a short reaction MUST still see the
// threat — via 【当前处境】（A）和【发生的事件】回声（B）。"威胁解除了"
// 幻觉的直接防线。
func TestSituation_InjectedIntoTacticalPrompt(t *testing.T) {
	ac := tacticalCtxForTest(nil)
	// 感知锚定游戏时间（D12 10:47:03 = 989223 游戏秒）。
	if _, err := ac.as.SetPerception(mustMarshalBarPerception(t, 11, 38823, 989223)); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	// 威胁情境已登记（被攻击，10 分钟前开始 → 已持续可见）。
	ac.as.BeginSituation(situationKindCombat, "玩家互动：被玩家 player_1 攻击（伤害 20，类型 physical）", 989223-600)

	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac.tacticalHc = fake
	if _, err := generateTacticalPlan(t.Context(), ac, "H-01", "车间装配", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("generateTacticalPlan: %v", err)
	}
	user := lastUserPromptOf(t, fake.capturedMsgs)
	for _, want := range []string{
		"【当前处境】以下情境仍在持续、尚未收到解除信号",
		"被玩家 player_1 攻击",
		"已持续约", // A 的持续时间口径
	} {
		if !strings.Contains(user, want) {
			t.Fatalf("tactical prompt missing %q:\n%s", want, user)
		}
	}
}

// TestEcho_ConsumedByPostReactionRefill verifies fix B's full flow: the
// force event echoed at replan success is drained into the FIRST refill
// after the reaction — the last-trace-of-context hole is closed.
func TestEcho_ConsumedByPostReactionRefill(t *testing.T) {
	ac := tacticalCtxForTest(nil)
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac.tacticalHc = fake

	// 反应 replan 成功后回声入队（force 路径的镜像——直接调 echo 验证
	// 消费语义）。
	ac.as.EchoWorldEvent(forceTestEvent("evt_echo_1"))
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("echo should be queued, got len=%d", got)
	}
	// 下一次分解（反应耗尽后的 refill）看到它，成功后消费。
	if _, err := generateTacticalPlan(t.Context(), ac, "H-01", "车间装配", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("generateTacticalPlan: %v", err)
	}
	if !strings.Contains(lastUserPromptOf(t, fake.capturedMsgs), "被玩家 player_1 攻击") {
		t.Fatalf("post-reaction refill must see the echoed event")
	}
	if got := ac.as.WorldEventQueueLen(); got != 0 {
		t.Fatalf("echo must be consumed by the successful refill, got len=%d", got)
	}
}

// TestReactionRefill_SuppressesAutoHint verifies fix C: with the reaction
// window still armed, the refill skips the queue-exhaustion auto-hint
// ("上次队列提前耗尽…安排长动作收尾")——反应刚结束时它是反向信号。
func TestReactionRefill_SuppressesAutoHint(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.tacticalHc = newFailedVenusClient()

	// 反应窗口 armed（模拟"紧急反应刚结束"），同 slot 队列耗尽 refill。
	ac.beginReaction(8, ac.as.LatestGameTimeSec(), "")
	// 第一次成功分解置位 tacticalHeaderPlan，让 refill 走"同 slot 重分解"
	// 路径（auto-hint 只在该路径生成）。
	ac.tacticalHc = &fakeLoopLLM{resp: speakToolCallResp()}
	if _, err := generateTacticalPlan(t.Context(), ac, "H-01", "车间装配作业", "main_workshop",
		"10:47", "09:00-12:00", "09:00-12:00: 车间装配作业", nil, nil, nil, testLogger(), "", "", "", nil, nil, nil, nil); err != nil {
		t.Fatalf("first plan: %v", err)
	}
	// 反应 replan 置 currentSlot（CommitReplan）。
	ac.as.CommitReplan(nil, "09:00-12:00", 0)

	// armed 状态下的 refill：无 auto-hint（传 fake transport：成功路径会
	// popAndSendQueueAction 下发首段）。
	fake := ac.tacticalHc.(*fakeLoopLLM)
	if ok := ac.tacticalRefill(t.Context(), "H-01", ft, nil, nil, testLogger()); !ok {
		t.Fatalf("refill should succeed")
	}
	if u := lastUserPromptOf(t, fake.capturedMsgs); strings.Contains(u, "上次队列提前耗尽") || strings.Contains(u, "只返回了 1 个 speak") {
		t.Fatalf("reaction-armed refill must suppress the queue-exhaustion auto-hint:\n%s", u)
	}

	// 对照：反应窗口解除后，auto-hint 恢复（既有行为不变）。
	ac.clearReaction()
	ac.as.ReplaceQueue(nil)
	ac.as.SetReplanHint("")
	if ok := ac.tacticalRefill(t.Context(), "H-01", ft, nil, nil, testLogger()); !ok {
		t.Fatalf("second refill should succeed")
	}
	if u := lastUserPromptOf(t, fake.capturedMsgs); !strings.Contains(u, "上次队列提前耗尽") {
		t.Fatalf("non-reaction refill must keep the auto-hint:\n%s", u)
	}
}

// TestSituation_InRouterPrompt verifies the router sees ongoing situations
// (an existing threat colors how urgent a NEW event is).
func TestSituation_InRouterPrompt(t *testing.T) {
	rt, ft, ac, llm, _ := newRouterTestRuntime(t, `{"interrupt": false, "severity": 2, "reason": "x"}`)
	seedPerception(t, ac)
	ac.as.BeginSituation(situationKindCombat, "被玩家 player_1 瞄准/锁定", ac.as.LatestGameTimeSec())

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_s4"))
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(llm.lastPrompt(), "世界事件") })
	if p := llm.lastPrompt(); !strings.Contains(p, "【当前处境】仍在持续、尚未解除") || !strings.Contains(p, "被玩家 player_1 瞄准") {
		t.Fatalf("router prompt must carry active situations:\n%s", p)
	}
	_ = ft
}

// TestSituation_StatusBarLine verifies the <agent_state> bar carries the
// ongoing situation every LLM call.
func TestSituation_StatusBarLine(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	seedPerception(t, ac)
	ac.as.BeginSituation(situationKindCombat, "被玩家 player_1 攻击", ac.as.LatestGameTimeSec())
	bar := ac.buildAgentStateBar("H-01", nil)
	if !strings.Contains(bar, "当前处境：") || !strings.Contains(bar, "尚未收到解除信号") {
		t.Fatalf("status bar must carry the ongoing situation:\n%s", bar)
	}
}
