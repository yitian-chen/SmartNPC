package main

// world_event inbound dispatch + the force hard-guarantee channel
// (事件驱动设计 §4，docs/AgentTown_WorldEvent_Protocol.md §四).
//
// handleWorldEvent is wired from Runtime.HandleMessage. The force branch is
// the design doc's hard-guarantee channel: zero LLM, zero debounce, cannot
// be vetoed — the stop AND the in-flight tactical LLM cancellation (§3.3
// 唯一盲区) fire synchronously in the WS receive path before any goroutine
// or model call, so nothing stands between the message and the
// interruption. The replan that follows is tactical work, not part of the
// guarantee, and runs async. Non-force events enqueue for the next safe
// point (§4.4); the lightweight router (P2) will sit between receipt and
// enqueue once it lands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract"
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// forceReplanPollInterval is how often acquireReplanSlot re-checks the
// replan slot while another replan holds it. The holder's in-flight LLM call
// is already cancelled by handleForceEvent, so it should release within one
// HTTP abort (~ms); a fine poll keeps the takeover snappy.
const forceReplanPollInterval = 20 * time.Millisecond

// forceReplanWaitLimit bounds the wait for an in-flight replan to release
// the slot after its LLM call was cancelled. Cancellation makes the holder
// fail fast (venus sendMu frees on HTTP abort), so a few seconds is a
// generous backstop for a holder stuck outside the LLM call itself.
var forceReplanWaitLimit = 5 * time.Second

// registerTacticalLLMCall wraps parent in a cancellable context and
// registers the cancel so a force event can abort this call (§3.3 唯一盲区).
// The returned deregister must be deferred by the caller; it is a no-op if
// a newer call has since registered (generation-guarded), so a stale call
// can never clear a live registration.
func (a *agentContext) registerTacticalLLMCall(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	a.coordMu.Lock()
	a.tacticalLLMGen++
	gen := a.tacticalLLMGen
	a.tacticalLLMCancel = cancel
	a.coordMu.Unlock()
	dereg := func() {
		cancel() // release ctx resources regardless
		a.coordMu.Lock()
		if a.tacticalLLMGen == gen {
			a.tacticalLLMCancel = nil
		}
		a.coordMu.Unlock()
	}
	return ctx, dereg
}

// cancelInFlightTacticalLLM aborts the in-flight tactical-layer LLM call,
// if any. Returns whether a call was actually cancelled. Called from the
// force path's synchronous segment — a memory operation, microsecond-level,
// part of the §4.2 guarantee extension (no uninterruptible window).
func (a *agentContext) cancelInFlightTacticalLLM() bool {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if a.tacticalLLMCancel == nil {
		return false
	}
	a.tacticalLLMCancel()
	return true
}

// cancelledByForce reports whether an LLM error is the result of a force
// interrupt cancelling the call (as opposed to a parent shutdown or a
// timeout): the error is context.Canceled while the parent context is still
// alive. Callers use this to skip their failure fallbacks — the force replan
// owns the world now, and a fallback (idle-speak / hint overwrite / queue
// drop) would fight it.
func cancelledByForce(err error, parent context.Context) bool {
	return err != nil && errors.Is(err, context.Canceled) && parent.Err() == nil
}

// handleWorldEvent dispatches one inbound world_event message. It reports
// whether the event was accepted into the event system (force path entered
// or enqueued); false means dropped by policy (unregistered agent / manual
// mode / agent offline) — the reason is in the log.
func (rt *Runtime) handleWorldEvent(agentID string, ev protocol.WorldEventPayload) bool {
	ac := rt.lookupAgent(agentID)
	if ac == nil {
		rt.logger.Warn("world_event dropped for unregistered agent",
			"agent_id", agentID, "event_id", ev.EventID, "category", ev.Category)
		return false
	}
	if !rt.autoPlanEnabled {
		// 手动模式与反应层触发同口径：MCP 不做任何自动决策。
		rt.logger.Debug("world_event dropped (manual mode)",
			"agent_id", agentID, "event_id", ev.EventID)
		return false
	}
	ac.coordMu.Lock()
	stopped := ac.stopped
	ac.coordMu.Unlock()
	if stopped {
		rt.logger.Debug("world_event dropped (agent offline)",
			"agent_id", agentID, "event_id", ev.EventID)
		return false
	}

	// P4-14：对话邀请即时消费（不走路由器/队列）——对话建立有实时性
	// 要求（UE 会话状态机等待 rsp），入队等安全点会把对话拖到几十游戏
	// 分钟后。协议规定 UE 停发独立 chat_invite、统一按 world_event 推送
	// social.chat_invite_incoming；Agent 侧在此解包转交对话 runner。
	if ev.Category == protocol.CategorySocial && ev.EventType == protocol.EventTypeChatInviteIncoming {
		// 战斗让位中礼貌拒绝（不转交 runner——战斗中的身体不会去聊天，
		// 但 rsp 必须回：UE 会话状态机在等，静默丢弃会把对端 A 挂死）。
		if ac.combatYieldActive() {
			var d protocol.SocialData
			_ = json.Unmarshal(ev.Data, &d)
			if ac.dialogue != nil && d.ConvID != "" {
				ac.dialogue.sendInviteRsp(d.ConvID, false)
			}
			rt.logger.Info("[world_event] 战斗让位中，对话邀请已礼貌拒绝",
				"agent_id", agentID, "event_id", ev.EventID, "from", d.From, "conv_id", d.ConvID)
			return true
		}
		if ac.dialogue != nil {
			var d protocol.SocialData
			_ = json.Unmarshal(ev.Data, &d)
			go ac.dialogue.handleInvite(rt.ctx, protocol.ChatInvitePayload{
				ConvID:      d.ConvID,
				FromAgentID: d.From,
				Content:     d.Content,
			})
			rt.logger.Info("[world_event] 对话邀请已转交对话 runner（即时）",
				"agent_id", agentID, "event_id", ev.EventID, "from", d.From, "conv_id", d.ConvID)
		} else {
			rt.logger.Debug("[world_event] chat_invite dropped (dialogue disabled)", "agent_id", agentID, "event_id", ev.EventID)
		}
		return true
	}

	// 修复 A：combat 类事件是持续状态的边沿——attacked/targeted 登记威胁
	// 情境（直到 combat_exit 或 TTL），combat_exit 解除。放在 force 分流
	// 之前：登记/解除对 force 与非 force 路径一视同仁。
	if prompt.IsCombatStartEvent(ev) {
		ac.as.BeginSituation(situationKindCombat, prompt.FormatWorldEvent(ev), ac.as.LatestGameTimeSec())
		rt.logger.Info("[world_event] 登记持续威胁情境（combat_exit 或 TTL 解除）",
			"agent_id", agentID, "event_id", ev.EventID, "event_type", ev.EventType)
	} else if prompt.IsCombatExitEvent(ev) {
		if ac.as.EndSituation(situationKindCombat) {
			rt.logger.Info("[world_event] 威胁情境已解除（combat_exit）",
				"agent_id", agentID, "event_id", ev.EventID)
		}
	}

	// Combat-detach 归还：让位中的 combat_exit 是确定性交接的完成信号，
	// 不走路由器——让位期间 agent 无在途动作、无反应窗口，路由判决必然
	// 缺上下文（大概率误判 no_interrupt，控制权就永远收不回；detach 让位
	// 本就不经裁决，归还对称地确定性接收）。放在 force 分流之前：force
	// 与非 force 两种 combat_exit 都接得住。
	if prompt.IsCombatExitEvent(ev) && ac.combatYieldActive() {
		ac.reclaimFromCombatYield(rt.ctx, agentID, rt.ws, *rt.kbPtr, rt.profiles, ev, false, rt.logger)
		return true
	}

	if ev.Force {
		rt.handleForceEvent(ac, agentID, ev)
		return true
	}

	switch res := ac.as.EnqueueWorldEvent(ev); res {
	case agentstate.EnqueueOK:
		rt.logger.Info("[world_event] 已入队，等待安全点 drain",
			"agent_id", agentID, "event_id", ev.EventID,
			"category", ev.Category, "event_type", ev.EventType,
			"severity", ev.Severity, "queue_len", ac.as.WorldEventQueueLen())
		// 路由器裁决（§4.3）：非 force 事件入队后异步判一次紧急度——
		// interrupt 则撤队转打断（routerInterrupt），不紧急/判不动则
		// 留在队列等安全点。入队先行保证任何失败路径下事件都不丢。
		if rt.eventRouter != nil {
			go rt.eventRouter.route(rt.ctx, agentID, ev)
		}
	case agentstate.EnqueueDuplicate:
		rt.logger.Debug("[world_event] 重复事件已丢弃（seq 重放）",
			"agent_id", agentID, "event_id", ev.EventID)
	case agentstate.EnqueueEvictedOldest:
		rt.logger.Warn("[world_event] 队列已满，丢弃最旧事件后入队",
			"agent_id", agentID, "event_id", ev.EventID,
			"category", ev.Category, "severity", ev.Severity,
			"queue_len", ac.as.WorldEventQueueLen())
	}
	return true
}

// handleForceEvent executes the §4.2 hard-guarantee channel. It must be
// called synchronously from the WS receive path: the stop below is the
// microsecond-level guarantee, so nothing asynchronous may precede it.
// Cancelling the in-flight tactical LLM call (§3.3) is part of the same
// synchronous segment — the "blind window" is closed at receive time.
func (rt *Runtime) handleForceEvent(ac *agentContext, agentID string, ev protocol.WorldEventPayload) {
	// Dedup is replay protection, not vetoing: an event_id already seen is
	// a seq-replay redelivery of an interruption that already fired.
	if !ac.as.MarkWorldEventSeen(ev.EventID) {
		rt.logger.Info("[world_event/force] 重复事件已丢弃（seq 重放）",
			"agent_id", agentID, "event_id", ev.EventID)
		return
	}

	// Combat-detach（UE 战斗 AI 接管）：detach=true 时下方全部动作（hint +
	// 反应窗口 + 重规划 + 新动作下发）都会与 UE 战斗系统抢身体——这正是
	// 2026-09-22 实测的 bug（MCP 1 秒内重规划下发逃跑，UE 战斗 AI 从未拿到
	// 控制权）。让位分支代替之：静默交出身体。combat_exit 误带 detach 时不
	// 进入（那是归还信号，不是接管信号）。
	if ev.Detach && !prompt.IsCombatExitEvent(ev) {
		rt.handleCombatDetach(ac, agentID, ev)
		return
	}
	// 让位期间的普通 force 事件同样不得 stop/重规划（身体在 UE 手里直到
	// combat_exit）：echo 回事件队列（上面的 MarkSeen 已烧掉 event_id 的
	// 正常入队通道，echo 特意绕过 dedup），归还后的第一次分解在
	// 【发生的事件】里见到它。
	if ac.combatYieldActive() {
		if ev.EventID != "" || ev.EventType != "" {
			ac.as.EchoWorldEvent(ev)
		}
		rt.logger.Info("[world_event/force] 战斗让位中，force 事件入队等待归还后处理（不打断不重规划）",
			"agent_id", agentID, "event_id", ev.EventID, "event_type", ev.EventType,
			"severity", ev.Severity)
		return
	}

	// 掐掉正在飞的战术层 LLM 请求（§3.3 唯一盲区）：半截思考直接丢弃
	// （agenticTurn 成功才落历史，取消天然零残留），被取消方经
	// cancelledByForce 判定后不做失败兜底，本次 force replan 随后接管。
	if ac.cancelInFlightTacticalLLM() {
		rt.logger.Info("[world_event/force] 已取消在途战术层 LLM 请求（半截思考丢弃，重规划即将接管）",
			"agent_id", agentID, "event_id", ev.EventID)
	}

	// 立即打断在途动作（执行中或 SmartObject 排队中均覆盖——stop 会把
	// 排队方一并移出队列，UE 回 action_completed{interrupted}）。
	actionID := ac.as.CurrentActionID()
	if actionID != "" {
		if err := rt.ws.SendStopAction(agentID, actionID); err != nil {
			// 发送失败保留在途追踪：后续 replan 完成后还会重试 stop。
			rt.logger.Warn("[world_event/force] stop_action 发送失败（打断延后到 replan 完成后重试）",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			// P4-10：先捕捉未完成任务槽（含已执行时长），再清在途追踪。
			ac.recordInterrupted("被紧急事件强制打断：" + truncateRunes(prompt.FormatWorldEvent(ev), 60))
			// 清在途追踪，交给延迟到达的 action_completed{interrupted}：
			// stash 保住 action_history 记账（与 checkTimeToStop 同模式）。
			ac.as.ClearInFlightKeepQueue()
			rt.logger.Warn("[world_event/force] 已强制打断在途动作",
				"agent_id", agentID, "action_id", actionID,
				"event_id", ev.EventID, "category", ev.Category,
				"event_type", ev.EventType, "severity", ev.Severity,
				"subject", ev.Subject)
		}
	} else {
		rt.logger.Warn("[world_event/force] 强制事件到达（无在途动作，直接重规划）",
			"agent_id", agentID, "event_id", ev.EventID, "category", ev.Category,
			"event_type", ev.EventType, "severity", ev.Severity, "subject", ev.Subject)
	}

	hint := forceEventHint(ev)
	// hint 落 AgentState：等待超时 / replan 失败两条兜底路径都靠 worker
	// 自然 refill 时经 BeginTacticalRefill 读到事件上下文。
	ac.as.SetReplanHint(hint)
	// 护栏（§4.5）：force 反应同样是一个反应任务——带截止时间，后续路由
	// 裁决需严格更高 severity 才能再打断（force 自身不受限，§4.2）。
	ac.beginReaction(ev.Severity, ac.as.LatestGameTimeSec(), prompt.FormatWorldEvent(ev))
	go ac.forceInterruptReplan(rt.ctx, agentID, rt.ws, *rt.kbPtr, rt.profiles, ev, hint, rt.logger)
}

// forceEventHint formats the replan hint for a force event. The 【强制打断】
// marker is the contract with prompt.tacticalHintLine: it renders the event
// as a top-priority 【紧急事件】 directive (explicitly overriding the slot
// goal and the duration-filling rules) instead of a mere interruption note.
func forceEventHint(ev protocol.WorldEventPayload) string {
	return "【强制打断】" + prompt.FormatWorldEvent(ev)
}

// handleCombatDetach executes the combat-detach handover, synchronously in
// the WS receive path (mirroring handleForceEvent). UE's combat AI is taking
// the body, so the agent stands down COMPLETELY: stop/clear the in-flight
// action as the handover assist, then yield — no replan, no new actions, no
// hint, no reaction window, until combat_exit (or the TTL safety net)
// returns control. Repeat attacks while already yielded only refresh the TTL
// anchor and the echoed event (idempotent entry).
func (rt *Runtime) handleCombatDetach(ac *agentContext, agentID string, ev protocol.WorldEventPayload) {
	alreadyYielded := ac.combatYieldActive()
	if !alreadyYielded {
		// 掐在途战术层 LLM（如此前非 detach 事件触发的逃跑重规划）：半截
		// 思考丢弃，被取消方不做失败兜底——但也不像 force 路径那样有新
		// replan 接管，让位本身即终态。
		if ac.cancelInFlightTacticalLLM() {
			rt.logger.Info("[combat-detach] 已取消在途战术层 LLM 请求（让位，不重规划）",
				"agent_id", agentID, "event_id", ev.EventID)
		}
		// 交接辅助：stop 在途动作，给 UE 战斗 AI 一个空闲身体。实测 UE
		// 不会预停我们的动作（2026-09-22：MCP 的 stop 才是打断 Exercise
		// 的那个）——所以这一步仍属必要。
		if actionID := ac.as.CurrentActionID(); actionID != "" {
			if err := rt.ws.SendStopAction(agentID, actionID); err != nil {
				// 发送失败也照常让位：UE 已持有身体，残留动作由 UE 战斗
				// 系统自然覆盖，无 replan 会去重试 stop。
				rt.logger.Warn("[combat-detach] stop_action 发送失败（UE 已接管，继续让位）",
					"agent_id", agentID, "action_id", actionID, "err", err)
			} else {
				ac.recordInterrupted("战斗让位（UE 战斗系统接管）：" + truncateRunes(prompt.FormatWorldEvent(ev), 60))
			}
		}
		// 业务复位：队列 + 在途 + slot（让位时长未知；归还 replan 经
		// selectCurrentGoal 按游戏时间从 dailyPlan 重推 goal）。事件队列被
		// ClearForReplan 保留——战斗前的上下文要活到归还后的 drain。在途
		// 动作 stash 进 clearedAction，迟到的 interrupted completion 仍能
		// 记满 action_history 行（与既有 Clear 语义一致）。
		info := ac.as.ClearForReplan()
		// 取消在途动作的超时 timer：迟到回调会发游离 stop / 提前烧掉
		// clearedAction stash（镜像 abandonCurrentPlan；普通 force 路径
		// 从不取消 timer 是既有 wart，让位路径不复制它）。
		if info.ActionID != "" {
			ac.coordMu.Lock()
			if timer, ok := ac.pendingActionTimeouts[info.ActionID]; ok {
				timer.Stop()
				delete(ac.pendingActionTimeouts, info.ActionID)
			}
			ac.coordMu.Unlock()
		}
		// 此前事件 arm 的反应窗口不得在让位期间触发截止硬切（stop+hint+
		// signal 全套都会惊扰 UE 的战斗）。
		ac.clearReaction()
		// 陈旧的 slotSwitchPending 若带过让位期，归还 replan 提交新队列后
		// 会被 processSlotSwitch 整队清掉——入口处就地解除（slot 边界迁移
		// 已被让位吞并，归还后按时间重推）。
		ac.as.ClearSlotSwitchPending()
	}
	ac.beginCombatYield(ev, ac.as.LatestGameTimeSec())
	rt.logger.Warn("[combat-detach] UE 战斗系统接管，agent 让位（静默至 combat_exit 或 TTL 兜底）",
		"agent_id", agentID, "event_id", ev.EventID, "event_type", ev.EventType,
		"severity", ev.Severity, "subject", ev.Subject, "repeat_attack", alreadyYielded)
}

// reclaimFromCombatYield ends a combat-detach yield and hands control back to
// the agent. Shared by the two reclaim triggers:
//   - combat_exit handback (ttl=false, ev = the exit event; the exit event is
//     echoed by forceInterruptReplan on success, so 【发生的事件】 reads the
//     chronological pair 被攻击 → 脱战),
//   - TTL safety net (ttl=true, ev = the last detach attack — UE never sent
//     combat_exit; the hint says so).
// The post-combat replan re-derives its goal from dailyPlan by game time
// (tacticalRefillForReplan → selectCurrentGoal), so the slot cleared at yield
// entry costs nothing.
func (a *agentContext) reclaimFromCombatYield(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	ev protocol.WorldEventPayload, ttl bool, logger *slog.Logger) {

	_, _, lastAttack := a.combatYieldSnapshot()
	a.endCombatYield()
	// 双保险：入口已 clearReaction，但让位横跨多个事件，归还时确保无
	// 残留反应窗口（deadline 硬切会 stop+hint+signal 惊扰刚接回的身体）。
	a.clearReaction()
	// 攻击事件回声（修复 B 同款"再见一次"语义）：归还后的第一次分解在
	// 【发生的事件】里见到战斗的起因，而不只 hint 一句话。TTL 失败兜底
	// 路径（abandonCurrentPlan）同样不丢。EventID/EventType 全空（合成/
	// 测试事件）时跳过。
	if lastAttack.EventID != "" || lastAttack.EventType != "" {
		a.as.EchoWorldEvent(lastAttack)
	}
	var hint string
	if ttl {
		hint = fmt.Sprintf("【战斗结束】威胁情境超过 %.0f 游戏分钟未收到解除信号（combat_exit），系统兜底收回控制权。", combatYieldTTLGameSec/60)
	} else {
		hint = "【战斗结束】" + prompt.FormatWorldEvent(ev)
	}
	// hint 先落 AgentState：acquireReplanSlot 超时 / replan 失败两条兜底
	// 路径都靠 worker 自然 refill 时读到战后上下文。
	a.as.SetReplanHint(hint)
	go a.forceInterruptReplan(ctx, agentID, ws, kb, profiles, ev, hint, logger)
	logger.Info("[combat-detach] 控制权归还 agent（战斗结束），重规划回日程",
		"agent_id", agentID, "event_id", ev.EventID, "event_type", ev.EventType, "ttl_expiry", ttl)
}

// checkCombatYieldExpiry implements the combat-detach TTL safety net, called
// from the worker loop's yield guard. Cold-start convention (mirrors the
// reaction deadline): a yield begun before the first perception stores
// since=0 and stays inert until a real game time latches the anchor —
// otherwise the first perception's clock jump would instantly "expire" the
// yield mid-combat. Returns true when a TTL reclaim was triggered.
func (a *agentContext) checkCombatYieldExpiry(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	logger *slog.Logger) bool {

	active, since, lastEvent := a.combatYieldSnapshot()
	if !active {
		return false
	}
	now := a.as.LatestGameTimeSec()
	if since <= 0 {
		if now > 0 {
			// 首条感知到达：从此刻起算 TTL（beginCombatYield 时无感知，
			// 挂 0 占位）。
			a.coordMu.Lock()
			a.combatYieldSinceGameSec = now
			a.coordMu.Unlock()
			logger.Info("[combat-detach] 首条感知到达，让位 TTL 起点锁存",
				"agent_id", agentID, "game_time_sec", now)
		}
		return false
	}
	if now <= 0 || now-since < combatYieldTTLGameSec {
		return false
	}
	logger.Warn("[combat-detach] 让位超时未收到 combat_exit，TTL 兜底收回控制权",
		"agent_id", agentID, "since_game_sec", since, "now_game_sec", now)
	a.reclaimFromCombatYield(ctx, agentID, ws, kb, profiles, lastEvent, true, logger)
	return true
}

// forceInterruptReplan runs the post-interrupt replan for a force event.
// The interruption itself already fired synchronously in handleForceEvent
// (the §4.2 guarantee); this goroutine only does the tactical work of
// re-decomposing the current slot with the event injected as the hint.
// On success the event is echoed into the world-event queue（修复 B）: the
// FIRST schedule refill after the reaction sees it once more in
// 【发生的事件】— the consumed hint must not be the last trace of the event
// in the context.
func (a *agentContext) forceInterruptReplan(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	ev protocol.WorldEventPayload, hint string, logger *slog.Logger) {

	if !a.acquireReplanSlot(agentID, hint, logger) {
		return
	}
	defer func() {
		a.coordMu.Lock()
		a.replanInProgress = false
		a.coordMu.Unlock()
	}()

	ok, cancelled := a.tacticalRefillForReplan(ctx, agentID, ws, kb, profiles, logger, hint)
	if cancelled {
		// 本 force replan 被更新的 force 事件掐掉（§3.3）：那个事件的 replan
		// 已接管（其 hint 已同步注入），此处跳过 abandon（清队列/覆盖 hint
		// 都会破坏更新的 force 反应）。
		logger.Info("[world_event/force] 重规划被更新的 force 事件取消，让位",
			"agent_id", agentID)
		return
	}
	if !ok {
		// 失败兜底：紧急事件后不应恢复旧计划——清掉过期队列，worker 以
		// 当前状态自然 refill（hint 已在 AgentState，会注入战术层 prompt）。
		logger.Warn("[world_event/force] 重规划失败，清空旧队列让 worker 自然 refill",
			"agent_id", agentID)
		a.abandonCurrentPlan(agentID, ws, hint, logger, "[world_event/force]")
		return
	}

	// 打断 replan 期间仍在途的动作：预 stop 发送失败的情况重试 stop；
	// 或等待 slot 期间 worker 从旧队列 pop 出的动作（replanInProgress 置位
	// 前的窗口）。新队列已就绪，worker 待 signal pop。
	if actionID := a.as.CurrentActionID(); actionID != "" {
		if err := ws.SendStopAction(agentID, actionID); err != nil {
			logger.Warn("[world_event/force] replan 后 stop_action 发送失败（新队列已就绪，等 completion 自然推进）",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			logger.Info("[world_event/force] 重规划完成，已打断残留动作",
				"agent_id", agentID, "action_id", actionID)
		}
	}
	// 修复 B：事件回声入队——反应耗尽后的第一次日程 refill 在【发生的事件】
	// 里再见到它一次（消费一次即清）。
	if ev.EventID != "" || ev.EventType != "" {
		a.as.EchoWorldEvent(ev)
	}
	a.signal()
}

// acquireReplanSlot waits for any in-flight replan (/debug/schedule or the
// reactive layer) to finish, then takes the replanInProgress slot. Bails on
// agent stop or wait-limit expiry — in both, the interruption has already
// fired and the hint is set, so the worker's natural refill still carries
// the event context.
func (a *agentContext) acquireReplanSlot(agentID, hint string, logger *slog.Logger) bool {
	deadline := time.Now().Add(forceReplanWaitLimit)
	for {
		a.coordMu.Lock()
		switch {
		case a.stopped:
			a.coordMu.Unlock()
			return false
		case !a.replanInProgress:
			a.replanInProgress = true
			a.coordMu.Unlock()
			return true
		}
		a.coordMu.Unlock()
		if time.Now().After(deadline) {
			logger.Warn("[world_event/force] 等待在途 replan 超时，放弃本次重规划（打断已生效，hint 已注入，worker 将自然 refill）",
				"agent_id", agentID, "hint", hint)
			a.signal()
			return false
		}
		time.Sleep(forceReplanPollInterval)
	}
}

// abandonCurrentPlan drops the stale plan after a failed replan: clears the
// old action queue + in-flight tracking, sends stop for the in-flight
// action, and signals the worker so it naturally refills from current
// state (the replan hint stays set in AgentState and flows into the next
// tactical prompt via BeginTacticalRefill).
//
// Shared by the reactive layer's replan failure path and the force
// interrupt's failure path. tag prefixes the log lines.
func (a *agentContext) abandonCurrentPlan(agentID string, ws contract.Transport,
	reason string, logger *slog.Logger, tag string) {
	// 业务字段（queue + 在途追踪 + slot）通过 AgentState 原子清理；
	// 协调字段（replanInProgress + pending timer）通过 coordMu 清理。
	// 两次加锁不嵌套。
	a.recordInterrupted("旧计划清退（" + truncateRunes(reason, 40) + "）")
	info := a.as.ClearForReplan()
	actionID := info.ActionID
	queueLen := info.QueueLen
	a.as.SetReplanHint(reason)

	a.coordMu.Lock()
	a.replanInProgress = false
	if actionID != "" {
		if timer, ok := a.pendingActionTimeouts[actionID]; ok {
			timer.Stop()
			delete(a.pendingActionTimeouts, actionID)
		}
	}
	a.coordMu.Unlock()

	if actionID != "" {
		if err := ws.SendStopAction(agentID, actionID); err != nil {
			logger.Warn(tag+" replan 失败后 stop_action 发送失败",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			logger.Info(tag+" replan 失败，已 stop 原 action，worker 将自然 refill",
				"agent_id", agentID, "action_id", actionID,
				"queue_len", queueLen, "replan_reason", reason)
		}
	} else {
		logger.Info(tag+" replan 失败，无在途 action，worker 将自然 refill",
			"agent_id", agentID, "queue_len", queueLen, "replan_reason", reason)
	}
	a.signal()
}

// ─── /debug/event 注入端点（P4-13）─────────────────────────────
//
// 合成 world_event 并喂进与 UE 上报完全相同的分发入口（Runtime.
// handleWorldEvent），用于无 UE 或带 UE 联调时验证事件系统全链路
// （force 打断 / 入队 / 后续 drain）。与 UE 不冲突：本端点只注入入站
// 消息，不直接向 UE 发任何东西（force 事件触发的 stop_action 是事件
// 系统本身的正常后果，与 UE 自己推事件的行为一致）。快速预设在前端
// debug 控制台（web/debug.html 的"事件下发" tab），curl 直接 POST 完整
// 事件。仅联调用，无认证。

// debugEventSeq numbers injected event ids within the process.
var debugEventSeq atomic.Int64

// nextDebugEventID generates an id for debug-injected events, mirroring
// the protocol's evt_<YYYYMMDD>_<seq> format with a "debug" marker so it
// can never collide with UE-generated ids.
func nextDebugEventID() string {
	return fmt.Sprintf("evt_debug_%s_%06d", time.Now().Format("20060102"), debugEventSeq.Add(1))
}

// debugGameTimeNow renders the agent's current authoritative game time in
// the protocol's "D12 10:47:03" format; empty when no perception yet.
func debugGameTimeNow(ac *agentContext) string {
	gt := ac.as.LatestGameTimeSec()
	if gt <= 0 {
		return ""
	}
	day := int(gt / 86400)
	tod := math.Mod(gt, 86400)
	return fmt.Sprintf("D%d %02d:%02d:%02d", day+1,
		int(tod/3600), int(math.Mod(tod, 3600)/60), int(math.Mod(tod, 60)))
}

// debugEventRequest is the POST /debug/event body: agent_id + the full
// world_event payload. Server-side autofill when omitted: event_id
// (evt_debug_*，与 UE 生成的 id 空间隔离), occurred_at (now), game_time
// (agent 当前权威游戏时间), data (空对象).
type debugEventRequest struct {
	AgentID string                     `json:"agent_id"`
	Event   protocol.WorldEventPayload `json:"event"`
}

// debugEventResponse echoes the effective (post-autofill) event fields plus
// what the event system did with it.
type debugEventResponse struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Category  string `json:"category,omitempty"`
	EventType string `json:"event_type,omitempty"`
	Force     bool   `json:"force"`
	Detach    bool   `json:"detach"`
	Severity  int    `json:"severity"`
	GameTime  string `json:"game_time,omitempty"`
	QueueLen  int    `json:"queue_len"`
	Note      string `json:"note,omitempty"`
}

// handleDebugEvent is the /debug/event handler. inject is the Runtime's
// dispatch entry (handleWorldEvent), returning whether the event was
// accepted into the event system.
func handleDebugEvent(logger *slog.Logger, lookupAgent func(string) *agentContext,
	inject func(agentID string, ev protocol.WorldEventPayload) bool,
	w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "method not allowed, use POST"})
		return
	}
	var req debugEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "invalid JSON body: " + err.Error()})
		return
	}
	if req.AgentID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "agent_id is required"})
		return
	}
	ac := lookupAgent(req.AgentID)
	if ac == nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "agent not registered: " + req.AgentID})
		return
	}
	ev := req.Event
	if ev.Category == "" {
		// Combat-detach 联调需要复现 UE 的畸形事件（category 缺失、
		// event_type 用短名）：命中战斗别名表时放行并按 effective
		// category 补齐，端到端验证容错逻辑；其余类别仍须显式携带。
		if eff := prompt.EffectiveCategory(ev); eff != "" {
			ev.Category = eff
		} else {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "event.category is required"})
			return
		}
	}
	if ev.EventType == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "event.event_type is required"})
		return
	}
	// Autofill：id 用 evt_debug_ 前缀隔离 UE 的 id 空间（去重互不干扰）；
	// game_time 缺省取 agent 当前权威游戏时间（手填 "D12 10:47:03" 易错）。
	if ev.EventID == "" {
		ev.EventID = nextDebugEventID()
	}
	if ev.OccurredAt == 0 {
		ev.OccurredAt = time.Now().UnixMilli()
	}
	if ev.GameTime == "" {
		ev.GameTime = debugGameTimeNow(ac)
	}
	if len(ev.Data) == 0 {
		ev.Data = json.RawMessage(`{}`)
	}

	handled := inject(req.AgentID, ev)

	note := "已入队，等待安全点 drain（action_completed 时交战术层统筹）"
	if !handled {
		note = "事件被策略丢弃（手动模式或 agent 离线，见 MCP 日志）"
	} else if ev.Force && ev.Detach && !prompt.IsCombatExitEvent(ev) {
		note = "force + detach 战斗接管：已停在途动作并让位 UE 战斗 AI（MCP 静默至 combat_exit 或 TTL 兜底，见 [combat-detach] 日志）"
	} else if ev.Force {
		note = "force 硬保证：在途动作已同步打断，重规划异步进行（见 [world_event/force] 日志）"
	}
	logger.Info("[debug/event] 事件已注入",
		"agent_id", req.AgentID, "event_id", ev.EventID,
		"category", ev.Category, "event_type", ev.EventType, "force", ev.Force,
		"severity", ev.Severity, "handled", handled)

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(debugEventResponse{
		OK:        handled,
		EventID:   ev.EventID,
		Category:  ev.Category,
		EventType: ev.EventType,
		Force:     ev.Force,
		Detach:    ev.Detach,
		Severity:  ev.Severity,
		GameTime:  ev.GameTime,
		QueueLen:  ac.as.WorldEventQueueLen(),
		Note:      note,
	})
}

// ─── 反应护栏（P2-6，设计文档 §4.5 两条护栏）──────────────────
//
// ① 反应任务带绝对截止时间，不能无界——超过 deadline 的反应被打断，
//   worker 重新按日程 refill（反应吞掉整个时段是最坏情形）。
// ② 反应打断反应，要求 severity 严格更高——进行中的反应窗口内，路由
//   裁决的 severity 不高于当前反应的事件保持入队（保守：等安全点统筹）。
//
// 状态在 worker 的日程 refill（tacticalRefill）/ slot 切换 / 到期 / agent
// 下线时清除——所以"仍 armed"即"自反应开始没有发生过日程 refill"，截止
// 硬切有明确的归属。force 事件不走 ②（§4.2 不可否决），但会重置反应窗口。

// reactionDeadlineGameSec bounds one reaction task in authoritative game
// seconds (60 game minutes — a reaction is "处理完事件再回到日程"，不该
// 吞掉整个时段；反应动作本身另有 time_to_stop / slot 边界兜底)。
// var 便于测试注入。
var reactionDeadlineGameSec = 3600.0

// beginReaction arms the guard for a newly started reaction task with its
// severity ladder bar and absolute deadline. nowGameSec is the current
// authoritative game time (<= 0 = no perception yet; the deadline check
// stays inert until perception arrives).
func (a *agentContext) beginReaction(severity int, nowGameSec float64, desc string) {
	if severity < 0 {
		severity = 0
	}
	if severity > 10 {
		severity = 10
	}
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	a.reactionActive = true
	a.reactionSeverity = severity
	a.reactionDesc = desc
	if nowGameSec > 0 {
		a.reactionDeadlineGameSec = nowGameSec + reactionDeadlineGameSec
	} else {
		a.reactionDeadlineGameSec = 0 // 无感知：等首条 perception 后再算
	}
}

// reactionSnapshot returns the guard state.
func (a *agentContext) reactionSnapshot() (active bool, severity int, deadlineGameSec float64) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	return a.reactionActive, a.reactionSeverity, a.reactionDeadlineGameSec
}

// clearReaction lifts the guard. Callers: schedule refill (tacticalRefill —
// the NPC has returned to schedule-driven planning), slot switch, agent
// stop, and deadline expiry.
func (a *agentContext) clearReaction() {
	a.coordMu.Lock()
	a.reactionActive = false
	a.reactionSeverity = 0
	a.reactionDeadlineGameSec = 0
	a.reactionDesc = ""
	a.coordMu.Unlock()
}

// reactionDescSnapshot returns the description of the event that armed the
// current reaction window ("" when not active).
func (a *agentContext) reactionDescSnapshot() string {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if !a.reactionActive {
		return ""
	}
	return a.reactionDesc
}

// mayInterruptReaction implements guardrail ②: an interrupt with the given
// severity may cut an active reaction only if strictly higher. No active
// reaction → always true.
func (a *agentContext) mayInterruptReaction(severity int) bool {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	return !a.reactionActive || severity > a.reactionSeverity
}

// checkReactionDeadline implements guardrail ① (called from the worker
// loop, after the replanBusy guard): at deadline expiry the still-armed
// reaction is cut — in-flight action stopped, queue dropped, hint tells the
// next refill to return to the schedule. Still-armed means no schedule
// refill happened since the reaction started, so the current activity is
// attributable to the reaction.
// checkReactionDeadline returns true when a cut actually happened (P3-9
// trigger signal: the reaction overran its budget — the day is off-script).
func (a *agentContext) checkReactionDeadline(agentID string, ws contract.Transport, logger *slog.Logger) bool {
	active, sev, deadline := a.reactionSnapshot()
	if !active {
		return false
	}
	now := a.as.LatestGameTimeSec()
	if now <= 0 || deadline <= 0 || now < deadline {
		return false
	}
	a.clearReaction()

	// 与 abandonCurrentPlan 同模式：清队列 + stop 在途 + cancel timer +
	// signal worker。hint 是"回到日程"（不带【强制打断】前缀 → 不升级为
	// 紧急事件，走普通 hint 注入）。
	a.recordInterrupted("反应截止时间到，切回日程")
	info := a.as.ClearForReplan()
	actionID := info.ActionID
	a.as.SetReplanHint("【反应截止】对紧急事件的反应时间已用完，请立即回到当前时段目标的原有日程继续安排。")

	a.coordMu.Lock()
	if actionID != "" {
		if timer, ok := a.pendingActionTimeouts[actionID]; ok {
			timer.Stop()
			delete(a.pendingActionTimeouts, actionID)
		}
	}
	a.coordMu.Unlock()
	if actionID != "" {
		if err := ws.SendStopAction(agentID, actionID); err != nil {
			logger.Warn("[反应护栏] 截止 stop_action 发送失败（worker 将自然 refill）",
				"agent_id", agentID, "action_id", actionID, "err", err)
		}
	}
	logger.Info("[反应护栏] 反应任务到截止时间，切回日程",
		"agent_id", agentID, "severity", sev, "game_time", now, "deadline", deadline,
		"action_id", actionID, "queue_len", info.QueueLen)
	a.signal()
	return true
}

// ─── Combat-detach yield state (combat-detach) ──────────────────────────────
//
// detach=true 的战斗事件 = "UE 战斗 AI 接管身体，agent 让位"。与 reaction
// guard 的本质区别：reaction 是 agent 自己的反应任务（有 deadline、会被
// refill 清除），yield 是把身体交给 UE（agent 全静默，直到 combat_exit 或
// TTL 兜底收回）。字段生命周期见 agentContext 定义处的注释。

// combatYieldActive reports whether UE's combat AI currently owns the body.
func (a *agentContext) combatYieldActive() bool {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	return a.combatYield
}

// combatYieldSnapshot returns the full yield state for the TTL check and the
// reclaim path.
func (a *agentContext) combatYieldSnapshot() (active bool, sinceGameSec float64, lastEvent protocol.WorldEventPayload) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	return a.combatYield, a.combatYieldSinceGameSec, a.combatYieldLastEvent
}

// beginCombatYield enters (or refreshes) the yield. Idempotent: a repeat
// attack while already yielded refreshes the TTL anchor and the echoed
// event, but never re-stops/re-clears (the caller's stop/clear work only
// happens on entry — see handleForceEvent's detach branch). nowGameSec
// follows beginReaction's convention: <= 0 (no perception yet) stores 0 and
// leaves the TTL inert until checkCombatYieldExpiry latches a real time.
func (a *agentContext) beginCombatYield(ev protocol.WorldEventPayload, nowGameSec float64) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	a.combatYield = true
	a.combatYieldLastEvent = ev
	if nowGameSec > 0 {
		a.combatYieldSinceGameSec = nowGameSec
	} else {
		a.combatYieldSinceGameSec = 0
	}
}

// endCombatYield lifts the yield. The caller (returnFromCombatYield) drives
// the post-combat replan; this only clears the flag.
func (a *agentContext) endCombatYield() {
	a.coordMu.Lock()
	a.combatYield = false
	a.combatYieldSinceGameSec = 0
	a.combatYieldLastEvent = protocol.WorldEventPayload{}
	a.coordMu.Unlock()
}

// situationKindCombat is the ongoing-threat situation kind (P3-9 修复 A).
const situationKindCombat = "combat"

// situationTTLGameSec bounds a situation without a resolution event
// (combat_exit never arrives): 30 game minutes, then it auto-degrades.
const situationTTLGameSec = 30 * 60.0

// combatYieldTTLGameSec bounds a combat-detach yield without a combat_exit
// handback (UE forgot / not implemented / bug): the agent reclaims control
// after this many game seconds since the LAST detach event. Each new attack
// refreshes the window, so an ongoing fight never expires mid-combat.
const combatYieldTTLGameSec = 30 * 60.0
