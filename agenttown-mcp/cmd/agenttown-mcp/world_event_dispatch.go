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

	// 修复 A：combat 类事件是持续状态的边沿——attacked/targeted 登记威胁
	// 情境（直到 combat_exit 或 TTL），combat_exit 解除。放在 force 分流
	// 之前：登记/解除对 force 与非 force 路径一视同仁。
	if isCombatStartEvent(ev) {
		ac.as.BeginSituation(situationKindCombat, prompt.FormatWorldEvent(ev), ac.as.LatestGameTimeSec())
		rt.logger.Info("[world_event] 登记持续威胁情境（combat_exit 或 TTL 解除）",
			"agent_id", agentID, "event_id", ev.EventID, "event_type", ev.EventType)
	} else if isCombatExitEvent(ev) {
		if ac.as.EndSituation(situationKindCombat) {
			rt.logger.Info("[world_event] 威胁情境已解除（combat_exit）",
				"agent_id", agentID, "event_id", ev.EventID)
		}
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
	ac.beginReaction(ev.Severity, ac.as.LatestGameTimeSec())
	go ac.forceInterruptReplan(rt.ctx, agentID, rt.ws, *rt.kbPtr, rt.profiles, ev, hint, rt.logger)
}

// forceEventHint formats the replan hint for a force event. The 【强制打断】
// marker is the contract with prompt.tacticalHintLine: it renders the event
// as a top-priority 【紧急事件】 directive (explicitly overriding the slot
// goal and the duration-filling rules) instead of a mere interruption note.
func forceEventHint(ev protocol.WorldEventPayload) string {
	return "【强制打断】" + prompt.FormatWorldEvent(ev)
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
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugEventResponse{Error: "event.category is required"})
		return
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
func (a *agentContext) beginReaction(severity int, nowGameSec float64) {
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
	a.coordMu.Unlock()
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

// situationKindCombat is the ongoing-threat situation kind (P3-9 修复 A).
const situationKindCombat = "combat"

// situationTTLGameSec bounds a situation without a resolution event
// (combat_exit never arrives): 30 game minutes, then it auto-degrades.
const situationTTLGameSec = 30 * 60.0

// isCombatStartEvent reports whether the event starts a combat threat
// (player_attacked / player_targeted — both are force by protocol, but the
// check is label-based so a mislabeled non-force one still registers).
func isCombatStartEvent(ev protocol.WorldEventPayload) bool {
	return ev.Category == protocol.CategoryPlayerInteraction &&
		(ev.EventType == protocol.EventTypePlayerAttacked || ev.EventType == protocol.EventTypePlayerTargeted)
}

// isCombatExitEvent reports whether the event resolves the combat threat.
func isCombatExitEvent(ev protocol.WorldEventPayload) bool {
	return ev.Category == protocol.CategoryPlayerInteraction &&
		ev.EventType == protocol.EventTypeCombatExit
}
