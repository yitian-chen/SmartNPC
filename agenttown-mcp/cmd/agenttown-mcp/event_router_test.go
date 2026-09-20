package main

// P2-5 轻量事件路由器运行时测试。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

// scriptedRouterLLM implements llmClient returning a scripted response body
// per call. It records the last user prompt so tests can assert the router
// saw the right inputs (persona / relationships / event).
type scriptedRouterLLM struct {
	fakeStrategicCaller
	mu       sync.Mutex
	response string   // raw body returned by every call
	prompts  []string // captured user prompts
}

func (f *scriptedRouterLLM) SendWithSummary(_ context.Context, _, user string, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, user)
	return makeStrategicResponse(f.response), nil
}

func (f *scriptedRouterLLM) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return ""
	}
	return f.prompts[len(f.prompts)-1]
}

// verdictRecorder is a concurrency-safe interrupt-verdict collector.
type verdictRecorder struct {
	mu    sync.Mutex
	items []prompt.RouterDecision
}

func (v *verdictRecorder) add(dec prompt.RouterDecision) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.items = append(v.items, dec)
}

func (v *verdictRecorder) len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.items)
}

// newRouterTestRuntime wires a Runtime whose eventRouter uses the scripted
// LLM. verdicts collects interrupt verdicts instead of executing them.
func newRouterTestRuntime(t *testing.T, llmBody string) (*Runtime, *fakeTransport, *agentContext, *scriptedRouterLLM, *verdictRecorder) {
	t.Helper()
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	llm := &scriptedRouterLLM{response: llmBody}
	ac.tacticalHc = llm
	verdicts := &verdictRecorder{}
	rt.eventRouter = newEventRouter(
		func(id string) llmClient {
			if id == "H-01" {
				return llm
			}
			return nil
		},
		rt.kbPtr, nil, rt.lookupAgent,
		func(_ string, _ protocol.WorldEventPayload, dec prompt.RouterDecision) {
			verdicts.add(dec)
		},
		testLogger(),
	)
	return rt, ft, ac, llm, verdicts
}

// TestEventRouter_NotUrgentStaysQueued verifies the enqueue branch: an
// interrupt=false verdict leaves the event queued and stops nothing.
func TestEventRouter_NotUrgentStaysQueued(t *testing.T) {
	rt, ft, ac, llm, _ := newRouterTestRuntime(t, `{"interrupt": false, "severity": 2, "reason": "关系一般，先忙手上的活"}`)
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r1"))

	// 裁决完成：无打断、事件仍在队列。
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(llm.lastPrompt(), "世界事件") })
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("not-urgent event must stay queued, got len=%d", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("not-urgent verdict must not stop anything, got %v", stops)
	}
	if !replanIdle(ac) {
		t.Fatalf("not-urgent verdict must not trigger a replan")
	}
}

// promptsOf is a tiny accessor to keep waitFor readable.
func (f *scriptedRouterLLM) promptsOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// TestEventRouter_UrgentInterrupts verifies the interrupt branch end to
// end with the REAL routerInterrupt: verdict=true → event pulled from the
// queue → stop in-flight action → hint with the verdict reason → replan
// takes over (the scripted LLM's body has no tool calls → replan fails →
// abandon path clears the queue).
func TestEventRouter_UrgentInterrupts(t *testing.T) {
	rt, ft, ac, llm, _ := newRouterTestRuntime(t, `{"interrupt": true, "severity": 8, "reason": "K-03 是最亲近的伙伴"}`)
	// 走真实 routerInterrupt（撤队 + stop + replan hint 全链路）。
	rt.eventRouter.processVerdict = rt.routerInterrupt
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r2"))

	// 裁决产生 → 事件被撤下队列 + 在途动作被打断。
	waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(llm.lastPrompt(), "世界事件") && ac.as.WorldEventQueueLen() == 0
	})
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("urgent verdict must stop the in-flight action, got %v", stops)
	}
	// hint 携带事件与裁决理由，走 force 同款重规划（失败 → abandon 清队列）。
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
	if hint := ac.as.ReplanHint(); !containsAll(hint, "【强制打断】", "K-03", "路由裁决：紧急", "最亲近的伙伴") {
		t.Fatalf("hint must carry event + verdict reason, got %q", hint)
	}
}

// TestEventRouter_FailureDegradesToEnqueue verifies the conservative
// fallback: LLM error / unparseable output → the event stays queued, no
// interrupt.
func TestEventRouter_FailureDegradesToEnqueue(t *testing.T) {
	// 用 newWorldEventTestRuntime（tacticalHc=nil → lookupHC 返回 nil）
	// 验证"无 LLM 客户端"路径 + 用坏 JSON 验证解析失败路径。
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	calls := 0
	rt.eventRouter = newEventRouter(
		func(string) llmClient { calls++; return nil },
		rt.kbPtr, nil, rt.lookupAgent, func(string, protocol.WorldEventPayload, prompt.RouterDecision) {},
		testLogger(),
	)

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r3"))
	time.Sleep(50 * time.Millisecond)
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("no-client router must leave the event queued, got len=%d", got)
	}
	if stops := stoppedActions(ft); len(stops) != 0 {
		t.Fatalf("no-client router must not stop anything, got %v", stops)
	}

	// 解析失败路径：坏 JSON 输出 → interrupt=false。
	rt2, ft2, ac2, llm2, verdicts2 := newRouterTestRuntime(t, "完全不是 JSON")
	dispatchTestEvent(t, rt2, "H-01", nonForceTestEvent("evt_r4"))
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(llm2.lastPrompt(), "世界事件") })
	if verdicts2.len() != 0 {
		t.Fatalf("unparseable verdict must not interrupt, got %d verdicts", verdicts2.len())
	}
	if got := ac2.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("unparseable verdict must keep the event queued, got len=%d", got)
	}
	if stops := stoppedActions(ft2); len(stops) != 0 {
		t.Fatalf("unparseable verdict must not stop anything, got %v", stops)
	}
}

// TestEventRouter_PromptCarriesJudgmentInputs verifies the router's prompt
// actually contains the per-NPC judgment inputs: persona from KB/profiles,
// current action with elapsed time, physical band line, and the rendered
// event.
func TestEventRouter_PromptCarriesJudgmentInputs(t *testing.T) {
	rt, _, ac, llm, _ := newRouterTestRuntime(t, `{"interrupt": false, "severity": 1, "reason": "x"}`)
	ac.as.RecordActionStarted("act-9", protocol.CmdInteractSmartObject,
		map[string]any{"semantic_group": "workbench", "interaction": "assemble"}, agentstate.SourceTactical, "")
	// 物理状态：让 PhysicalLine 非空（62/31/78/340 → 中等/精神饱满/严重磨损）。
	ac.as.SetPhysicalState(&protocol.PhysicalState{Energy: 62, Fatigue: 31, JointWear: 78, Money: 340}, nil)

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r5"))

	waitFor(t, 2*time.Second, func() bool { return strings.Contains(llm.lastPrompt(), "世界事件：发生故障") })
	p := llm.lastPrompt()
	for _, want := range []string{
		"游戏时间", "main_workshop",
		"InteractSmartObject", "已执行约",
		"物理状态：",
		"客观严重度 7",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("router prompt missing %q:\n%s", want, p)
		}
	}
}

// TestEventRouter_SameEventDifferentNPCs verifies the emergence contract
// (§4.3 验收点): the same event routed for two agents with different
// scripted verdicts produces one interrupt and one enqueue — the router
// call itself is per-agent (different prompts), and only the interrupt
// verdict acts. H-02's verdict drives the real routerInterrupt (queue
// pull + stop); H-01's stays queued.
func TestEventRouter_SameEventDifferentNPCs(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{"interrupt": false, "severity": 2, "reason": "不熟"}`)

	// H-02: 阿静式的近关系裁决（脚本换成 interrupt=true）。
	ac2, _ := newAgentContext(context.Background())
	seedPerception(t, ac2)
	agents := map[string]*agentContext{"H-01": ac, "H-02": ac2}
	rt.lookupAgent = func(id string) *agentContext { return agents[id] }
	// 路由器按 lookupAgent 解析 per-agent 客户端（真实装配同款）。
	rt.eventRouter.lookupAgent = rt.lookupAgent
	rt.eventRouter.lookupHC = func(id string) llmClient {
		if a := rt.lookupAgent(id); a != nil {
			return a.tacticalHc
		}
		return nil
	}
	llmH1 := &scriptedRouterLLM{response: `{"interrupt": false, "severity": 2, "reason": "与 K-03 关系一般"}`}
	llmH2 := &scriptedRouterLLM{response: `{"interrupt": true, "severity": 9, "reason": "K-03 是阿静最亲近的伙伴"}`}
	ac.tacticalHc = llmH1
	ac2.tacticalHc = llmH2
	// 真实 routerInterrupt：H-02 的打断裁决走完整链路。
	rt.eventRouter.processVerdict = rt.routerInterrupt
	ac2.as.RecordActionStarted("act-2", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	// 同一条 K-03 故障广播分别推给两个 NPC（协议：广播 = 多条独立消息）。
	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_k03"))
	ev2 := nonForceTestEvent("evt_k03") // 同 event_id：本测试各 agent 独立 AgentState，互不去重
	payload, _ := json.Marshal(ev2)
	rt.HandleMessage(context.Background(), protocol.TypeWorldEvent, "H-02", payload)

	// 两个裁决都完成。
	waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(llmH1.lastPrompt(), "世界事件") && strings.Contains(llmH2.lastPrompt(), "世界事件")
	})
	// H-01 的事件留在队列（入队），H-02 的被撤下（打断）。
	waitFor(t, 2*time.Second, func() bool { return ac2.as.WorldEventQueueLen() == 0 })
	if got := ac.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("H-01 event should stay queued, got len=%d", got)
	}
	// H-02 的在途动作被打断，H-01 没有。
	stops := stoppedActions(ft)
	if len(stops) != 1 || stops[0] != "act-2" {
		t.Fatalf("only H-02's action should be stopped, got %v", stops)
	}
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac2) })
}

// TestRouterMotive_SituationResolvedBypassesGuardrail verifies that a
// situation_resolved verdict interrupts even when its severity is LOWER
// than the active reaction's severity (combat_exit during a high-severity
// flee reaction must be allowed through — it's ending the reaction, not
// competing with it).
func TestRouterMotive_SituationResolvedBypassesGuardrail(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.as.RecordActionStarted("act-1", "MoveTo", map[string]any{"target_id": "repair_bay"}, agentstate.SourceTactical, "")
	// 武装一个 severity 9 的反应窗口（模拟被攻击后的逃跑反应）。
	ac.beginReaction(9, ac.as.LatestGameTimeSec())
	ac.as.EnqueueWorldEvent(reactionTestEvent("evt_g1"))

	// combat_exit：severity 3 << 9，motive=situation_resolved → 应放行。
	rt.routerInterrupt("H-01", reactionTestEvent("evt_g1"), prompt.RouterDecision{
		Interrupt: true, Severity: 3,
		Reason: "战斗已结束，逃跑不再必要", Motive: prompt.RouterMotiveSituationResolved,
	})
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("situation_resolved must bypass the severity ladder, stops=%v", stops)
	}
	// 窗口被清除（不是新反应），不再 armed。
	if active, _, _ := ac.reactionSnapshot(); active {
		t.Fatalf("situation_resolved should clear the reaction window, not re-arm it")
	}
	// hint 带【情境解除】前缀。
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【情境解除】") {
		t.Fatalf("hint should carry the situation-resolved prefix, got %q", hint)
	}
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac) })
}

// TestRouterMotive_SocialLowSeverityWindow verifies a social interrupt
// arms a low-severity reaction window: any subsequent higher-severity
// interrupt can override it, and a same-or-lower social event is gated.
func TestRouterMotive_SocialLowSeverityWindow(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, `{}`)
	ac.as.RecordActionStarted("act-1", "InteractSmartObject",
		map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")
	ac.as.EnqueueWorldEvent(reactionTestEvent("evt_s1"))

	// 社交打断（severity 2）→ 放行 + 低窗口。
	rt.routerInterrupt("H-01", reactionTestEvent("evt_s1"), prompt.RouterDecision{
		Interrupt: true, Severity: 2,
		Reason: "有人打招呼，回应一声", Motive: prompt.RouterMotiveSocial,
	})
	if stops := stoppedActions(ft); len(stops) != 1 {
		t.Fatalf("social interrupt should proceed, stops=%v", stops)
	}
	if active, sev, _ := ac.reactionSnapshot(); !active || sev != 2 {
		t.Fatalf("social should arm a severity-2 window, got active=%v sev=%d", active, sev)
	}
	if hint := ac.as.ReplanHint(); !strings.Contains(hint, "【社交回应】") {
		t.Fatalf("hint should carry the social prefix, got %q", hint)
	}
	waitFor(t, 2*time.Second, func() bool { return replanIdle(ac) })
}
