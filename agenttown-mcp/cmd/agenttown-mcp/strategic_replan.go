package main

// Event-driven strategic-layer replan（P3-9 升格，设计文档 §5.5）。
//
// 07:00 的计划对当天发生的事件保持无知：紧急反应跨时段被切/超时切回日程
// 后，后续时段仍按旧假设继续走。本文件补上"计划是可变文档"的另一半：
// 显著偏差触发**剩余时段**的战略层重规划，修订写回 dailyPlan（内存 + Store
// 持久化，状态栏与后续决策立即一致），修订留痕供当晚反思（P5-15）。
//
// 触发场景（§5.5"早上 6 点做的规划不可能预见 6 小时后的实际状态"）：
//   - 反应跨时段边界被切断（advanceSlotIfNeeded 时反应仍在进行）
//   - 反应超截止时间被硬切回日程（checkReactionDeadline）
// 节流（防计划 churn 摧毁行为连贯性——设计的另一大目标）：
//   - 每游戏日上限 strategicReplanDailyCap 次（尝试即计数，失败也烧）
//   - 相邻两次间隔 ≥ strategicReplanMinGapGameSec 游戏秒
// 手动模式不触发（无 dailyPlan 可改，同反应层口径）。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/weeklyschedule"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

const (
	// strategicReplanDailyCap bounds replans per game day per agent.
	strategicReplanDailyCap = 2
	// strategicReplanMinGapGameSec is the minimum game-time gap between two
	// replans (2 game hours — an event storm must not become a plan storm).
	strategicReplanMinGapGameSec = 2 * 3600.0
)

// maybeStrategicReplan evaluates the trigger and spawns the replan when
// allowed. Called async from the worker loop at the two cut points.
func (a *agentContext) maybeStrategicReplan(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	weeklySched *weeklyschedule.Schedule, logger *slog.Logger, triggerReason string) {

	// 手动模式 / 无战略客户端 / 无当日计划：无事可做。
	if !a.autoPlanEnabled || a.strategicHc == nil {
		return
	}
	if plan, _, _ := a.as.SnapshotSchedule(); plan == "" {
		return
	}
	nowGameSec := a.as.LatestGameTimeSec()
	day := a.as.LatestDayCount()
	if !a.as.TryBeginStrategicReplan(nowGameSec, day, strategicReplanDailyCap, strategicReplanMinGapGameSec) {
		logger.Debug("[战略层/replan] 节流跳过（当日上限或间隔不足）",
			"agent_id", agentID, "trigger", triggerReason)
		return
	}
	logger.Info("[战略层/replan] 显著偏差触发当日计划修订",
		"agent_id", agentID, "trigger", triggerReason)
	go a.strategicReplan(ctx, agentID, ws, kb, profiles, weeklySched, logger, triggerReason)
}

// strategicReplan regenerates the REMAINDER of today's plan and writes it
// back. The replan holds the replanInProgress slot (blocks worker refill
// mid-flight) and registers its LLM call in the cancel registry (§3.3：force
// 事件可掐掉重规划本身，被掐时保留旧计划让位)。
func (a *agentContext) strategicReplan(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	weeklySched *weeklyschedule.Schedule, logger *slog.Logger, triggerReason string) {

	if !a.acquireReplanSlot(agentID, triggerReason, logger) {
		return
	}
	defer func() {
		a.coordMu.Lock()
		a.replanInProgress = false
		a.coordMu.Unlock()
	}()

	snap := a.as.Snapshot()
	tod := snap.LatestTimeOfDay()
	if tod == "" {
		logger.Warn("[战略层/replan] 无当前游戏时间，放弃修订", "agent_id", agentID)
		return
	}
	oldPlan := snap.DailyPlan

	// 注册 LLM 取消（§3.3）：registry 名为 tactical，实为"在途 LLM"通用句柄。
	ctx, deregLLM := a.registerTacticalLLMCall(ctx)
	defer deregLLM()

	// 修订上下文：原计划 + 偏差事实（P4-10 的未完成任务槽与上次结束原因）。
	hint := fmt.Sprintf("当日计划修订。修订原因：%s。\n原当日计划：\n%s\n", triggerReason, oldPlan)
	if snap.UnfinishedTask != "" {
		hint += fmt.Sprintf("当前未完成的任务：%s。\n", snap.UnfinishedTask)
	}
	if snap.LastEndResult != "" {
		end := snap.LastEndResult
		if snap.LastEndWhy != "" {
			end += "（" + snap.LastEndWhy + "）"
		}
		hint += fmt.Sprintf("上一个动作的结束方式：%s。\n", end)
	}
	hint += "请基于当前时间与实际执行状况，重新规划从当前时间起到今天结束的剩余日程：吸收上述偏差（例如压缩或顺延后续时段、补上未完成的事），已过去的时段不需要输出。输出必须是一个 JSON 数组，每个元素形为 {\"time\":\"HH:MM-HH:MM\",\"goal\":\"目标描述\"}，时段目标风格与原计划一致。"

	dayCtx := weeklyschedule.WeeklyLine(a.as.LatestDayCount(), weeklySched)
	newPlan, err := a.generateDailyPlanCore(ctx, agentID, kb, profiles, logger,
		"（本次为当日计划修订，无需注入昨日总结）", dayCtx, hint, tod)
	if err != nil {
		if cancelledByForce(err, ctx) {
			logger.Info("[战略层/replan] 修订被更新的 force 事件取消，保留旧计划", "agent_id", agentID)
			return
		}
		logger.Warn("[战略层/replan] 修订失败，保留旧计划", "agent_id", agentID, "err", err)
		return
	}

	// 写回：已过去的时段保留为既成事实，剩余时段替换为新计划。
	merged := mergeRemainder(oldPlan, newPlan, tod)
	a.as.SetDailyPlan(merged, a.as.CurrentDay())
	// currentPlanIndex 指向首个新时段（= 保留的旧时段数），后续 selectCurrentGoal
	// 从新时段起步。SetDailyPlan 已持久化（write-through）。
	a.as.SetCurrentPlanIndex(countPastSlots(oldPlan, tod))
	a.as.RecordPlanRevision(fmt.Sprintf("%s 修订（%s）：%s", tod, triggerReason,
		strings.ReplaceAll(mergeRemainderDigest(newPlan), "\n", "；")))

	logger.Info("[战略层/replan] 当日计划已修订",
		"agent_id", agentID, "trigger", triggerReason, "tod", tod,
		"new_remainder", newPlan, "merged", merged)
	// 唤醒 worker：按新计划 refill 当前时段。
	a.signal()
}

// mergeRemainder merges the old plan's fully-past slots (既成事实，含跨午夜
// 归一) with the new plan's slots. Slots still running or not yet started are
// replaced by the revision.
func mergeRemainder(oldPlan, newPlan, timeOfDay string) string {
	past := pastSlots(oldPlan, timeOfDay)
	if len(past) == 0 {
		return newPlan
	}
	lines := make([]string, 0, len(past)+4)
	for _, it := range past {
		lines = append(lines, it.Time+": "+it.Goal)
	}
	return strings.Join(lines, "\n") + "\n" + newPlan
}

// pastSlots returns the old-plan items whose slot already ended by
// timeOfDay (cross-midnight slots normalized via prompt.SlotRangeMinute).
func pastSlots(oldPlan, timeOfDay string) []dailyPlanItem {
	cur := prompt.ParsePlanMinute(timeOfDay)
	if cur < 0 {
		return nil
	}
	var kept []dailyPlanItem
	for _, it := range parseFormattedPlan(oldPlan) {
		start, end := prompt.SlotRangeMinute(it.Time)
		if start < 0 || end < 0 {
			continue
		}
		if end <= cur {
			kept = append(kept, it)
		}
	}
	return kept
}

// countPastSlots counts the fully-past old slots — the index of the first
// new slot in the merged plan.
func countPastSlots(oldPlan, timeOfDay string) int {
	return len(pastSlots(oldPlan, timeOfDay))
}

// mergeRemainderDigest renders a one-line digest of the new remainder for
// the revision note.
func mergeRemainderDigest(newPlan string) string {
	lines := strings.Split(strings.TrimSpace(newPlan), "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return strings.Join(lines, "；") + "…"
}
