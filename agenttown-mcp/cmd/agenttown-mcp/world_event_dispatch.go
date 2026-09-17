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
	go ac.forceInterruptReplan(rt.ctx, agentID, rt.ws, *rt.kbPtr, rt.profiles, hint, rt.logger)
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
func (a *agentContext) forceInterruptReplan(ctx context.Context, agentID string,
	ws contract.Transport, kb *worldkb.KB, profiles map[string]*profile.Profile,
	hint string, logger *slog.Logger) {

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
