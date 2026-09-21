package main

// P2-5 轻量事件路由器运行时测试（jev 判决模型版）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/jev"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
)

// pf is a pointer-to-float64 helper for building scripted answers.
func pf(v float64) *float64 { return &v }

// judgeResp builds a full judgment response (three questions answered).
func judgeResp(noul float64, motive string, score float64) *jev.Response {
	return &jev.Response{
		Model: "jev-1.13.0",
		Answers: map[string]jev.Answer{
			"should_interrupt": {Type: jev.TypeNoul, Noul: pf(noul)},
			"motive":           {Type: jev.TypeChoice, Choice: motive, Confidence: pf(0.9)},
			"severity":         {Type: jev.TypeScore, Score: pf(score)},
		},
	}
}

// scriptedJudge implements routerJudge returning a scripted response (or
// error) per call. It records the requests so tests can assert the router
// sent the right state (conversation + user attributes).
type scriptedJudge struct {
	mu   sync.Mutex
	resp *jev.Response
	err  error
	reqs []*jev.Request
}

func newScriptedJudge(resp *jev.Response) *scriptedJudge { return &scriptedJudge{resp: resp} }

func notInterruptJudge() *scriptedJudge { return newScriptedJudge(judgeResp(0.1, "no_interrupt", 0.1)) }

func (f *scriptedJudge) Judge(_ context.Context, req *jev.Request) (*jev.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// LastRequestBody returns nil — dumpLastRequestBody no-ops on it in tests.
func (f *scriptedJudge) LastRequestBody() []byte { return nil }

// lastState renders the last request's state as one searchable blob — what
// the judgment model sees (conversation turns + user attributes, JSON).
func (f *scriptedJudge) lastState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return ""
	}
	b, _ := json.Marshal(f.reqs[len(f.reqs)-1].State)
	return string(b)
}

// lastReq returns the last request as-is (for structured assertions).
func (f *scriptedJudge) lastReq() *jev.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *scriptedJudge) statesOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		b, _ := json.Marshal(r.State)
		out = append(out, string(b))
	}
	return out
}

var _ routerJudge = (*scriptedJudge)(nil)

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
// judge. verdicts collects interrupt verdicts instead of executing them.
func newRouterTestRuntime(t *testing.T, j *scriptedJudge) (*Runtime, *fakeTransport, *agentContext, *scriptedJudge, *verdictRecorder) {
	t.Helper()
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	ac.jevHc = j
	verdicts := &verdictRecorder{}
	rt.eventRouter = newEventRouter(
		func(id string) routerJudge {
			if id == "H-01" {
				return j
			}
			return nil
		},
		rt.kbPtr, nil, rt.lookupAgent,
		func(_ string, _ protocol.WorldEventPayload, dec prompt.RouterDecision) {
			verdicts.add(dec)
		},
		testLogger(),
	)
	return rt, ft, ac, j, verdicts
}

// TestEventRouter_NotUrgentStaysQueued verifies the enqueue branch: a
// low-interrupt-probability verdict leaves the event queued and stops nothing.
func TestEventRouter_NotUrgentStaysQueued(t *testing.T) {
	rt, ft, ac, j, _ := newRouterTestRuntime(t, newScriptedJudge(judgeResp(0.15, "no_interrupt", 0.2)))
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r1"))

	// 裁决完成：无打断、事件仍在队列。
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j.lastState(), "世界事件") })
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

// TestEventRouter_UrgentInterrupts verifies the interrupt branch end to
// end with the REAL routerInterrupt: noul above threshold → event pulled
// from the queue → stop in-flight action → hint with the verdict reason →
// replan takes over (the scripted judge's replan has no tool calls →
// replan fails → abandon path clears the queue).
func TestEventRouter_UrgentInterrupts(t *testing.T) {
	// noul 0.92 + urgent + score 0.8 → severity 8。
	rt, ft, ac, j, _ := newRouterTestRuntime(t, newScriptedJudge(judgeResp(0.92, "urgent", 0.8)))
	// 走真实 routerInterrupt（撤队 + stop + replan hint 全链路）。
	rt.eventRouter.processVerdict = rt.routerInterrupt
	ac.as.RecordActionStarted("act-1", protocol.CmdWorkShift, nil, agentstate.SourceTactical, "")
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r2"))

	// 裁决产生 → 事件被撤下队列 + 在途动作被打断。
	waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(j.lastState(), "世界事件") && ac.as.WorldEventQueueLen() == 0
	})
	if stops := stoppedActions(ft); len(stops) != 1 || stops[0] != "act-1" {
		t.Fatalf("urgent verdict must stop the in-flight action, got %v", stops)
	}
	// hint 携带事件与裁决理由（jev 无自由文本，理由由概率/动机/紧急度合成），
	// 走 force 同款重规划（失败 → abandon 清队列）。
	waitFor(t, 2*time.Second, func() bool { return ac.as.QueueLen() == 0 && replanIdle(ac) })
	if hint := ac.as.ReplanHint(); !containsAll(hint, "【强制打断】", "K-03", "路由裁决：紧急", "打断概率0.92") {
		t.Fatalf("hint must carry event + verdict reason, got %q", hint)
	}
}

// TestEventRouter_FailureDegradesToEnqueue verifies the conservative
// fallback: no judgment client / call error / unusable answers → the event
// stays queued, no interrupt.
func TestEventRouter_FailureDegradesToEnqueue(t *testing.T) {
	// 无判决客户端（--jev-model=""）：lookupJudge 返回 nil。
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	calls := 0
	rt.eventRouter = newEventRouter(
		func(string) routerJudge { calls++; return nil },
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

	// 调用失败路径（超时/网络等）。
	rt2, ft2, ac2, j2, verdicts2 := newRouterTestRuntime(t,
		&scriptedJudge{err: errors.New("jev status 403: 模型不存在")})
	dispatchTestEvent(t, rt2, "H-01", nonForceTestEvent("evt_r4"))
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j2.lastState(), "世界事件") })
	if verdicts2.len() != 0 {
		t.Fatalf("failed call must not interrupt, got %d verdicts", verdicts2.len())
	}
	if got := ac2.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("failed call must keep the event queued, got len=%d", got)
	}
	if stops := stoppedActions(ft2); len(stops) != 0 {
		t.Fatalf("failed call must not stop anything, got %v", stops)
	}

	// 响应缺 should_interrupt（ok=false）→ 同样保守入队。
	rt3, ft3, ac3, j3, verdicts3 := newRouterTestRuntime(t,
		newScriptedJudge(&jev.Response{Answers: map[string]jev.Answer{}}))
	dispatchTestEvent(t, rt3, "H-01", nonForceTestEvent("evt_r4b"))
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j3.lastState(), "世界事件") })
	if verdicts3.len() != 0 {
		t.Fatalf("unusable answers must not interrupt, got %d verdicts", verdicts3.len())
	}
	if got := ac3.as.WorldEventQueueLen(); got != 1 {
		t.Fatalf("unusable answers must keep the event queued, got len=%d", got)
	}
	if stops := stoppedActions(ft3); len(stops) != 0 {
		t.Fatalf("unusable answers must not stop anything, got %v", stops)
	}
}

// TestEventRouter_StateCarriesJudgmentInputs verifies the judgment state
// actually contains the per-NPC judgment inputs: current action with
// elapsed time, physical band line, and the rendered event.
func TestEventRouter_StateCarriesJudgmentInputs(t *testing.T) {
	rt, _, ac, j, _ := newRouterTestRuntime(t, notInterruptJudge())
	ac.as.RecordActionStarted("act-9", protocol.CmdInteractSmartObject,
		map[string]any{"semantic_group": "workbench", "interaction": "assemble"}, agentstate.SourceTactical, "")
	// 物理状态：让 PhysicalLine 非空（62/31/78/340 → 中等/精神饱满/严重磨损）。
	ac.as.SetPhysicalState(&protocol.PhysicalState{Energy: 62, Fatigue: 31, JointWear: 78, Money: 340}, nil)

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_r5"))

	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j.lastState(), "世界事件：发生故障") })
	p := j.lastState()
	for _, want := range []string{
		"游戏时间", "main_workshop",
		"InteractSmartObject", "已执行约",
		"物理状态：",
		"客观严重度 7",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("router state missing %q:\n%s", want, p)
		}
	}
}

// TestRouterConversation_Rendering verifies the agentic-loop history →
// judgment "conversation" array mapping: user turns verbatim, assistant text
// kept, assistant tool_calls rendered as compact one-liners, tool placeholders
// skipped, and the tail-window cap applied.
func TestRouterConversation_Rendering(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	as.AppendConversationMessage(llmtypes.Message{Role: "user", Content: "当前时段目标：清晨冥想收心"})
	as.AppendConversationMessage(llmtypes.Message{
		Role: "assistant",
		ToolCalls: []llmtypes.ToolCall{
			{ID: "tc-1", Type: "function", Function: llmtypes.ToolFunction{
				Name:      "rest_at_residence",
				Arguments: `{"semantic_group":"sleep_pod","interaction":"meditate"}`,
			}},
			{ID: "tc-2", Type: "function", Function: llmtypes.ToolFunction{
				Name:      "speak",
				Arguments: `{"content":"先定神再开工"}`,
			}},
		},
	})
	as.AppendConversationMessage(llmtypes.Message{Role: "tool", Content: "result=pending", ToolCallID: "tc-1"})
	as.AppendConversationMessage(llmtypes.Message{Role: "assistant", Content: "冥想结束，准备开工"})

	conv := routerConversation(as)
	if len(conv) != 3 { // user + assistant(tool_calls) + assistant(text)；tool 占位跳过
		t.Fatalf("conversation len = %d, want 3 (tool placeholder skipped):\n%+v", len(conv), conv)
	}
	if conv[0].Role != "user" || conv[0].Content != "当前时段目标：清晨冥想收心" {
		t.Errorf("conv[0] wrong: %+v", conv[0])
	}
	// assistant tool_calls → describeAction 单行渲染（多个以"；"连接）。
	if !strings.Contains(conv[1].Content, "rest_at_residence(semantic_group=sleep_pod, interaction=meditate)") ||
		!strings.Contains(conv[1].Content, "speak(content=先定神再开工)") {
		t.Errorf("conv[1] tool_calls rendering wrong: %+v", conv[1])
	}
	if conv[2].Content != "冥想结束，准备开工" {
		t.Errorf("conv[2] assistant text wrong: %+v", conv[2])
	}

	// 窗口截断：超窗时只留尾部。
	for i := 0; i < routerConversationWindow+5; i++ {
		as.AppendConversationMessage(llmtypes.Message{Role: "user", Content: fmt.Sprintf("第 %d 条", i)})
	}
	conv = routerConversation(as)
	if len(conv) > routerConversationWindow {
		t.Fatalf("conversation must be capped at %d, got %d", routerConversationWindow, len(conv))
	}
	if last := conv[len(conv)-1].Content; last != fmt.Sprintf("第 %d 条", routerConversationWindow+4) {
		t.Errorf("window must keep the tail, last = %q", last)
	}
}

// TestEventRouter_ConversationRidesInState verifies route() puts the
// agentic-loop history into the judgment request's state.conversation —
// the router's context knowledge.
func TestEventRouter_ConversationRidesInState(t *testing.T) {
	rt, _, ac, j, _ := newRouterTestRuntime(t, notInterruptJudge())
	ac.as.AppendConversationMessage(llmtypes.Message{Role: "user", Content: "当前时段目标：清晨冥想收心"})
	ac.as.AppendConversationMessage(llmtypes.Message{Role: "assistant", Content: "冥想中"})

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_conv_1"))

	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j.lastState(), "世界事件：发生故障") })
	req := j.lastReq()
	if req == nil {
		t.Fatal("judge never called")
	}
	found := false
	for _, m := range req.State.Conversation {
		if m.Role == "user" && m.Content == "当前时段目标：清晨冥想收心" {
			found = true
		}
	}
	if !found {
		t.Fatalf("state.conversation must carry the agentic-loop history:\n%+v", req.State.Conversation)
	}
	usr, ok := req.State.User.(map[string]string)
	if !ok || !strings.Contains(usr["event"], "K-03 关节锁死") {
		t.Fatalf("state.user must carry the event:\n%v", req.State.User)
	}
}

// TestEventRouter_SameEventDifferentNPCs verifies the emergence contract
// (§4.3 验收点): the same event routed for two agents with different
// scripted verdicts produces one interrupt and one enqueue — the judgment
// call itself is per-agent (different states), and only the interrupt
// verdict acts. H-02's verdict drives the real routerInterrupt (queue
// pull + stop); H-01's stays queued.
func TestEventRouter_SameEventDifferentNPCs(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())

	// H-02: 阿静式的近关系裁决（脚本换成高打断概率）。
	ac2, _ := newAgentContext(context.Background())
	seedPerception(t, ac2)
	agents := map[string]*agentContext{"H-01": ac, "H-02": ac2}
	rt.lookupAgent = func(id string) *agentContext { return agents[id] }
	// 路由器按 lookupAgent 解析 per-agent 判决客户端（真实装配同款）。
	rt.eventRouter.lookupAgent = rt.lookupAgent
	rt.eventRouter.lookupJudge = func(id string) routerJudge {
		if a := rt.lookupAgent(id); a != nil {
			return a.jevHc
		}
		return nil
	}
	jH1 := newScriptedJudge(judgeResp(0.15, "no_interrupt", 0.2))
	jH2 := newScriptedJudge(judgeResp(0.95, "urgent", 0.9))
	ac.jevHc = jH1
	ac2.jevHc = jH2
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
		return strings.Contains(jH1.lastState(), "世界事件") && strings.Contains(jH2.lastState(), "世界事件")
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

// TestRouterDecisionFromJev pins the verdict mapping: threshold boundary,
// motive whitelist, self-contradiction degradation, severity scaling and
// the social cap.
func TestRouterDecisionFromJev(t *testing.T) {
	cases := []struct {
		name       string
		resp       *jev.Response
		wantOK     bool
		wantInter  bool
		wantMotive string
		wantSev    int
	}{
		{
			name:   "nil response",
			resp:   nil,
			wantOK: false,
		},
		{
			name:   "missing should_interrupt",
			resp:   &jev.Response{Answers: map[string]jev.Answer{}},
			wantOK: false,
		},
		{
			name: "should_interrupt without noul value",
			resp: &jev.Response{Answers: map[string]jev.Answer{
				"should_interrupt": {Type: jev.TypeNoul},
			}},
			wantOK: false,
		},
		{
			name:   "boundary 0.5 stays conservative",
			resp:   judgeResp(0.5, "urgent", 0.5),
			wantOK: true, wantInter: false, wantMotive: "urgent", wantSev: 5,
		},
		{
			name:   "boundary 0.51 interrupts",
			resp:   judgeResp(0.51, "urgent", 0.5),
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 5,
		},
		{
			name:   "noul 0.92 urgent score 0.86 → severity 9",
			resp:   judgeResp(0.92, "urgent", 0.86),
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 9,
		},
		{
			name:   "score 1.05 clamps to 10",
			resp:   judgeResp(0.95, "urgent", 1.05),
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 10,
		},
		{
			name:   "score negative clamps to 0",
			resp:   judgeResp(0.6, "urgent", -0.2),
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 0,
		},
		{
			name: "missing severity → 0",
			resp: &jev.Response{Answers: map[string]jev.Answer{
				"should_interrupt": {Type: jev.TypeNoul, Noul: pf(0.9)},
			}},
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 0,
		},
		{
			name:   "social severity capped at 3",
			resp:   judgeResp(0.9, "social", 0.8),
			wantOK: true, wantInter: true, wantMotive: "social", wantSev: 3,
		},
		{
			name:   "situation_resolved keeps its severity",
			resp:   judgeResp(0.85, "situation_resolved", 0.3),
			wantOK: true, wantInter: true, wantMotive: "situation_resolved", wantSev: 3,
		},
		{
			name:   "unknown motive degrades to urgent",
			resp:   judgeResp(0.9, "garbage", 0.5),
			wantOK: true, wantInter: true, wantMotive: "urgent", wantSev: 5,
		},
		{
			name:   "self-contradiction noul high + no_interrupt → conservative",
			resp:   judgeResp(0.8, "no_interrupt", 0.5),
			wantOK: true, wantInter: false, wantSev: 5,
		},
		{
			name:   "low noul stays queued",
			resp:   judgeResp(0.15, "no_interrupt", 0.1),
			wantOK: true, wantInter: false, wantSev: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, ok := routerDecisionFromJev(tc.resp)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if dec.Interrupt != tc.wantInter {
				t.Fatalf("interrupt = %v, want %v", dec.Interrupt, tc.wantInter)
			}
			if tc.wantMotive != "" && dec.Motive != tc.wantMotive {
				t.Fatalf("motive = %q, want %q", dec.Motive, tc.wantMotive)
			}
			if dec.Severity != tc.wantSev {
				t.Fatalf("severity = %d, want %d", dec.Severity, tc.wantSev)
			}
			if dec.Reason == "" {
				t.Fatalf("reason must never be empty, got %+v", dec)
			}
		})
	}
}

// TestRouterMotive_SituationResolvedBypassesGuardrail verifies that a
// situation_resolved verdict interrupts even when its severity is LOWER
// than the active reaction's severity (combat_exit during a high-severity
// flee reaction must be allowed through — it's ending the reaction, not
// competing with it).
func TestRouterMotive_SituationResolvedBypassesGuardrail(t *testing.T) {
	rt, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
	ac.as.RecordActionStarted("act-1", "MoveTo", map[string]any{"target_id": "repair_bay"}, agentstate.SourceTactical, "")
	// 武装一个 severity 9 的反应窗口（模拟被攻击后的逃跑反应）。
	ac.beginReaction(9, ac.as.LatestGameTimeSec(), "")
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
	rt, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
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

// TestRouter_ReactionDescInjectedIntoState pins the 2026-09-20 fix: when a
// reaction window is armed, the judgment state's 【当前动作】 carries the
// reaction context — the judgment model can see "this MoveTo is a flee
// response" and correctly judge a combat_exit as situation_resolved.
func TestRouter_ReactionDescInjectedIntoState(t *testing.T) {
	rt, _, ac, j, _ := newRouterTestRuntime(t, notInterruptJudge())
	seedPerception(t, ac)
	ac.as.RecordActionStarted("act-1", "MoveTo",
		map[string]any{"target_type": "zone", "target_id": "residential_quarters"}, agentstate.SourceTactical, "")
	// 武装一个带描述的反应窗口（模拟被攻击后的逃跑反应）。
	ac.beginReaction(9, ac.as.LatestGameTimeSec(), "玩家互动：被玩家 player_1 攻击（伤害 20，类型 physical）")

	dispatchTestEvent(t, rt, "H-01", nonForceTestEvent("evt_desc_1"))
	waitFor(t, 2*time.Second, func() bool { return strings.Contains(j.lastState(), "世界事件") })

	p := j.lastState()
	if !strings.Contains(p, "这是对紧急事件的反应动作") {
		t.Fatalf("router state must carry the reaction context when the window is armed:\n%s", p)
	}
	if !strings.Contains(p, "被玩家 player_1 攻击") {
		t.Fatalf("router state must carry the reaction's origin event:\n%s", p)
	}
}

// TestTacticalRefill_ReactionContextHint verifies the 2026-09-20 fix: when a
// reaction window is still armed and the action completes (refill fires),
// the tactical prompt carries the reaction context — the LLM knows it was
// mid-reaction, not just seeing "工作台装配" with no transition context.
func TestTacticalRefill_ReactionContextHint(t *testing.T) {
	_, ft, ac, _, _ := newRouterTestRuntime(t, notInterruptJudge())
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.beginReaction(9, ac.as.LatestGameTimeSec(), "被玩家攻击")

	// fakeLoopLLM 捕获 prompt + speakToolCallResp 让分解成功（hint 被
	// BeginTacticalRefill 消费后进 prompt，ReplanHint() 读回为空——需从
	// 捕获的 prompt 断言）。
	fake := &fakeLoopLLM{resp: speakToolCallResp()}
	ac.tacticalHc = fake
	_ = ac.tacticalRefill(context.Background(), "H-01", ft, nil, nil, testLogger())

	user := lastUserPromptOf(t, fake.capturedMsgs)
	if !strings.Contains(user, "正在应对紧急事件") {
		t.Fatalf("refill prompt during a reaction window must carry the transition hint:\n%s", user)
	}
	if !strings.Contains(user, "被玩家攻击") {
		t.Fatalf("refill prompt must carry the reaction's origin event:\n%s", user)
	}
}
