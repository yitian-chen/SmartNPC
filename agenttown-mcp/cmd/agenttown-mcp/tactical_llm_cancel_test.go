package main

// P1-4（§3.3 唯一盲区）测试：force 事件掐掉在途战术层 LLM 请求。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

// blockingTacticalLLM implements llmClient for cancellation tests: the first
// SendLoop blocks until its ctx is cancelled (returning ctx.Err()); calls
// after the first return failAfter immediately. started signals the first
// call is in flight; calls counts invocations so tests can observe the
// takeover (the force replan's own call).
type blockingTacticalLLM struct {
	fakeStrategicCaller
	started   chan struct{}
	calls     atomic.Int32
	blockOnce sync.Once
	failAfter error
}

func newBlockingTacticalLLM(failAfter error) *blockingTacticalLLM {
	return &blockingTacticalLLM{started: make(chan struct{}), failAfter: failAfter}
}

func (f *blockingTacticalLLM) SendLoop(ctx context.Context, _ []llmtypes.Message, _ []venus.Tool, _, _ string, _ []byte) (*llmtypes.Response, error) {
	n := f.calls.Add(1)
	if n == 1 {
		f.blockOnce.Do(func() { close(f.started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, f.failAfter
}

// TestCancelledByForce_Decision covers the classification contract: only
// "error is context.Canceled while the parent context is still alive" means
// a force cancel — parent shutdown keeps normal failure semantics, and
// timeouts / plain errors never count.
func TestCancelledByForce_Decision(t *testing.T) {
	cancelledParent, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		err    error
		parent context.Context
		want   bool
	}{
		{"force cancel", context.Canceled, context.Background(), true},
		{"wrapped force cancel", errors.Join(errors.New("tactical llm: "), context.Canceled), context.Background(), true},
		{"parent shutdown", context.Canceled, cancelledParent, false},
		{"timeout", context.DeadlineExceeded, context.Background(), false},
		{"plain failure", errors.New("connection refused"), context.Background(), false},
		{"nil error", nil, context.Background(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cancelledByForce(tc.err, tc.parent); got != tc.want {
				t.Fatalf("cancelledByForce(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRegisterTacticalLLMCall_GenerationGuard verifies the registration
// lifecycle: a cancelled stale call's deregister must not clear a newer
// call's registration (generation-guarded), and cancel only reaches the
// currently registered call.
func TestRegisterTacticalLLMCall_GenerationGuard(t *testing.T) {
	ac, _ := newAgentContext(context.Background())

	ctx1, dereg1 := ac.registerTacticalLLMCall(context.Background())
	if ac.cancelInFlightTacticalLLM() != true {
		t.Fatalf("first registration should be cancellable")
	}
	<-ctx1.Done() // cancel reached ctx1

	// New call registers before the stale one deregisters.
	ctx2, dereg2 := ac.registerTacticalLLMCall(context.Background())
	dereg1() // stale deregister must NOT clear ctx2's registration
	if ac.cancelInFlightTacticalLLM() != true {
		t.Fatalf("cancel should still reach the newer registration after stale dereg")
	}
	select {
	case <-ctx2.Done():
	default:
		t.Fatalf("ctx2 should have been cancelled")
	}
	dereg2()
	if ac.cancelInFlightTacticalLLM() {
		t.Fatalf("cancel after final dereg should be a no-op")
	}
}

// TestForceEvent_CancelsInFlightReplanLLM is the end-to-end §3.3 scenario:
// a reactive-layer replan holds the slot with its LLM call in flight; a
// force event (1) aborts that call immediately, (2) the cancelled replan
// reports cancelled=true and does no fallback, (3) the force replan takes
// over the slot within milliseconds (not the old ~70s window), and its
// failure path leaves the force hint intact.
func TestForceEvent_CancelsInFlightReplanLLM(t *testing.T) {
	rt, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac) // D12 10:47:03 → falls in the 09:00-12:00 slot
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	llm := newBlockingTacticalLLM(errors.New("post-block failure"))
	ac.tacticalHc = llm
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	// 模拟反应层 replan 持有 slot（设 replanInProgress + 调 replan，defer
	// 释放——与 reactive execute 的真实时序一致）。
	var holderCancelled bool
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		ac.coordMu.Lock()
		ac.replanInProgress = true
		ac.coordMu.Unlock()
		defer func() {
			ac.coordMu.Lock()
			ac.replanInProgress = false
			ac.coordMu.Unlock()
		}()
		_, holderCancelled = ac.tacticalRefillForReplan(context.Background(), "H-01",
			ft, nil, nil, testLogger(), "反应层 replan 原因")
	}()

	select {
	case <-llm.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("holder replan never reached its LLM call")
	}

	t0 := time.Now()
	dispatchTestEvent(t, rt, "H-01", forceTestEvent("evt_cancel_1"))

	// 1) 在途调用被掐掉，holder 快速返回且报告 cancelled。
	select {
	case <-holderDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("in-flight replan did not unblock after force cancel")
	}
	if !holderCancelled {
		t.Fatalf("cancelled replan must report cancelled=true")
	}
	// 2) force replan 快速接管（远小于旧 ~70s 窗口；断言 2s 上限）。
	waitFor(t, 2*time.Second, func() bool { return llm.calls.Load() >= 2 })
	if elapsed := time.Since(t0); elapsed > 2*time.Second {
		t.Fatalf("force replan takeover took %v — cancellation did not shrink the window", elapsed)
	}
	// 3) force replan（第二次调用失败）走 abandon：清队列 + force hint 保留。
	waitFor(t, 2*time.Second, func() bool {
		return ac.as.QueueLen() == 0 && replanIdle(ac)
	})
	if hint := ac.as.ReplanHint(); !containsAll(hint, "【强制打断】", "player_1") {
		t.Fatalf("force hint must survive the takeover, got %q", hint)
	}
	if hint := ac.as.ReplanHint(); containsAny(hint, "反应层 replan 原因") {
		t.Fatalf("cancelled holder must not have overwritten the hint, got %q", hint)
	}
}

// TestTacticalRefillForReplan_CancelledNoFallback verifies the cancelled
// contract at unit level: queue preserved (no ClearForReplan in the failure
// path), cancelled=true, when the LLM call is aborted via the registry.
func TestTacticalRefillForReplan_CancelledNoFallback(t *testing.T) {
	_, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.tacticalHc = newBlockingTacticalLLM(errors.New("post-block failure"))
	ac.as.RefillQueue([]agentstate.PlannedAction{
		{Action: "InteractSmartObject", Params: map[string]any{"semantic_group": "workbench"}},
	}, "09:00-12:00")

	type result struct {
		ok        bool
		cancelled bool
	}
	done := make(chan result, 1)
	go func() {
		ok, cancelled := ac.tacticalRefillForReplan(context.Background(), "H-01",
			ft, nil, nil, testLogger(), "反应层 replan 原因")
		done <- result{ok, cancelled}
	}()
	select {
	case <-ac.tacticalHc.(*blockingTacticalLLM).started:
	case <-time.After(2 * time.Second):
		t.Fatalf("replan never reached its LLM call")
	}

	ac.cancelInFlightTacticalLLM()

	select {
	case res := <-done:
		if res.ok || !res.cancelled {
			t.Fatalf("cancelled replan must return (ok=false, cancelled=true), got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("replan did not unblock after cancel")
	}
	// 取消路径不动旧队列（保留由调用方决定后续）。
	if got := ac.as.QueueLen(); got != 1 {
		t.Fatalf("old queue must be preserved on force-cancel, got len=%d", got)
	}
}

// TestTacticalRefill_WorkerRefillSkipsFallbackOnCancel verifies the worker's
// plain refill path: a force-cancelled decomposition must NOT enqueue the
// fallback actions (the "网络波动了" speak would fight the force response).
// A queue left empty lets the force replan's queue land uncontested.
func TestTacticalRefill_WorkerRefillSkipsFallbackOnCancel(t *testing.T) {
	_, ft, ac := newWorldEventTestRuntime(t)
	seedPerception(t, ac)
	ac.as.SetDailyPlan("09:00-12:00: 车间装配作业", 11)
	ac.tacticalHc = newBlockingTacticalLLM(errors.New("post-block failure"))

	done := make(chan bool, 1)
	go func() {
		done <- ac.tacticalRefill(context.Background(), "H-01", ft, nil, nil, testLogger())
	}()
	select {
	case <-ac.tacticalHc.(*blockingTacticalLLM).started:
	case <-time.After(2 * time.Second):
		t.Fatalf("worker refill never reached its LLM call")
	}

	ac.cancelInFlightTacticalLLM()

	select {
	case ok := <-done:
		if ok {
			t.Fatalf("cancelled refill must return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("worker refill did not unblock after cancel")
	}
	// 关键断言：没有兜底动作入队（对照普通失败路径会补 speak+look_around）。
	if got := ac.as.QueueLen(); got != 0 {
		t.Fatalf("force-cancelled refill must not enqueue fallback actions, got len=%d", got)
	}
}

// helpers

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
