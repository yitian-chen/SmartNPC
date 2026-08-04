package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
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
		timeOfDay      string // perception_update.environment 时间，"HH:MM" 格式（测试内转 time_of_day_sec）
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
			// 按约定 19，environment 用 time_of_day_sec（当天秒数）。
			parts := strings.Split(c.timeOfDay, ":")
			if len(parts) != 2 {
				t.Fatalf("invalid timeOfDay %q", c.timeOfDay)
			}
			h, _ := strconv.Atoi(parts[0])
			m, _ := strconv.Atoi(parts[1])
			todSec := h*3600 + m*60
			ac.latestPerception = []byte(fmt.Sprintf(`{"environment":{"time_of_day_sec":%d},"location":{}}`, todSec))
			ac.mu.Unlock()

			r := &reactiveRunner{}
			input := r.buildInput("H-01", ac, TriggerZoneChange, "test")
			if input.CurrentSlot != c.wantSlot {
				t.Errorf("CurrentSlot = %q, want %q", input.CurrentSlot, c.wantSlot)
			}
		})
	}
}

// TestUpgradeIfPhysicalAlert_ReasonContainsOrigReaction 验证升级后的 reason
// 字符串包含原始 Reaction（而非误显示为 replan）。这是 P2 修复的核心：
// 修复前 reason 会显示"原决策=replan"，修复后正确显示"原决策=observe/continue"。
func TestUpgradeIfPhysicalAlert_ReasonContainsOrigReaction(t *testing.T) {
	cases := []struct {
		name         string
		input        ReactiveInput
		dec          ReactiveDecision
		wantOrigKind string
	}{
		{
			name:         "observe upgraded, reason should contain observe",
			input:        ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:          ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantOrigKind: "observe",
		},
		{
			name:         "continue upgraded, reason should contain continue",
			input:        ReactiveInput{Fatigue: 70, Energy: 50, Health: 100},
			dec:          ReactiveDecision{Reaction: ReactionContinue, Reason: "继续行动"},
			wantOrigKind: "continue",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := upgradeIfPhysicalAlert(c.input, c.dec)
			wantSub := "原决策=" + c.wantOrigKind + "/"
			if !strings.Contains(got.Reason, wantSub) {
				t.Errorf("Reason = %q, want substring %q", got.Reason, wantSub)
			}
			// 升级后应为 replan
			if got.Reaction != ReactionReplan {
				t.Errorf("Reaction = %q, want replan", got.Reaction)
			}
		})
	}
}

// TestFallbackStopAndRefill_ClearsQueueAndSignalsWorker 验证 replan 规划失败后
// fallbackStopAndRefill 清空队列 + 清在途追踪 + signal worker，让 worker 醒来
// 后通过自然 tacticalRefill 路径重新规划。这是"修复 replan 失败导致战术层
// 重规划延迟"的核心：不再让 agent 继续执行触发 replan 的坏 action。
func TestFallbackStopAndRefill_ClearsQueueAndSignalsWorker(t *testing.T) {
	ws := newTestWS()
	r := &reactiveRunner{ws: ws, logger: testLogger()}
	ac, _ := newAgentContext(context.Background())

	ac.mu.Lock()
	ac.currentActionID = "act_inflight_456"
	ac.currentActionCmd = "WorkAtWorkbench"
	ac.currentActionParams = map[string]any{"target_object_id": "workbench_01"}
	ac.actionQueue = []plannedAction{
		{Action: "work_at_workbench", Params: map[string]any{"target_object_id": "workbench_01"}},
		{Action: "wait", Params: map[string]any{"duration_sec": 600}},
	}
	ac.mu.Unlock()

	r.fallbackStopAndRefill("H-01", ac, "疲劳告警 replan 失败")

	ac.mu.Lock()
	defer ac.mu.Unlock()
	if len(ac.actionQueue) != 0 {
		t.Errorf("actionQueue 应被清空，仍剩 %d 个 action", len(ac.actionQueue))
	}
	if ac.currentActionID != "" {
		t.Errorf("currentActionID 应被清空，得到 %q", ac.currentActionID)
	}
	if ac.currentActionCmd != "" {
		t.Errorf("currentActionCmd 应被清空，得到 %q", ac.currentActionCmd)
	}
	if ac.replanHint != "疲劳告警 replan 失败" {
		t.Errorf("replanHint = %q, want replan 原因", ac.replanHint)
	}
	if ac.replanInProgress {
		t.Errorf("replanInProgress 应被清除，让 worker 能 refill")
	}

	// signal 后 worker 应被唤醒
	select {
	case <-ac.wake:
		// ok：worker 被唤醒
	default:
		t.Error("expected worker wake signal after fallbackStopAndRefill")
	}
}

// TestFallbackStopAndRefill_NoInFlightAction 验证无在途 action 时
// fallbackStopAndRefill 仍清空队列并 signal worker。
func TestFallbackStopAndRefill_NoInFlightAction(t *testing.T) {
	ws := newTestWS()
	r := &reactiveRunner{ws: ws, logger: testLogger()}
	ac, _ := newAgentContext(context.Background())

	ac.mu.Lock()
	ac.currentActionID = "" // 无在途
	ac.actionQueue = []plannedAction{
		{Action: "wait", Params: map[string]any{"duration_sec": 300}},
	}
	ac.mu.Unlock()

	r.fallbackStopAndRefill("H-01", ac, "replan 失败")

	ac.mu.Lock()
	defer ac.mu.Unlock()
	if len(ac.actionQueue) != 0 {
		t.Errorf("actionQueue 应被清空，仍剩 %d", len(ac.actionQueue))
	}
	select {
	case <-ac.wake:
		// ok
	default:
		t.Error("expected worker wake signal")
	}
}

// TestUpgradeIfPhysicalAlert 验证代码层兜底：物理状态告警时 continue/observe
// 被强制升级为 replan，replan 决策不受影响。
func TestUpgradeIfPhysicalAlert(t *testing.T) {
	cases := []struct {
		name     string
		input    ReactiveInput
		dec      ReactiveDecision
		wantKind ReactionKind
		wantSub  string // reason 应包含的子串
	}{
		{
			name:     "fatigue alert upgrades observe to replan",
			input:    ReactiveInput{Fatigue: 80, Energy: 70, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantKind: ReactionReplan,
			wantSub:  "物理状态告警自动升级",
		},
		{
			name:     "energy alert upgrades continue to replan",
			input:    ReactiveInput{Fatigue: 30, Energy: 25, Health: 100},
			dec:      ReactiveDecision{Reaction: ReactionContinue, Reason: "继续行动"},
			wantKind: ReactionReplan,
			wantSub:  "物理状态告警自动升级",
		},
		{
			name:     "health alert upgrades observe to replan",
			input:    ReactiveInput{Fatigue: 30, Energy: 70, Health: 40},
			dec:      ReactiveDecision{Reaction: ReactionObserve, Reason: "状态尚可"},
			wantKind: ReactionReplan,
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
		})
	}
}

