package main

// P3-9 升格测试：事件驱动战略层 replan——触发/节流/合并写回/留痕。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

// fakeLoopLLM 需要 tools 目录（agenticTurn 会派生 tools）——空目录即可
// （capabilityRegistryRef 为 nil 时 tacticalToolsFromRegistry 返回空）。

func planJSONResponse(items string) *llmtypes.Response {
	return makeStrategicResponse(`[` + items + `]`)
}

// TestTryBeginStrategicReplan_Throttle covers the gate matrix: per-day cap,
// game-time debounce, day rollover resets the counter.
func TestTryBeginStrategicReplan_Throttle(t *testing.T) {
	s := agentstate.New()
	// 首次放行。
	if !s.TryBeginStrategicReplan(10*3600, 11, 2, 7200) {
		t.Fatalf("first attempt should pass")
	}
	// 间隔不足（10h → 11h，差 1h < 2h）：拦截。
	if s.TryBeginStrategicReplan(11*3600, 11, 2, 7200) {
		t.Fatalf("within debounce gap should be gated")
	}
	// 间隔足够：放行，直至当日上限。
	if !s.TryBeginStrategicReplan(13*3600, 11, 2, 7200) {
		t.Fatalf("after gap should pass")
	}
	if s.TryBeginStrategicReplan(20*3600, 11, 2, 7200) {
		t.Fatalf("daily cap reached should be gated")
	}
	// 跨日：计数重置。
	if !s.TryBeginStrategicReplan(30*3600, 12, 2, 7200) {
		t.Fatalf("new day should reset the cap")
	}
}

// TestRecordPlanRevision_Bounded verifies the revision log keeps the most
// recent notes only.
func TestRecordPlanRevision_Bounded(t *testing.T) {
	s := agentstate.New()
	for i := 0; i < 40; i++ {
		s.RecordPlanRevision("rev")
	}
	if got := len(s.PlanRevisions()); got != 32 {
		t.Fatalf("revision log should cap at 32, got %d", got)
	}
}

// TestMergeRemainder covers the merge rules: fully-past old slots stay as
// facts, everything else (running/future) is replaced by the revision;
// cross-midnight slots are normalized (an un-started night slot is future).
func TestMergeRemainder(t *testing.T) {
	oldPlan := "07:00-09:00: 晨练\n09:00-12:00: 上午装配\n14:00-18:00: 下午拆解\n22:00-06:00: 夜间休眠"
	newPlan := "12:30-15:00: 处理故障后续\n15:00-18:00: 下午拆解（压缩）\n19:00-22:00: 晚间休息\n22:00-06:00: 夜间休眠"

	// 12:30：前两段已过（end<=750），后两段（含未开始的夜间）被替换。
	got := mergeRemainder(oldPlan, newPlan, "12:30")
	want := "07:00-09:00: 晨练\n09:00-12:00: 上午装配\n" + newPlan
	if got != want {
		t.Fatalf("merge =\n%q\nwant\n%q", got, want)
	}
	if n := countPastSlots(oldPlan, "12:30"); n != 2 {
		t.Fatalf("past slots = %d, want 2", n)
	}

	// 首时段未过（6:30）：完全替换。
	if got := mergeRemainder(oldPlan, newPlan, "06:30"); got != newPlan {
		t.Fatalf("no past slots → pure replacement, got %q", got)
	}
}

// TestStrategicReplan_EndToEnd drives the full flow the user observed:
// reaction active when the slot boundary hits → worker's advanceSlotIfNeeded
// reports the cut → strategic replan regenerates the REMAINDER, writes back
// the merged plan (past slots preserved), points currentPlanIndex at the
// first new slot, and records a revision. The throttle gates a second run.
func TestStrategicReplan_EndToEnd(t *testing.T) {
	ac, ctx := newAgentContext(context.Background())
	ac.autoPlanEnabled = true
	fake := &fakeLoopLLM{resp: planJSONResponse(
		`{"time":"12:30-15:00","goal":"处理故障后续"},{"time":"15:00-18:00","goal":"下午拆解"},{"time":"19:00-22:00","goal":"晚间休息"}`)}
	ac.strategicHc = fake
	ft := &fakeTransport{connected: true}

	// 当日计划 + 已过 12:30 的当前时段 + 在途动作 + 活跃反应窗口。
	ac.as.SetDailyPlan("07:00-09:00: 晨练\n09:00-12:00: 上午装配\n14:00-18:00: 下午拆解\n22:00-06:00: 夜间休眠", 11)
	perc := protocolPerceptionAt(t, 11, 45060) // D12 12:31:00
	if _, err := ac.as.SetPerception(perc); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	ac.as.CommitTacticalRefill("09:00-12:00", 1, false)
	ac.as.RecordActionStarted("act-1", "WorkShift", map[string]any{"semantic_group": "workbench"}, agentstate.SourceTactical, "")
	ac.beginReaction(8, ac.as.LatestGameTimeSec(), "")

	// 时段切换：反应被切断。
	if !ac.advanceSlotIfNeeded(ft, "H-01", testLogger()) {
		t.Fatalf("slot switch during an active reaction must report the cut")
	}
	// 触发战略 replan（同步走 maybe，内部 go strategicReplan）。
	ac.maybeStrategicReplan(ctx, "H-01", ft, nil, nil, nil, testLogger(), "紧急反应跨越时段边界被切断")

	// 等 replan 完成：修订留痕出现（mutex 保护，写回后必然存在）+ slot 释放。
	// 不能只等 replanIdle——goroutine 未 acquire 时也 idle（既有歧义）。
	waitFor(t, 3*time.Second, func() bool {
		return len(ac.as.PlanRevisions()) == 1 && replanIdle(ac)
	})
	plan, slot, idx := ac.as.SnapshotSchedule()
	// 语义断言（不锁死时间：首段前伸到触发时刻 12:31 + 时段节点随机抖动
	// 是既有设计）：前两段保留为既成事实，后三段为新计划的 goal，首个新
	// 时段起点 ≥ 触发时刻。
	items := parseFormattedPlan(plan)
	if len(items) != 5 {
		t.Fatalf("revised plan should have 5 slots (2 past + 3 new), got %d:\n%s", len(items), plan)
	}
	if items[0].Goal != "晨练" || items[0].Time != "07:00-09:00" || items[1].Goal != "上午装配" {
		t.Fatalf("past slots must be preserved verbatim:\n%s", plan)
	}
	if items[2].Goal != "处理故障后续" || items[3].Goal != "下午拆解" || items[4].Goal != "晚间休息" {
		t.Fatalf("new remainder goals wrong:\n%s", plan)
	}
	if s, _, ok := prompt.SplitPlanRange(items[2].Time); !ok || s < 751 {
		t.Fatalf("first new slot must start at/after the trigger time 12:31, got %s", items[2].Time)
	}
	_ = slot
	if idx != 2 {
		t.Fatalf("currentPlanIndex = %d, want 2 (first new slot)", idx)
	}
	if revs := ac.as.PlanRevisions(); len(revs) != 1 || !strings.Contains(revs[0], "紧急反应跨越时段边界被切断") {
		t.Fatalf("revision note = %v", revs)
	}
	// 战略 prompt 带修订上下文（原计划 + 偏差）。
	if len(fake.capturedMsgs) == 0 {
		t.Fatalf("strategic LLM was never called")
	}
	if u := lastUserPromptOf(t, fake.capturedMsgs); !strings.Contains(u, "当日计划修订") || !strings.Contains(u, "原当日计划") {
		t.Fatalf("replan prompt missing revision context:\n%s", u)
	}

	// 节流：紧接的第二次触发被拦（间隔不足），不再调 LLM。
	calls := len(fake.capturedMsgs)
	ac.maybeStrategicReplan(ctx, "H-01", ft, nil, nil, nil, testLogger(), "再次触发")
	time.Sleep(200 * time.Millisecond)
	if len(fake.capturedMsgs) != calls {
		t.Fatalf("debounced trigger must not call the LLM again, %d -> %d", calls, len(fake.capturedMsgs))
	}
}

// TestStrategicReplan_ManualModeGated verifies --auto-plan=false suppresses
// the replan (no plan to revise in manual mode).
func TestStrategicReplan_ManualModeGated(t *testing.T) {
	ac, ctx := newAgentContext(context.Background())
	ac.autoPlanEnabled = false
	fake := &fakeLoopLLM{resp: planJSONResponse(`{"time":"12:30-15:00","goal":"x"}`)}
	ac.strategicHc = fake
	perc := protocolPerceptionAt(t, 11, 45060)
	if _, err := ac.as.SetPerception(perc); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	ac.as.SetDailyPlan("09:00-12:00: 上午装配", 11)

	ac.maybeStrategicReplan(ctx, "H-01", nil, nil, nil, nil, testLogger(), "手动模式")
	time.Sleep(100 * time.Millisecond)
	if len(fake.capturedMsgs) != 0 {
		t.Fatalf("manual mode must not trigger the strategic replan")
	}
	if plan, _, _ := ac.as.SnapshotSchedule(); plan != "09:00-12:00: 上午装配" {
		t.Fatalf("manual mode plan must stay untouched, got %q", plan)
	}
}

// protocolPerceptionAt builds a perception JSON at the given day/todSec.
func protocolPerceptionAt(t *testing.T, day int, todSec float64) json.RawMessage {
	return mustMarshalBarPerception(t, day, todSec, float64(day)*86400+todSec)
}

// silence unused import in minimal test configurations.
var _ = venus.Tool{}
var _ sync.Mutex

// TestParseDailyPlan_BracketlessTolerance pins the 2026-09-18 field failure:
// the replan LLM occasionally drops the outer array brackets (comma-separated
// {...},{...} with a stray trailing ]), which made both event-driven
// revisions fail parsing and silently keep the old plan.
func TestParseDailyPlan_BracketlessTolerance(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"no brackets, no trailing", `{"time":"12:39-13:15","goal":"避一避"},{"time":"13:15-15:30","goal":"拆解"}`, 2},
		{"no opening bracket, stray trailing ]", "\n\n{\"time\":\"12:39-13:15\",\"goal\":\"避一避\"},{\"time\":\"13:15-15:30\",\"goal\":\"拆解\"}]", 2},
		{"single object no brackets", `{"time":"12:39-13:15","goal":"避一避"}`, 1},
		{"normal array untouched", `["a"]`[:0] + `[{"time":"07:00-08:00","goal":"晨练"}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items, err := parseDailyPlan(tc.raw)
			if err != nil {
				t.Fatalf("parseDailyPlan(%q): %v", tc.raw, err)
			}
			if len(items) != tc.want {
				t.Fatalf("items = %d, want %d (%+v)", len(items), tc.want, items)
			}
		})
	}
	// 仍无法挽救的形态照常报错。
	if _, err := parseDailyPlan("not json at all"); err == nil {
		t.Fatalf("garbage input must still error")
	}
}

// TestStrategicReplan_BracketlessResponseEndToEnd is the regression for the
// user's report: a replan whose LLM response lacks the array brackets must
// still revise the plan.
func TestStrategicReplan_BracketlessResponseEndToEnd(t *testing.T) {
	ac, ctx := newAgentContext(context.Background())
	ac.autoPlanEnabled = true
	// 仿真实测的失败形态：无 [ 开头、尾部残留 ]。
	fake := &fakeLoopLLM{resp: makeStrategicResponse(
		`{"time":"12:31-15:00","goal":"处理故障后续"},{"time":"15:00-18:00","goal":"下午拆解"}]`)}
	ac.strategicHc = fake
	ft := &fakeTransport{connected: true}

	ac.as.SetDailyPlan("07:00-09:00: 晨练\n09:00-12:00: 上午装配\n14:00-18:00: 下午拆解", 11)
	perc := protocolPerceptionAt(t, 11, 45060) // D12 12:31
	if _, err := ac.as.SetPerception(perc); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}

	ac.maybeStrategicReplan(ctx, "H-01", ft, nil, nil, nil, testLogger(), "紧急反应跨越时段边界被切断")
	waitFor(t, 3*time.Second, func() bool {
		return len(ac.as.PlanRevisions()) == 1 && replanIdle(ac)
	})
	plan, _, idx := ac.as.SnapshotSchedule()
	items := parseFormattedPlan(plan)
	if len(items) != 4 {
		t.Fatalf("revised plan should have 4 slots (2 past + 2 new), got %d:\n%s", len(items), plan)
	}
	if items[2].Goal != "处理故障后续" || idx != 2 {
		t.Fatalf("bracket-less response must still revise the plan, got %q idx=%d", items[2].Goal, idx)
	}
}
