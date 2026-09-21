package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
)

// mustMarshalBarPerception 构造带完整 Environment 的 perception JSON
// （DayCount/todSec/gameSec 由用例指定），供状态栏测试。
func mustMarshalBarPerception(t *testing.T, dayCount int, todSec, gameSec float64) json.RawMessage {
	t.Helper()
	zone := "main_workshop"
	p := protocol.PerceptionPayload{
		Location:    protocol.Location{CurrentZone: &zone},
		Environment: protocol.Environment{GameTimeSec: gameSec, TimeOfDaySec: todSec, DayCount: dayCount, TimeScale: 90},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal perception: %v", err)
	}
	return b
}

// barTestPlan 是 6 时段的测试日计划；第 3 条（idx=2）为当前时段。
const barTestPlan = "07:00-09:00: 晨练拉伸\n" +
	"09:00-12:00: 车间装配作业\n" +
	"12:00-14:00: 午间休整\n" +
	"14:00-18:00: 车间装配作业\n" +
	"18:00-22:00: 广场休息\n" +
	"22:00-06:00: 夜间休眠"

// newBarAgentState 构造一份有感知（D12 10:47:03）+ 物理 + 日计划
// （当前时段 09:00-12:00 idx=2）的 AgentState。
func newBarAgentState(t *testing.T) *agentstate.AgentState {
	t.Helper()
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	tod := 10*3600 + 47*60 + 3
	if _, err := as.SetPerception(mustMarshalBarPerception(t, 11, float64(tod), 11*86400+float64(tod))); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	as.SetPhysicalState(&protocol.PhysicalState{Energy: 62, Fatigue: 31, JointWear: 78, Money: 340}, nil)
	as.SetDailyPlan(barTestPlan, 11)
	as.CommitTacticalRefill("09:00-12:00", 1, false)
	return as
}

// lastUserPromptContent 返回 messages 中最后一条非 <agent_state> 状态栏的
// user 消息内容（测试辅助：请求末尾的状态栏是瞬态注入，断言 user prompt
// 时应跳过）。找不到返回空串。
func lastUserPromptContent(messages []llmtypes.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !strings.HasPrefix(messages[i].Content, agentStateBarOpen) {
			return messages[i].Content
		}
	}
	return ""
}

// TestBuildAgentStateBar_NoPerception 验证无感知数据（UE 未推送首条
// perception）时返回空串，agenticTurn 跳过注入。
func TestBuildAgentStateBar_NoPerception(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	ac := &agentContext{as: as}
	if got := ac.buildAgentStateBar("H-01", nil); got != "" {
		t.Errorf("no perception should produce empty bar, got:\n%s", got)
	}
}

// TestBuildAgentStateBar_FullState 验证四行内容齐全：游戏时间（D+DayCount+1、
// 秒级）、物理状态（分档自然语言而非数值）、当前日程（[序号/总数]+goal+
// 剩余分钟）、当前动作（工具名+关键参数+time_to_stop 剩余与预计结束时刻）。
func TestBuildAgentStateBar_FullState(t *testing.T) {
	as := newBarAgentState(t)
	// 在途动作：InteractSmartObject(workbench/assemble)，time_to_stop 1 小时
	// （10:47:03 + 3600s → 预计 11:47 结束）。
	as.RecordActionStarted("act-1", protocol.CmdInteractSmartObject,
		map[string]any{"semantic_group": "workbench", "interaction": "assemble"},
		agentstate.SourceTactical, "")
	as.ArmTimeStop("act-1", 11*86400+10*3600+47*60+3+3600, 3600)
	ac := &agentContext{as: as}

	got := ac.buildAgentStateBar("H-01", nil)
	if !strings.HasPrefix(got, agentStateBarOpen+"\n") {
		t.Errorf("bar should open with %s tag, got:\n%s", agentStateBarOpen, got)
	}
	if !strings.HasSuffix(got, agentStateBarClose) {
		t.Errorf("bar should close with %s tag, got:\n%s", agentStateBarClose, got)
	}
	for _, want := range []string{
		"当前游戏时间：D12 10:47:03",
		"物理状态：电量：中等、疲劳度：精神饱满、关节磨损度：严重磨损、余额：340",
		"当前日程：[2/6] 09:00-12:00 车间装配作业（剩余约 1 小时 13 分钟）",
		"当前动作：InteractSmartObject(workbench/assemble)（剩余约 1 小时，预计 11:47 结束）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bar missing %q, got:\n%s", want, got)
		}
	}
}

// TestBuildAgentStateBar_PhysicalUsesBandsNotNumbers 验证物理状态用分档
// 自然语言而非纯数字（62/31/78/340 不得裸出现在物理行）。
func TestBuildAgentStateBar_PhysicalUsesBandsNotNumbers(t *testing.T) {
	as := newBarAgentState(t)
	ac := &agentContext{as: as}
	got := ac.buildAgentStateBar("H-01", nil)
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "物理状态：") {
			for _, banned := range []string{"62", "31", "78"} {
				if strings.Contains(line, banned) {
					t.Errorf("physical line should use band labels, not raw values %q:\n%s", banned, line)
				}
			}
			return
		}
	}
	t.Errorf("physical line missing from bar:\n%s", got)
}

// TestBuildAgentStateBar_CrossMidnightSlot 验证跨午夜时段的剩余时间
// （22:00-06:00，23:10 时剩余 6 小时 50 分钟）。
func TestBuildAgentStateBar_CrossMidnightSlot(t *testing.T) {
	as := newBarAgentState(t)
	// 改推感知到 23:10 并切换当前时段为夜间睡眠（idx=5）。
	tod := 23*3600 + 10*60
	if _, err := as.SetPerception(mustMarshalBarPerception(t, 11, float64(tod), 11*86400+float64(tod))); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	as.CommitTacticalRefill("22:00-06:00", 5, false)
	ac := &agentContext{as: as}

	got := ac.buildAgentStateBar("H-01", nil)
	if want := "当前日程：[6/6] 22:00-06:00 夜间休眠（剩余约 6 小时 50 分钟）"; !strings.Contains(got, want) {
		t.Errorf("cross-midnight remaining wrong, want %q, got:\n%s", want, got)
	}
}

// TestBuildAgentStateBar_IdleAndNoPlanDegradation 验证降级路径：无计划 →
// 日程行明示"无"；无在途动作 → 动作行明示"空闲"。空态要有明确文字而非缺行。
func TestBuildAgentStateBar_IdleAndNoPlanDegradation(t *testing.T) {
	fresh := agentstate.New()
	fresh.SetIdentity("H-01", nil)
	if _, err := fresh.SetPerception(mustMarshalBarPerception(t, 0, 7*3600+5*60, 7*3600+5*60)); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	ac := &agentContext{as: fresh}

	got := ac.buildAgentStateBar("H-01", nil)
	if want := "当前日程：无（尚未生成当日计划）"; !strings.Contains(got, want) {
		t.Errorf("missing no-plan line, want %q, got:\n%s", want, got)
	}
	if want := "当前动作：无（空闲）"; !strings.Contains(got, want) {
		t.Errorf("missing idle line, want %q, got:\n%s", want, got)
	}
	if want := "当前游戏时间：D1 07:05:00"; !strings.Contains(got, want) {
		t.Errorf("day-0 perception should render D1, want %q, got:\n%s", want, got)
	}
}

// TestBuildAgentStateBar_ActionWithoutTimeStop 验证未武装 time_to_stop 的
// 在途动作不展示剩余时间（长复合动作持续到时段切换，无从推算）。
func TestBuildAgentStateBar_ActionWithoutTimeStop(t *testing.T) {
	as := newBarAgentState(t)
	as.RecordActionStarted("act-2", protocol.CmdGenericAct,
		map[string]any{"behavior": "look_around"}, agentstate.SourceTactical, "")
	ac := &agentContext{as: as}

	got := ac.buildAgentStateBar("H-01", nil)
	if want := "当前动作：generic_act(look_around)"; !strings.Contains(got, want) {
		t.Errorf("in-flight action line wrong, want %q, got:\n%s", want, got)
	}
	if strings.Contains(got, "剩余") && strings.Contains(got, "结束）") {
		// 日程行的"剩余"是合法的；这里只检查动作行不带剩余子句。
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "当前动作：") && strings.Contains(line, "剩余") {
				t.Errorf("action without time_to_stop should not show remaining:\n%s", line)
			}
		}
	}
}

// TestAgenticTurn_AppendsStateBarNotHistory 验证 agenticTurn 集成：请求
// messages 末尾追加 <agent_state> user 消息，成功落历史的是 userContent
// 而非状态栏（瞬态注入不入历史）。
func TestAgenticTurn_AppendsStateBarNotHistory(t *testing.T) {
	as := newBarAgentState(t)
	fake := &fakeLoopLLM{resp: makeLoopTextResponse(`{"accept":true}`)}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"dialogue", "user内容A", "none", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}

	// 请求形态：[system, user, 状态栏]。
	if len(fake.capturedMsgs) != 3 {
		t.Fatalf("request messages len = %d, want 3 (system+user+state bar)", len(fake.capturedMsgs))
	}
	bar := fake.capturedMsgs[2]
	if bar.Role != "user" || !strings.HasPrefix(bar.Content, agentStateBarOpen) {
		t.Errorf("last message should be the state bar, got %+v", bar)
	}
	if !strings.Contains(bar.Content, "当前游戏时间：D12 10:47:03") {
		t.Errorf("state bar should carry game time, got:\n%s", bar.Content)
	}

	// 历史：user + assistant（不含状态栏）。
	hist := as.Conversation()
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (user+assistant, no state bar)", len(hist))
	}
	if hist[0].Role != "user" || hist[0].Content != "user内容A" {
		t.Errorf("hist[0] = %+v, want user/user内容A (not the state bar)", hist[0])
	}
	if strings.Contains(hist[0].Content, agentStateBarOpen) {
		t.Errorf("state bar must not be persisted into history:\n%s", hist[0].Content)
	}
}

// TestAgenticTurn_NoStateBarWithoutPerception 验证无感知时请求形态保持
// [system, user]（状态栏跳过）。
func TestAgenticTurn_NoStateBarWithoutPerception(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	fake := &fakeLoopLLM{resp: makeLoopTextResponse(`{"accept":true}`)}
	ac := &agentContext{as: as, tacticalHc: fake}

	if _, err := ac.agenticTurn(context.Background(), fake, nil, nil, nil, "H-01",
		"dialogue", "user内容A", "none", "", nil); err != nil {
		t.Fatalf("agenticTurn: %v", err)
	}
	if len(fake.capturedMsgs) != 2 {
		t.Fatalf("request messages len = %d, want 2 (no perception → no state bar)", len(fake.capturedMsgs))
	}
}
