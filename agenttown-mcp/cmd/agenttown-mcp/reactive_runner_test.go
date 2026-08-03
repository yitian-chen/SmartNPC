package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// testLogger 返回一个丢弃输出的 slog.Logger，供测试中 reactiveRunner 使用。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestWS 构造一个未连接的 ws.Server，供 reactiveRunner.execute 测试使用。
// SendStopAction 会返回 error（未连接），但测试只验证 actionQueue 清空逻辑，
// 不依赖 ws 实际发送成功。
func newTestWS() *wsserver.Server {
	return wsserver.New(wsserver.Options{})
}

// TestBuildInput_LiveSlot 验证 buildInput 实时从 dailyPlan + timeOfDay 计算 slot，
// 而非读取可能 stale 的 ac.currentSlot。覆盖三种场景：
//   - currentSlot stale 但 dailyPlan 有覆盖当前时间的 slot → 实时计算
//   - __debug__ 前缀的 currentSlot（debug 注入） → 保留原值
//   - dailyPlan 为空 → fallback 到 currentSlot
func TestBuildInput_LiveSlot(t *testing.T) {
	cases := []struct {
		name           string
		currentSlot    string
		dailyPlan      string
		timeOfDay      string // perception_update.environment.time_of_day
		wantSlot       string
	}{
		{
			name:        "stale currentSlot refreshed by dailyPlan",
			currentSlot: "07:00-09:00", // stale: 已过 09:00
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检\n09:00-12:00: 装配作业",
			timeOfDay:   "10:30",
			wantSlot:    "09:00-12:00",
		},
		{
			name:        "debug slot preserved",
			currentSlot: "__debug__07:00-09:00",
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检\n09:00-12:00: 装配作业",
			timeOfDay:   "10:30",
			wantSlot:    "__debug__07:00-09:00",
		},
		{
			name:        "empty dailyPlan falls back to currentSlot",
			currentSlot: "07:00-09:00",
			dailyPlan:   "",
			timeOfDay:   "10:30",
			wantSlot:    "07:00-09:00",
		},
		{
			name:        "time outside all slots falls back to currentSlot",
			currentSlot: "07:00-09:00",
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检",
			timeOfDay:   "23:30",
			wantSlot:    "07:00-09:00",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac, _ := newAgentContext(context.Background())
			ac.mu.Lock()
			ac.currentSlot = c.currentSlot
			ac.dailyPlan = c.dailyPlan
			// 注入 perception 让 latestTimeOfDayLocked 返回 c.timeOfDay
			ac.latestPerception = []byte(`{"environment":{"time_of_day":"` + c.timeOfDay + `"},"location":{}}`)
			ac.mu.Unlock()

			r := &reactiveRunner{}
			input := r.buildInput("H-01", ac, TriggerZoneChange, "test")
			if input.CurrentSlot != c.wantSlot {
				t.Errorf("CurrentSlot = %q, want %q", input.CurrentSlot, c.wantSlot)
			}
		})
	}
}

// TestExecute_InterruptClearsQueue 验证 ReactionInterrupt 清空 actionQueue
// 并设置 replanHint，防止 worker 在 stop_action 后立即 pop 下一个排队 action。
// 这是 Fix B 的核心：interrupt 必须真正让 agent 停下来，而不是只 stop 当前
// action 后让 worker 继续 pop 队列里剩余的消耗性动作。
func TestExecute_InterruptClearsQueue(t *testing.T) {
	ws := newTestWS()
	r := &reactiveRunner{ws: ws, logger: testLogger()}
	ac, _ := newAgentContext(context.Background())

	ac.mu.Lock()
	ac.currentActionID = "act_inflight_123"
	ac.actionQueue = []plannedAction{
		{Action: "work_at_workbench", Params: map[string]any{"target_object_id": "workbench_01"}},
		{Action: "wait", Params: map[string]any{"duration_sec": 600}},
	}
	ac.mu.Unlock()

	r.execute("H-01", ac, ReactiveDecision{
		Reaction: ReactionInterrupt,
		Reason:   "疲劳值超过60，需要休息",
	})

	ac.mu.Lock()
	defer ac.mu.Unlock()
	if len(ac.actionQueue) != 0 {
		t.Errorf("actionQueue 应被清空，仍剩 %d 个 action", len(ac.actionQueue))
	}
	if ac.replanHint != "疲劳值超过60，需要休息" {
		t.Errorf("replanHint = %q, want 包含 interrupt 原因", ac.replanHint)
	}
}

// TestExecute_InterruptNoInFlightActionSignalsWorker 验证 interrupt 时即使
// 无在途 action 也 signal worker，让 worker 看到 queue 空 + replanHint 后 refill。
func TestExecute_InterruptNoInFlightActionSignalsWorker(t *testing.T) {
	ws := newTestWS()
	r := &reactiveRunner{ws: ws, logger: testLogger()}
	ac, _ := newAgentContext(context.Background())

	ac.mu.Lock()
	ac.currentActionID = "" // 无在途
	ac.actionQueue = []plannedAction{
		{Action: "wait", Params: map[string]any{"duration_sec": 300}},
	}
	ac.mu.Unlock()

	r.execute("H-01", ac, ReactiveDecision{
		Reaction: ReactionInterrupt,
		Reason:   "疲劳告警",
	})

	ac.mu.Lock()
	defer ac.mu.Unlock()
	if len(ac.actionQueue) != 0 {
		t.Errorf("actionQueue 应被清空，仍剩 %d", len(ac.actionQueue))
	}
	// signal 后 worker 应被唤醒（wake channel 应有信号或已被消费）
	select {
	case <-ac.wake:
		// ok：worker 被唤醒
	default:
		t.Error("expected worker wake signal after interrupt with no in-flight action")
	}
}

// TestUpgradeIfPhysicalAlert 验证代码层兜底：物理状态告警时 continue/observe
// 被强制升级为 interrupt，其他决策不受影响。
func TestUpgradeIfPhysicalAlert(t *testing.T) {
	cases := []struct {
		name     string
		input    ReactiveInput
		dec      ReactiveDecision
		wantKind ReactionKind
		wantSub  string // reason 应包含的子串
	}{
		{
			name:     "fatigue alert upgrades observe to interrupt",
			input:    ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantKind: ReactionInterrupt,
			wantSub:  "物理状态告警自动升级",
		},
		{
			name:     "energy alert upgrades continue to interrupt",
			input:    ReactiveInput{Fatigue: 30, Energy: 25, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionContinue, Reason: "继续行动"},
			wantKind: ReactionInterrupt,
			wantSub:  "物理状态告警自动升级",
		},
		{
			name:     "health alert upgrades observe to interrupt",
			input:    ReactiveInput{Fatigue: 30, Energy: 70, Health: 40},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantKind: ReactionInterrupt,
			wantSub:  "物理状态告警自动升级",
		},
		{
			name:     "no alert preserves observe",
			input:    ReactiveInput{Fatigue: 50, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantKind: ReactionObserve,
		},
		{
			name:     "no alert preserves continue",
			input:    ReactiveInput{Fatigue: 50, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionContinue, Reason: "继续"},
			wantKind: ReactionContinue,
		},
		{
			name:     "interrupt preserved (no upgrade needed)",
			input:    ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionInterrupt, Reason: "疲劳高"},
			wantKind: ReactionInterrupt,
			wantSub:  "疲劳高",
		},
		{
			name:     "act preserved even under alert",
			input:    ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionAct, Reason: "紧急行动", Action: &ReactionAction{Cmd: "wait"}},
			wantKind: ReactionAct,
		},
		{
			name:     "replan preserved even under alert",
			input:    ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionReplan, Reason: "整体重新规划"},
			wantKind: ReactionReplan,
		},
		{
			name:     "boundary: fatigue exactly at threshold not upgraded",
			input:    ReactiveInput{Fatigue: 60, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "ok"},
			wantKind: ReactionObserve,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := upgradeIfPhysicalAlert(c.input, c.dec)
			if got.Reaction != c.wantKind {
				t.Errorf("Reaction = %q, want %q", got.Reaction, c.wantKind)
			}
			if c.wantSub != "" && !strings.Contains(got.Reason, c.wantSub) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, c.wantSub)
			}
			// 升级后 Action 应被清空
			if c.wantKind == ReactionInterrupt && got.Action != nil {
				t.Errorf("升级为 interrupt 后 Action 应为 nil，得到 %v", got.Action)
			}
		})
	}
}

