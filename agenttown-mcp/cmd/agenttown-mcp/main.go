// Command agenttown-mcp is the MCP server bridging Mock UE to Hermes Gateway
// for the AgentTown_v3 project.
//
// Three roles in one process:
//
//  1. MCP Server (Streamable HTTP at :8760/mcp) — Hermes connects here as
//     a standard MCP client, discovers the game tools, and calls them.
//  2. WebSocket Server (:9090/ws) — Mock UE (simulating UE) connects here,
//     pushes protocol messages (perception_update / state_report / ...).
//  3. Hermes HTTP Client — owns the per-game-day session.
//
// Messages follow the 7-field envelope in pkg/protocol. The action
// lifecycle is command → action_started(ACK) → action_completed; tools
// return after ACK and completions are folded into the next perception.
//
// IMPORTANT: in stdio mode, never write logs to stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/internal/log"
	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/transport"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

var version = "0.1.0-dev"

// agentContext holds per-agent state accumulated between decision turns.
// Delivery uses a latest-wins queue: at most one Hermes request is in flight
// per agent, pending trigger reasons merge, and only the newest perception is
// retained while that request runs.
type agentContext struct {
	mu                    sync.Mutex
	online                bool
	agentEpoch            int64
	latestPhysical        *protocol.PhysicalState
	latestPerception      json.RawMessage
	currentTask           *protocol.CurrentTaskProgress
	currentActionID       string                 // 当前执行中的 action_id（mu 保护），空表示无执行中动作
	currentActionCmd      string                 // mu 保护，当前在途 action 的 cmd（如 MoveToLocation / WorkAtWorkbench），用于反应层 prompt
	currentActionParams   map[string]any         // mu 保护，当前在途 action 的 params，用于反应层 prompt
	currentActionStart    time.Time              // mu 保护，当前在途 action 的开始时间，用于反应层计算 elapsed
	pendingActionTimeouts map[string]*time.Timer // action_id → 超时 timer（mu 保护），约定 §5.2 action_completed 1.5× 估值超时
	completedBeforeArm    map[string]struct{}   // action_id → 已完成但 timer 尚未 arm（mu 保护），防止 ACK/completion 竞态导致 timer 泄漏
	dailyPlan             string                 // mu 保护，战略层生成的每日计划（格式化字符串），空=未生成或失败
	strategicHc           llmClient              // mu 保护，战略层专用 LLM client（hermes 或 venus，独立 session）
	tacticalHc            llmClient              // mu 保护，战术层专用 LLM client（hermes 或 venus，独立 session）
	actionQueue           []plannedAction        // mu 保护，战术层分解出的待执行 action（FIFO）
	currentActionSrc      actionSource           // mu 保护，当前在途 action 的来源（hermes/tactical/空）
	currentPlanIndex      int                    // mu 保护，当前执行到 daily_plan 第几个 item（记账用）
	currentSlot           string                 // mu 保护，当前分解的时段 "HH:MM-HH:MM"（防同时段重复分解）
	redecomposeCount      int                    // mu 保护，当前时段已重复分解次数（防死循环）
	// 反应层状态（mu 保护）
	prevZone         string               // 上次感知的 zone id（用于检测 zone 变化触发反应层）
	prevObjectIDs    []string             // 上次感知的 nearby_objects id 列表（用于检测新物体出现）
	lastReactiveAt   map[string]time.Time // 去抖：trigger dedupe key → 上次触发时间
	perceptionCount  int                  // 累计感知次数（用于周期性触发反应层）
	debugOverride    bool                 // mu 保护，debug 手动 action 期间暂停 worker dispatch（避免 stop→idle wait 竞态）
	wake           chan struct{}
	cancel         context.CancelFunc
	stopped        bool
	// replan 状态（mu 保护）
	lastReplanAt     time.Time // wall-clock，30 min 去抖（replanDedupeWindow）
	replanInProgress bool      // replan 规划进行中，阻止 worker 抢先 pop/refill
	replanHint       string    // 传入战术层 prompt 的"上次中断原因"（replan reason）
}

func newAgentContext(parent context.Context, epochs ...int64) (*agentContext, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	agentEpoch := int64(1)
	if len(epochs) > 0 {
		agentEpoch = epochs[0]
	}
	return &agentContext{
		online: true, agentEpoch: agentEpoch,
		wake: make(chan struct{}, 1), cancel: cancel,
		pendingActionTimeouts: make(map[string]*time.Timer),
		completedBeforeArm:    make(map[string]struct{}),
		lastReactiveAt:        make(map[string]time.Time),
	}, ctx
}

// observePerception 存储最新感知payload，供战术层 refill 时读取当前世界状态。
// 反应层：检测 zone 变化 / 新物体出现 / 周期性触发，若显著变化则返回 trigger 信息供
// message handler 异步触发 reactiveRunner.trigger。
func (a *agentContext) observePerception(payload json.RawMessage) (ReactiveTrigger, string, error) {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", "", fmt.Errorf("parse perception: %w", err)
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return "", "", nil
	}
	// 提取当前 zone + object ids，对比 prev 检测显著变化
	curZone := ""
	if p.Location.CurrentZone != nil {
		curZone = *p.Location.CurrentZone
	}
	curObjectIDs := extractObjectIDs(p)
	prevZone := a.prevZone
	prevObjectIDs := a.prevObjectIDs
	prevPhysical := a.latestPhysical
	// 更新感知状态
	a.latestPerception = cloneRawMessage(payload)
	a.prevZone = curZone
	a.prevObjectIDs = curObjectIDs
	a.perceptionCount++
	pCount := a.perceptionCount
	a.mu.Unlock()

	// 检测显著变化（zone/新物体）。物理警戒带由 updateState 检测，
	// 这里 prev/cur physical 都用 latestPhysical（即上次 state_report），
	// 不重复检测物理触发。
	trigger, detail := shouldTriggerReactive(prevZone, curZone, prevObjectIDs, curObjectIDs, prevPhysical, prevPhysical)
	// 事件类触发优先；无事件时检查周期性触发
	if trigger == "" {
		trigger, detail = shouldTriggerPeriodic(pCount)
	}

	// 感知是 worker 的主驱动源：每次感知到达都唤醒它检查战术队列
	// （pop 下一个 / refill 新时段）。tacticalRefill 内部的守卫避免重复 LLM 调用。
	a.signal()
	return trigger, detail, nil
}

// updateState 存储权威的物理/任务状态。反应层：检测物理状态突破警戒带，
// 返回 trigger 信息供 message handler 触发 reactiveRunner。
func (a *agentContext) updateState(report protocol.StateReportPayload) (ReactiveTrigger, string) {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return "", ""
	}
	prevPhysical := a.latestPhysical
	physical := report.PhysicalState
	a.latestPhysical = &physical
	a.currentTask = cloneTask(report.CurrentTaskProgress)
	a.mu.Unlock()

	// 检测物理警戒带突破（zone/objects 不在此检测，由 observePerception 负责）
	return shouldTriggerReactive("", "", nil, nil, prevPhysical, &physical)
}

// recordActionCompletion 处理 action_completed。所有来源的 completion 都清
// 在途追踪并 signal worker（pop 下一个或 refill）。反应层：仅在 action 异常
// 完成（failed/interrupted/error）时触发评估——成功完成是常态，每次都问
// "要不要打断"意义不大（模型看不到战术层整体规划，只能基于贫乏信息答 continue）。
// 异常完成才是真正需要反应层介入的时机。
func (a *agentContext) recordActionCompletion(completion protocol.ActionCompletedPayload) (bool, ReactiveTrigger, string) {
	a.mu.Lock()
	if a.currentTask != nil && a.currentTask.ActionID == completion.ActionID {
		a.currentTask = nil
	}
	if a.currentActionID == completion.ActionID {
		a.currentActionID = ""
		a.currentActionCmd = ""
		a.currentActionParams = nil
		a.currentActionStart = time.Time{}
	}
	// 取消 action_completed 超时 timer（约定 §5.2）。
	// 竞态处理：ACK 和 action_completed 可能同一批到达（read loop 顺序处理），
	// completion handler 可能在 SendAction 调用方 armActionTimeout 之前执行。
	// 此时 timer 尚未注册，记录到 completedBeforeArm，让 armActionTimeout 跳过 arm。
	if timer, ok := a.pendingActionTimeouts[completion.ActionID]; ok {
		timer.Stop()
		delete(a.pendingActionTimeouts, completion.ActionID)
	} else {
		a.completedBeforeArm[completion.ActionID] = struct{}{}
	}
	a.currentActionSrc = ""
	a.mu.Unlock()
	a.signal()
	// 反应层触发：仅异常完成触发。detail 用 result 作为去抖维度（避免每次
	// action_id 不同导致去抖失效），相同 result 在 60s 内不重复触发。
	if completion.Result == protocol.ResultSuccess {
		return true, "", ""
	}
	detail := fmt.Sprintf("result=%s progress=%.2f", completion.Result, completion.Progress)
	return true, TriggerActionDone, detail
}

// recordEventNotification 处理环境事件通知。反应层：返回 trigger 信息供
// message handler 异步触发 reactiveRunner。环境事件不打断战术队列——
// reactiveRunner 决策若为 replan 才会发 stop_action。
func (a *agentContext) recordEventNotification(event protocol.EventNotificationPayload) (ReactiveTrigger, string) {
	// 提取事件类型用于去抖键（事件 id 每次不同，去抖会失效；type 是合理维度）
	eventType, _ := event.Event["type"].(string)
	if eventType == "" {
		eventType = "unknown"
	}
	detail := fmt.Sprintf("event_id=%s level=%s type=%s",
		event.EventID, event.PerceptionLevel, eventType)
	return TriggerEventNotify, detail
}

func (a *agentContext) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *agentContext) stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	a.online = false
	a.latestPerception = nil
	a.currentActionID = "" // agent 下线时清空（避免残留）
	a.currentActionSrc = ""
	a.currentActionCmd = ""
	a.currentActionParams = nil
	a.currentActionStart = time.Time{}
	a.actionQueue = nil    // 清空战术层队列
	a.currentSlot = ""     // 重置时段
	a.redecomposeCount = 0 // 重置重复分解计数
	// 停止所有 pending action 超时 timer
	for _, timer := range a.pendingActionTimeouts {
		timer.Stop()
	}
	a.pendingActionTimeouts = make(map[string]*time.Timer)
	cancel := a.cancel
	a.mu.Unlock()
	cancel()
}

func (a *agentContext) recordActionStarted(actionID, cmd string, params map[string]any, decisionEpoch int64, src actionSource) {
	_ = decisionEpoch // 反应层移除后不再记录到 recentActions，保留参数兼容调用方
	a.mu.Lock()
	a.currentActionID = actionID // 追踪当前执行中的 action（约定9 stop_action ID 匹配）
	a.currentActionSrc = src     // 记录来源：completion 时按来源路由（tactical→signal pop 下一个）
	a.currentActionCmd = cmd     // 反应层 prompt 用：描述当前在途动作
	a.currentActionParams = params
	a.currentActionStart = time.Now()
	a.mu.Unlock()
}

func cloneRawMessage(payload json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), payload...)
}

func clonePhysical(physical *protocol.PhysicalState) *protocol.PhysicalState {
	if physical == nil {
		return nil
	}
	copy := *physical
	return &copy
}

func cloneTask(task *protocol.CurrentTaskProgress) *protocol.CurrentTaskProgress {
	if task == nil {
		return nil
	}
	copy := *task
	return &copy
}

// runPerceptionWorker 是战术层队列驱动的状态机：wake → UE 连接检查 →
// pop 队列下一个 action；队列空则 tacticalRefill，无 goal/失败则发短 wait。
// 反应层仅 continue/observe/replan：replan 通过 tacticalRefillForReplan
// 重规划并打断在途 action，不打断 worker 正常的 pop/refill 循环。
func runPerceptionWorker(
	ctx context.Context,
	agentID string,
	ac *agentContext,
	ws *wsserver.Server,
	kb *worldkb.KB,
	logger *slog.Logger,
) {
	// 战略层：进入感知循环前生成当日计划。
	plan := generateDailyPlan(ctx, ac.strategicHc, agentID, kb, logger)
	ac.mu.Lock()
	ac.dailyPlan = plan
	ac.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			logger.Info("perception worker stopped", "agent_id", agentID)
			return
		case <-ac.wake:
		}

		// UE 断线时跳过本轮，等重连后 agent_registered 路径 signal 唤醒。
		if !ws.IsConnected() {
			logger.Warn("[UE 断线] 跳过本轮，等重连", "agent_id", agentID)
			continue
		}

		// replan 规划进行中：跳过 pop/refill，等 tacticalRefillForReplan 完成后
		// signal 唤醒。避免规划期间 worker 抢先 pop 旧队列剩余 action 或 refill
		// 覆盖正在生成的新队列。
		ac.mu.Lock()
		replanBusy := ac.replanInProgress
		ac.mu.Unlock()
		if replanBusy {
			continue
		}

		// 在途 action（composite 执行中）时跳过 pop/refill：UE 正忙，pop 出的
		// action 会被 busy 拒，refill 出的队列也会被拒。等 action_completed 自然
		// 唤醒 worker（completion 路径会 signal 并清 currentActionID）。
		if ac.hasInFlightAction() {
			continue
		}

		// debug 手动 action 期间暂停 dispatch：debug 端点刚发了 stop_action 清 UE
		// busy，若此刻 worker 被唤醒会立刻补一个 idle wait 重新占用，导致手动
		// action 被 busy 拒。debugOverride 由 handleDebugAction 设置/清除。
		ac.mu.Lock()
		override := ac.debugOverride
		ac.mu.Unlock()
		if override {
			continue
		}

		if ac.hasQueueNext() {
			// 队列还有下一个：pop 并直发
			ac.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
		} else {
			// 队列空：尝试战术 refill；无 goal 或失败则发短 wait 避免忙循环
			if !ac.tacticalRefill(ctx, agentID, ws, kb, logger) {
				ac.sendIdleWait(ctx, agentID, ws, logger)
			}
		}
	}
}

// extractTimeOfDay 从 perception_update payload 中提取 "HH:MM" 格式的游戏时间。
// 战术层用它判断当前时段、选 goal。失败返回空串。
// 用于按需注入每日计划（判断时段边界跨越）。失败返回空串。
func extractTimeOfDay(raw json.RawMessage) string {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Environment.TimeOfDay
}

type guardedExecutor struct {
	ws     *wsserver.Server
	lookup func(string) *agentContext
	caps   *CapabilityRegistry // per-agent cmd capability gate; nil = no check
}

func (g *guardedExecutor) validate(agentID string) (*agentContext, error) {
	ac := g.lookup(agentID)
	if ac == nil {
		return nil, fmt.Errorf("unknown agent: %s", agentID)
	}
	if !g.ws.IsConnected() {
		return nil, errors.New("UE disconnected")
	}
	return ac, nil
}

func (g *guardedExecutor) SendAction(ctx context.Context, agentID string, decisionEpoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error) {
	ac, err := g.validate(agentID)
	if err != nil {
		return nil, err
	}
	// Per-agent capability gate: reject if the agent doesn't have the
	// required cmd in its effective capability set (per-agent override
	// over global default). Defense-in-depth on top of tactical-layer
	// prompt filtering — protects against LLM hallucinating an action
	// the agent can't execute.
	if g.caps != nil && !g.caps.HasCmd(agentID, cmd) {
		return nil, fmt.Errorf("agent %s lacks capability for cmd %s", agentID, cmd)
	}
	ack, err := g.ws.SendAction(ctx, agentID, cmd, params)
	if err == nil && ack != nil {
		ac.recordActionStarted(ack.ActionID, cmd, params, decisionEpoch, sourceHermes)
		// 注册 action_completed 超时 timer（约定 §5.2：estimated_duration × 1.5）
		ac.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, g.ws, agentID, g.lookup)
	}
	return ack, err
}

// RequestScan 请求 UE 立即回吐一次 perception_update（fire-and-forget）。
// 感知到达后走正常 observePerception 路径，触发反应层评估。
func (g *guardedExecutor) RequestScan(ctx context.Context, agentID, scanID string) error {
	if _, err := g.validate(agentID); err != nil {
		return err
	}
	return g.ws.RequestScan(ctx, agentID, scanID)
}

// SendStopAction 发送 stop_action 控制消息停止指定 action。
// actionID 为空时查 agentContext 当前在途 action；仍为空则返回 nil（无在途 action 是 no-op）。
func (g *guardedExecutor) SendStopAction(agentID, actionID string) error {
	ac, err := g.validate(agentID)
	if err != nil {
		return err
	}
	if actionID == "" {
		ac.mu.Lock()
		actionID = ac.currentActionID
		ac.mu.Unlock()
		if actionID == "" {
			return nil // 无在途 action，no-op
		}
	}
	return g.ws.SendStopAction(agentID, actionID)
}

// armActionTimeout 注册 action_completed 超时 timer（约定 §5.2）。
// 超时时长 = estimated_duration_sec × 1.5；est 为 nil 或 ≤0 时默认 60s。
// 超时回调：发 stop_action 停止该动作 + 日志警告 + 触发重新决策。
func (a *agentContext) armActionTimeout(
	actionID string,
	estDurationSec *float64,
	ws *wsserver.Server,
	agentID string,
	lookup func(string) *agentContext,
) {
	timeout := 60 * time.Second // 默认
	if estDurationSec != nil && *estDurationSec > 0 {
		timeout = time.Duration(*estDurationSec * 1.5 * float64(time.Second))
		// 设下限 5s 避免估算过短导致误超时
		if timeout < 5*time.Second {
			timeout = 5 * time.Second
		}
	}

	timer := time.AfterFunc(timeout, func() {
		logger := slog.Default()
		logger.Warn("action_completed timeout, sending stop_action",
			"agent_id", agentID, "action_id", actionID, "timeout", timeout)
		// 发 stop_action 停止该动作
		if err := ws.SendStopAction(agentID, actionID); err != nil {
			logger.Error("stop_action send failed on action timeout", "err", err)
		}
		// 清除本地追踪并触发重新决策
		ac := lookup(agentID)
		if ac != nil {
		ac.mu.Lock()
		ac.currentActionID = ""
		ac.currentActionSrc = "" // 清来源，避免 completion 路由错乱
		ac.currentActionCmd = ""
		ac.currentActionParams = nil
		ac.currentActionStart = time.Time{}
		delete(ac.pendingActionTimeouts, actionID)
		ac.mu.Unlock()
			// 触发重新决策（下次 perception 会处理）
			select {
			case ac.wake <- struct{}{}:
			default:
			}
		}
	})

	a.mu.Lock()
	// 竞态处理：如果 action_completed 已先于 ACK 到达（read loop 同批处理），
	// recordActionCompletion 会把它记到 completedBeforeArm。此时不应 arm timer，
	// 否则永不会被取消，180s 后盲触发 stop_action（STOP_ID_MISMATCH）。
	if _, alreadyDone := a.completedBeforeArm[actionID]; alreadyDone {
		delete(a.completedBeforeArm, actionID)
		a.mu.Unlock()
		// 竞态保护：time.AfterFunc 创建即启动，此时 timer 已在倒计时但永不会被
		// recordActionCompletion 取消（completion 已处理过），必须显式 stop，
		// 否则 5s/180s 后盲触发 stop_action（STOP_ID_MISMATCH）。
		timer.Stop()
		return
	}
	// 如果已有同 action_id 的 timer，先 stop 旧的
	if old, ok := a.pendingActionTimeouts[actionID]; ok {
		old.Stop()
	}
	a.pendingActionTimeouts[actionID] = timer
	a.mu.Unlock()
}

// ─── 战术层队列辅助方法 ────────────────────────────────────────

// hasQueueNext 返回队列是否还有待执行 action（mu 保护）。
func (a *agentContext) hasQueueNext() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actionQueue) > 0
}

// hasInFlightAction 返回是否有在途 action（已下发未 completion）。
// worker 用它在主循环跳过 pop/refill，避免 UE busy 拒绝循环。
func (a *agentContext) hasInFlightAction() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentActionID != ""
}

// queueLen 返回队列长度（mu 保护）。
func (a *agentContext) queueLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.actionQueue)
}

// latestTimeOfDay 从 latestPerception 提取 "HH:MM" 游戏时间（mu 保护读取 perception）。
func (a *agentContext) latestTimeOfDay() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestTimeOfDayLocked()
}

// latestTimeOfDayLocked 同 latestTimeOfDay 但假定调用方已持锁。
func (a *agentContext) latestTimeOfDayLocked() string {
	return extractTimeOfDay(a.latestPerception)
}

// latestZone 从 latestPerception 提取当前区域 id（mu 保护读取 perception）。
func (a *agentContext) latestZone() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestZoneLocked()
}

// latestZoneLocked 同 latestZone 但假定调用方已持锁。
func (a *agentContext) latestZoneLocked() string {
	if len(a.latestPerception) == 0 {
		return ""
	}
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(a.latestPerception, &p); err != nil {
		return ""
	}
	if p.Location.CurrentZone != nil {
		return *p.Location.CurrentZone
	}
	return ""
}

// sendIdleWait 发一个 60 秒的 wait，避免队列空且无 goal 时忙循环。
func (a *agentContext) sendIdleWait(ctx context.Context, agentID string, ws *wsserver.Server, logger *slog.Logger) {
	if !ws.IsConnected() {
		return
	}
	// 有在途 action 时跳过：UE 正在执行 composite，wait 会被 busy 拒。
	// 等 action_completed 自然唤醒 worker（completion 路径会 signal）。
	a.mu.Lock()
	inFlight := a.currentActionID != ""
	a.mu.Unlock()
	if inFlight {
		return
	}
	ack, err := ws.SendAction(ctx, agentID, protocol.CmdWait, map[string]any{"duration_sec": 60})
	if err != nil {
		logger.Debug("[战术层] idle wait 发送失败", "agent_id", agentID, "err", err)
		return
	}
	if ack != nil {
		// 与 popAndSendQueueAction 一致：记录在途 action + 注册超时 timer，
		// 否则 currentActionID 为空，hasInFlightAction() 永远 false，
		// completion 的 signal 会立即唤醒 worker 重入 sendIdleWait 形成忙循环。
		a.recordActionStarted(ack.ActionID, protocol.CmdWait, nil, 0, sourceTactical)
		a.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, ws, agentID, func(string) *agentContext { return a })
	}
}

// popAndSendQueueAction 从队列 pop 一个 action，映射后直发 ws.SendAction。
// 不经过 Hermes / MCP 工具 / guardedExecutor（无活跃 decision_epoch）。
// 手动 recordActionStarted + armActionTimeout，source=tactical。
func (a *agentContext) popAndSendQueueAction(ctx context.Context, agentID string,
	ws *wsserver.Server, kb *worldkb.KB, logger *slog.Logger) {

	a.mu.Lock()
	if len(a.actionQueue) == 0 {
		a.mu.Unlock()
		return
	}
	// 在途 action 时不要 pop：UE 正忙，pop 出的 action 会被 busy 拒。
	// 等 action_completed 唤醒 worker（清 currentActionID）再 pop。
	if a.currentActionID != "" {
		a.mu.Unlock()
		return
	}
	pa := a.actionQueue[0]
	a.actionQueue = a.actionQueue[1:]
	a.mu.Unlock()

	cmd, params, err := mapTacticalAction(pa, agentID, kb, capabilityRegistryRef)
	if err != nil {
		logger.Warn("[战术层] action 映射失败，跳过", "agent_id", agentID, "action", pa.Action, "err", err)
		// 跳过这一个，signal 让 worker 处理下一个（若队列空则触发 refill）
		a.signal()
		return
	}

	logger.Info("[战术层] 下发 action", "agent_id", agentID, "action", pa.Action, "cmd", cmd, "queue_left", a.queueLen())
	ack, err := ws.SendAction(ctx, agentID, cmd, params)
	if err != nil {
		// 区分两种失败：
		//   (a) UE 在途 composite 未完成 → 回填队首，等在途 action_completed 唤醒
		//   (b) UE 断线 / 真错误 → 无在途 action 会触发 completion，signal 让
		//       worker 下一轮重试 pop / refill
		a.mu.Lock()
		hasInFlight := a.currentActionSrc == sourceTactical
		if hasInFlight {
			a.actionQueue = append([]plannedAction{pa}, a.actionQueue...)
			a.mu.Unlock()
			logger.Warn("[战术层] 下发失败（在途 action 占用），回填队首等待 completion",
				"agent_id", agentID, "action", pa.Action, "err", err)
			return
		}
		a.mu.Unlock()
		logger.Warn("[战术层] 下发失败，signal worker 下一轮重试", "agent_id", agentID, "err", err)
		a.signal()
		return
	}
	if ack != nil {
		// 复用现有记账 + 超时机制；source=tactical 让 completion 走队列路径
		a.recordActionStarted(ack.ActionID, cmd, params, 0 /*无 decision_epoch*/, sourceTactical)
		// lookup 返回 a 自身——超时回滚只需清当前 agent 状态
		a.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, ws, agentID, func(string) *agentContext { return a })
	}
}

// tacticalStreamingEnabled controls whether tacticalRefill uses streaming LLM
// calls (generateTacticalPlanStreaming) or non-streaming (generateTacticalPlan).
// Set from --tactical-stream flag. Default false: streaming only helps when the
// upstream LLM emits tokens incrementally; if it buffers the full response
// (DeepSeek peak-hour queueing behavior), streaming adds SSE overhead with no
// latency benefit.
var tacticalStreamingEnabled bool

// tacticalCallTimeout 是单次战术层 LLM 调用（流式或非流式）的硬超时。
// 由 --tactical-timeout flag 配置，默认 60s。
// 之前直接用进程 ctx，导致 Hermes 后端 DeepSeek 排队时单次调用最长卡 120s，
// 整个游戏时段空转。超时后由调用方发 idle wait，下一个感知周期重新尝试，
// 比死等更划算。
var tacticalCallTimeout = 60 * time.Second

// llmClient 是战略层/战术层 LLM 客户端的统一接口。
// *hermes.Client（Hermes Gateway，OpenAI Responses 协议，默认）和 *venus.Client
// （Venus 代理，OpenAI Chat Completions 协议）均实现此接口，通过 --llm-backend 切换。
type llmClient interface {
	SendWithSummary(ctx context.Context, input, summary string) (*hermes.Response, error)
	SendStreaming(ctx context.Context, input string, onDelta func(string)) (*hermes.Response, error)
	ResetSession()
}

// 编译期断言：两个 backend 都满足 llmClient 接口。
var (
	_ llmClient = (*hermes.Client)(nil)
	_ llmClient = (*venus.Client)(nil)
)

// reactiveRunnerRef 是进程级反应层执行器（package-level 便于 WS handler 调用）。
// nil 表示反应层未启用（--ollama-url="" 或客户端初始化失败）。
// trigger() 内部 nil-check，WS handler 无需额外判空。
var reactiveRunnerRef *reactiveRunner

// capabilityRegistryRef 是进程级能力注册表（package-level 便于战术层 worker 与
// debug handler 引用，避免长串参数传递）。nil 表示未启用能力过滤（降级为全量
// 内置工具）。main() 启动时赋值。
var capabilityRegistryRef *CapabilityRegistry

// kbRef 是当前生效的 world KB 指针（package-level 供 debug handler 引用）。
// worldKBSwap 成功后同步更新；runHTTP 的 /debug/kb handler 读 kbRef 而不是
// 闭包捕获的 kb 参数，确保 UE 推送新 KB 后 /debug/kb 返回最新数据。
var kbRef *worldkb.KB

// tacticalRefill 调战术层 LLM 流式分解当前时段 goal，边接收边入队，
// 首 action 在流式期间即提前下发以降低体感延迟。成功返回 true。
func (a *agentContext) tacticalRefill(ctx context.Context, agentID string,
	ws *wsserver.Server, kb *worldkb.KB, logger *slog.Logger) bool {

	// 1. 取当前时段 goal（持锁）
	a.mu.Lock()
	if a.tacticalHc == nil {
		a.mu.Unlock()
		return false // tacticalHc 未初始化，调用方发 idle wait
	}
	// 在途 action 未完成时禁止 refill：UE 此时仍占用，refill 出的队列会被
	// UE busy 全部拒绝，整队消耗光。等在途 action_completed 自然唤醒 worker 再 refill。
	if a.currentActionID != "" {
		a.mu.Unlock()
		return false
	}
	goal, slot, idx := selectCurrentGoal(a.dailyPlan, a.latestTimeOfDayLocked())
	if goal == "" {
		a.mu.Unlock()
		return false
	}
	// 同时段重复分解守卫：队列还有 action 时不 redecompose（继续执行剩余 action）；
	// 队列空且已 redecompose ≥1 次才放弃，调用方发 idle wait 等下一时段。
	// 阈值从 2 收紧到 1：避免队列提前耗尽时 50s 内连续重调 LLM 浪费 token
	// （LLM 若 1 次重分解仍给不够时长的 plan，第 2 次大概率也不够，不如 idle wait）。
	if slot == a.currentSlot {
		if len(a.actionQueue) > 0 {
			a.mu.Unlock()
			return false // 队列有剩余，等它们执行完再考虑 redecompose
		}
		if a.redecomposeCount >= 1 {
			a.mu.Unlock()
			return false
		}
	}
	zone := a.latestZoneLocked()
	physical := clonePhysical(a.latestPhysical)
	tacticalHc := a.tacticalHc
	kbRef := kb
	// 读取 replanHint（反应层 replan 设置的"上次中断原因"），让战术层
	// LLM 看到中断理由（如"疲劳>60需要休息"）从而规划休息/充电动作，而非
	// 继续规划工作动作导致循环 replan。消费后清空，避免下次 refill 误用。
	hint := a.replanHint
	a.replanHint = ""
	a.actionQueue = nil
	a.mu.Unlock()

	var actions []plannedAction
	var err error

	// 战术层 LLM 调用统一 30s 硬超时：避免 Hermes 后端排队时单次调用卡 120s
	// 拖死整个游戏时段。超时后调用方发 idle wait，下一感知周期重试。
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	if tacticalStreamingEnabled {
		// 流式路径：onAction 回调逐个入队 + 首 action 提前下发。
		// 回调在 SendStreaming 的 onDelta 调用栈里同步执行（worker 仍阻塞在 tacticalRefill），
		// 不跨回调持有 mu。
		_, _, err = generateTacticalPlanStreaming(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, logger, hint, capabilityRegistryRef,
			func(pa plannedAction) {
				a.mu.Lock()
				a.actionQueue = append(a.actionQueue, pa)
				shouldDispatch := a.currentActionID == "" && len(a.actionQueue) == 1
				a.mu.Unlock()
				if shouldDispatch {
					logger.Info("[战术层] 流式下发首 action", "agent_id", agentID, "action", pa.Action)
					a.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
				}
			},
		)
	} else {
		// 非流式路径（默认）：等完整响应后一次性填充队列。
		actions, _, err = generateTacticalPlan(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, logger, hint, capabilityRegistryRef)
		if err == nil {
			a.mu.Lock()
			a.actionQueue = actions
			a.mu.Unlock()
		}
	}

	// 3. LLM 调用结束后的记账（流式/非流式共用）
	a.mu.Lock()
	if err != nil {
		queued := len(a.actionQueue)
		a.mu.Unlock()
		logger.Warn("[战术层] 分解失败，保留已入队 action",
			"agent_id", agentID, "queued", queued, "err", err)
		return false
	}
	isRedecompose := slot == a.currentSlot
	if isRedecompose {
		a.redecomposeCount++
	} else {
		a.currentSlot = slot
		a.currentPlanIndex = idx
		a.redecomposeCount = 0
	}
	queueLen := len(a.actionQueue)
	redecomposeCount := a.redecomposeCount
	queuedActions := append([]plannedAction(nil), a.actionQueue...)
	a.mu.Unlock()

	actionsJSON, _ := json.Marshal(queuedActions)
	logger.Info("[战术层] 队列已填充",
		"agent_id", agentID, "slot", slot, "queue_len", queueLen,
		"redecompose", isRedecompose, "redecompose_count", redecomposeCount,
		"replan_hint", hint, "actions", string(actionsJSON))

	// inner_thought 不再推送 UE（协议未定义 narrative 消息类型）。
	// thought 仍在 tactical.go 的 [战术层] 分解成功 日志中记录，调试可见性保留。

	// 5. 补发：非流式路径总有首 action 要 pop；流式路径若首 action 已在回调中
	// 下发则此处 no-op（队列空或在途 action 占用）。
	a.mu.Lock()
	needFallback := a.currentActionID == "" && len(a.actionQueue) > 0
	a.mu.Unlock()
	if needFallback {
		a.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
	}
	return true
}

// tacticalRefillForReplan 供反应层 replan 决策调用：绕过 currentActionID 守卫，
// 强制重新分解当前时段 goal。规划成功后清空旧队列、写入新队列并 signal worker。
// 不在此处发 stop_action（由调用方 execute() 在规划成功后发）。
// 规划失败返回 false，调用方应保持原 action 不打断。
//
// 与 tacticalRefill 的区别：
//  1. 不检查 currentActionID（允许在途 action 期间规划，这是本函数存在的全部意义）
//  2. 强制重新分解（不受 redecomposeCount ≥2 限制，replan 是反应层显式请求）
//  3. 重置 redecomposeCount = 0（replan 即"重新开始"）
//  4. 通过 replanHint 注入"上次中断原因"到战术层 prompt
func (a *agentContext) tacticalRefillForReplan(
	ctx context.Context, agentID string, ws *wsserver.Server,
	kb *worldkb.KB, logger *slog.Logger, replanHint string,
) bool {
	// 1. 取当前时段 goal（持锁）——不检查 currentActionID
	a.mu.Lock()
	if a.tacticalHc == nil {
		a.mu.Unlock()
		return false
	}
	goal, slot, idx := selectCurrentGoal(a.dailyPlan, a.latestTimeOfDayLocked())
	if goal == "" {
		a.mu.Unlock()
		logger.Warn("[战术层/replan] 无当前时段 goal，无法 replan",
			"agent_id", agentID)
		return false
	}
	zone := a.latestZoneLocked()
	physical := clonePhysical(a.latestPhysical)
	tacticalHc := a.tacticalHc
	kbRef := kb
	hint := replanHint
	a.mu.Unlock()

	// 2. LLM 调用（30s 硬超时，与 tacticalRefill 一致）
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	var actions []plannedAction
	var err error

	if tacticalStreamingEnabled {
		// 流式路径：回调收集到 local slice（不直接修改 a.actionQueue），
		// 成功后才覆盖旧队列。失败则旧队列不受影响。
		var collected []plannedAction
		_, _, err = generateTacticalPlanStreaming(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, logger, hint, capabilityRegistryRef,
			func(pa plannedAction) {
				collected = append(collected, pa)
			},
		)
		if err == nil {
			actions = collected
		}
	} else {
		actions, _, err = generateTacticalPlan(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, logger, hint, capabilityRegistryRef)
	}

	// 3. 失败处理：保留旧队列（不清空），调用方保持原 action
	a.mu.Lock()
	if err != nil {
		queued := len(a.actionQueue)
		a.mu.Unlock()
		logger.Warn("[战术层/replan] 规划失败，保留原队列和原 action",
			"agent_id", agentID, "queued", queued, "err", err)
		return false
	}

	// 4. 成功：原子完成——覆盖旧队列、重置计数、清 hint、signal worker
	a.actionQueue = actions
	a.redecomposeCount = 0
	a.currentSlot = slot
	a.currentPlanIndex = idx
	a.replanHint = ""
	queueLen := len(a.actionQueue)
	queuedActions := append([]plannedAction(nil), a.actionQueue...)
	a.mu.Unlock()

	actionsJSON, _ := json.Marshal(queuedActions)
	logger.Info("[战术层/replan] 重规划成功，新队列已就绪",
		"agent_id", agentID, "slot", slot, "queue_len", queueLen,
		"replan_hint", hint, "actions", string(actionsJSON))

	// 唤醒 worker（execute() 也会再 signal 一次，幂等）
	a.signal()
	return true
}

func main() {
	log.EnableUTF8Console()

	var (
		showVersion        = flag.Bool("version", false, "print version and exit")
		logLevel           = flag.String("log-level", "info", "log level: debug|info|warn|error")
		httpAddr           = flag.String("http", ":8760", "MCP Streamable HTTP addr (empty = stdio)")
		wsAddr             = flag.String("ws", ":9090", "WebSocket server addr for Mock UE")
		hermesURL          = flag.String("hermes-url", "http://localhost:8642", "Hermes Gateway base URL")
		hermesAPIKey       = flag.String("hermes-api-key", "agenttown-test-key", "Hermes Gateway bearer token")
		hermesModel        = flag.String("hermes-model", "deepseek-v4-pro", "Hermes model name")
		mcpAPIKey          = flag.String("mcp-api-key", "", "if set, require this Bearer token on /mcp")
		httpAllowAnyOrigin = flag.Bool("http-allow-any-origin", true,
			"disable origin / localhost restrictions so cross-host clients can connect")
	worldKBPath = flag.String("world-kb", "assets/world_kb.yaml", "path to world_kb.yaml (required, fail-fast on error)")
	worldKBManifest    = flag.String("world-kb-manifest", "assets/world_kb.manifest.json",
		"path to write world_kb.manifest.json (empty skips manifest; written when UE pushes world_kb)")
	tacticalStream = flag.Bool("tactical-stream", false,
		"enable streaming for tactical layer LLM calls (experimental: only helps if upstream LLM emits tokens incrementally)")
	ollamaURL = flag.String("ollama-url", "http://localhost:11434",
		"Ollama base URL for reactive layer (empty disables reactive layer)")
	ollamaModel = flag.String("ollama-model", "qwen2.5:7b-instruct-q4_K_M",
		"Ollama model name for reactive layer decisions")
	ollamaNumThread = flag.Int("ollama-num-thread", 16,
		"CPU threads for Ollama inference (0=use default 16, -1=let Ollama decide). "+
			"CPU inference on high-core-count machines often regresses past ~16 threads; "+
			"benchmark to find the optimum for your host.")
	// ─── 战略层/战术层 LLM backend 切换 ───────────────────────────
	// 默认走 hermes：MCP → Hermes Gateway → 后端模型（由 Hermes config.yaml
	// 决定，当前为 Venus qwen3.6-35b-a3b）。需要直连 Venus 绕过 Hermes 时切 venus。
	llmBackend = flag.String("llm-backend", "hermes",
		"LLM backend for strategic/tactical layers: hermes|venus")
	venusURL = flag.String("venus-url", "http://v2.open.venus.oa.com/llmproxy",
		"Venus LLM proxy base URL (OpenAI Chat Completions API compatible)")
	venusAPIKey = flag.String("venus-api-key", "",
		"Venus API key (overrides VENUS_API_KEY env var)")
	venusModel = flag.String("venus-model", "qwen3.6-35b-a3b",
		"Venus model name")
	venusTimeout = flag.Duration("venus-timeout", 60*time.Second,
		"Venus HTTP timeout per call")
	tacticalTimeout = flag.Duration("tactical-timeout", 60*time.Second,
		"hard timeout for a single tactical-layer LLM call (streaming or not)")
	)
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stderr, version)
		return
	}

	logger := log.New(*logLevel)
	slog.SetDefault(logger)
	tacticalStreamingEnabled = *tacticalStream
	tacticalCallTimeout = *tacticalTimeout
	logger.Info("starting agenttown-mcp",
		"version", version,
		"http", *httpAddr,
		"ws", *wsAddr,
		"llm_backend", *llmBackend,
		"hermes_url", *hermesURL,
		"venus_url", *venusURL,
		"venus_model", *venusModel,
		"tactical_stream", tacticalStreamingEnabled,
		"tactical_timeout", tacticalCallTimeout,
		"ollama_url", *ollamaURL,
		"ollama_model", *ollamaModel,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ─── Build components ──────────────────────────────────────

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "agenttown-mcp",
			Title:   "AgentTown UE Bridge",
			Version: version,
		},
		&mcp.ServerOptions{Logger: logger},
	)

	ws := wsserver.New(wsserver.Options{
		Addr:   *wsAddr,
		Logger: logger,
	})

	// ─── Load World KB (fail-fast) ─────────────────────────────
	// The KB is the semantic→coordinate dictionary shared with Mock UE.
	// Without it, move_to cannot translate semantic targets to coordinates,
	// so a load failure is fatal. Loaded before registering agents so the
	// perception worker can safely close over it.
	//
	// UE pushes an updated KB via the world_kb WebSocket message on connect
	// (handled by worldKBSwap → MergeAndWriteBytes); this Load is the
	// startup seed before UE connects.
	kb, err := worldkb.Load(*worldKBPath)
	if err != nil {
		logger.Error("failed to load world_kb", "path", *worldKBPath, "err", err)
		os.Exit(1)
	}
	logger.Info("world kb loaded",
		"path", *worldKBPath,
		"zones", len(kb.Zones),
		"objects", len(kb.Objects),
		"agents", len(kb.Agents),
	)
	kbRef = kb // expose to /debug/kb handler

	// ─── 反应层 Ollama 客户端 ────────────────────────────────────
	// --ollama-url="" 显式禁用反应层；否则初始化客户端（即使 Ollama 进程
	// 不在跑也不报错——Chat 调用失败时反应层静默降级为 continue）。
	if *ollamaURL != "" {
		ollamaClient := ollama.New(ollama.Options{
			BaseURL: *ollamaURL,
			Model:   *ollamaModel,
			// HTTP client timeout 作为 backstop，必须 > reactiveCallTimeout，
			// 让 context deadline 成为真正的硬截止。否则 HTTP 超时会先于
			// ctx 触发，导致 "Client.Timeout exceeded while awaiting headers"
			// 错误，违背反应层 "ctx 是硬截止" 的设计意图。
			Timeout: reactiveCallTimeout + 5*time.Second,
			// CPU 推理线程数。云开发环境（EPYC 96 vCPU）实测默认 96 线程
			// 反而劣化到 ~8 tok/s，限制到 16 线程可恢复到 ~24 tok/s。
			// -1 表示不传 num_thread，让 Ollama 自决（本地 GPU 场景用）。
			NumThread: *ollamaNumThread,
			Logger:    logger,
		})
		reactiveRunnerRef = newReactiveRunner(ollamaClient, ws, kb, logger)
		logger.Info("reactive layer enabled",
			"ollama_url", ollamaClient.BaseURL(),
			"ollama_model", ollamaClient.Model(),
			"ollama_num_thread", ollamaClient.NumThread(),
		)
	} else {
		logger.Info("reactive layer disabled (--ollama-url=\"\")")
	}

	// Per-agent context (Phase 1: single agent, but keyed for multi-NPC).
	var agentsMu sync.Mutex
	var nextAgentEpoch int64
	// firstAgentRegistered gates the world_kb startup window: once any agent
	// has been registered, subsequent world_kb pushes are rejected (the KB
	// is now in active use by worker goroutines / tools / reactive runner).
	// Guarded by agentsMu.
	var firstAgentRegistered bool
	agents := make(map[string]*agentContext)
	lookupAgent := func(id string) *agentContext {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		return agents[id]
	}
	registerAgent := func(id string) (*agentContext, bool) {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		if ac, ok := agents[id]; ok {
			return ac, false // reconnect: preserve current lifecycle and session
		}
		firstAgentRegistered = true
		nextAgentEpoch++
		ac, workerCtx := newAgentContext(ctx, nextAgentEpoch)
		// 战略层/战术层各用一个独立 LLM client 实例（独立 session 链）。
		// backend 由 --llm-backend 选择：hermes（默认，MCP → Hermes → 后端模型）
		// 或 venus（直连 Venus OpenAI Chat Completions API，绕过 Hermes）。
		switch *llmBackend {
		case "venus":
			venusAPIKeyValue := *venusAPIKey
			if venusAPIKeyValue == "" {
				venusAPIKeyValue = os.Getenv("VENUS_API_KEY")
			}
			ac.strategicHc = venus.New(venus.Config{
				BaseURL: *venusURL,
				APIKey:  venusAPIKeyValue,
				Model:   *venusModel,
				Logger:  logger,
				Timeout: *venusTimeout,
			})
			ac.tacticalHc = venus.New(venus.Config{
				BaseURL: *venusURL,
				APIKey:  venusAPIKeyValue,
				Model:   *venusModel,
				Logger:  logger,
				Timeout: *venusTimeout,
			})
		case "hermes":
			ac.strategicHc = hermes.New(hermes.Config{
				URL:              *hermesURL,
				APIKey:           *hermesAPIKey,
				Model:            *hermesModel,
				Logger:           logger,
				SkipSystemPrompt: true, // 战略层后端调用，不需要 RPG persona/skills/memory 注入
			})
			ac.tacticalHc = hermes.New(hermes.Config{
				URL:              *hermesURL,
				APIKey:           *hermesAPIKey,
				Model:            *hermesModel,
				Logger:           logger,
				SkipSystemPrompt: true, // 战术层后端调用，不需要 RPG persona/skills/memory 注入
			})
		default:
			// flag 校验已在 parse 后完成，此处不应到达；防御性日志。
			logger.Error("unknown llm-backend, falling back to venus", "backend", *llmBackend)
			ac.strategicHc = venus.New(venus.Config{
				BaseURL: *venusURL, APIKey: os.Getenv("VENUS_API_KEY"),
				Model: *venusModel, Logger: logger, Timeout: *venusTimeout,
			})
			ac.tacticalHc = ac.strategicHc
		}
		agents[id] = ac
		go runPerceptionWorker(workerCtx, id, ac, ws, kb, logger)
		return ac, true
	}

	// Capability registry seeded with built-in defaults so the system
	// works even if UE never sends a capability_registry message. UE
	// (e.g. mock_ue) is expected to send one on connect to declare its
	// actually-implemented cmds, overwriting this seed.
	capabilityRegistry := NewCapabilityRegistry()
	capabilityRegistry.Register(protocol.SystemAgentID, BuiltinCmdCapabilities)
	capabilityRegistryRef = capabilityRegistry // expose to tactical worker + debug handler

	// All tools pass through the online/decision-epoch guard before WS send.
	// The executor also gates on per-agent cmd capability (HasCmd).
	executor := &guardedExecutor{ws: ws, lookup: lookupAgent, caps: capabilityRegistry}
	tools.RegisterAll(server, executor, kb, logger)

	// ─── Wire inbound message handler ──────────────────────────
	ws.SetMessageHandler(func(_ context.Context, msgType, agentID string, payload json.RawMessage) {
		switch msgType {
		case protocol.TypeCapabilityRegistry:
			var cr protocol.CapabilityRegistryPayload
			if err := json.Unmarshal(payload, &cr); err != nil {
				logger.Warn("capability_registry parse failed", "err", err, "agent_id", agentID)
				return
			}
			capabilityRegistry.Register(agentID, cr.Actions)
			logger.Info("capability_registry registered",
				"agent_id", agentID, "actions", len(cr.Actions))
			// Reconcile MCP tool list so AddTool/RemoveTools reflects
			// the new global capability set. Per-agent overrides
			// don't change the global tool list — the guardedExecutor
			// enforces per-agent capability at SendAction time.
			tools.ReconcileTools(server, executor, kb, logger,
				capabilityRegistry.EffectiveActions(protocol.SystemAgentID))
		case protocol.TypeWorldKB:
			// UE pushes the full world KB (generated + authored JSON blobs)
			// on connection. MCP merges, persists, and swaps the in-memory
			// KB. Only accepted before the first agent_registered — after
			// that, worker goroutines hold kb pointers and hot-swap would
			// race. Handler holds agentsMu for the full duration so a
			// concurrent agent_registered cannot start workers mid-swap.
			agentsMu.Lock()
			newKB, err := worldKBSwap(firstAgentRegistered, payload, *worldKBPath, *worldKBManifest)
			if err != nil {
				agentsMu.Unlock()
				if errors.Is(err, errAgentWindowClosed) {
					logger.Warn("world_kb rejected: agents already registered, startup window closed",
						"agent_id", agentID)
				} else {
					logger.Error("world_kb merge failed, keeping existing KB",
						"err", err, "path", *worldKBPath)
				}
				return
			}
			kb = newKB
		kbRef = newKB // sync /debug/kb handler
			// Re-register tools so their closures capture the new kb.
			// AddTool is idempotent (replaces same-named tools).
			tools.RegisterAll(server, executor, kb, logger)
			if reactiveRunnerRef != nil {
				reactiveRunnerRef.kb = kb
			}
			agentsMu.Unlock()
			logger.Info("world_kb merged and persisted",
				"path", *worldKBPath,
				"manifest", *worldKBManifest,
				"zones", len(kb.Zones),
				"objects", len(kb.Objects),
				"agents", len(kb.Agents),
			)
		case protocol.TypeAgentRegistered:
			// First registration = new day. Re-registration after reconnect =
			// restore, keep the per-agent strategic/tactical sessions
			// (§4.2: match by agent_id, don't wipe Agent Mind state).
			ac, isNew := registerAgent(agentID)
			if isNew {
				logger.Info("agent_registered (new day)", "agent_id", agentID,
					"agent_epoch", ac.agentEpoch, "payload", string(payload))
			} else {
				logger.Info("agent_registered (reconnect, session kept)", "agent_id", agentID,
					"agent_epoch", ac.agentEpoch)
				// 重连后唤醒 worker：UE 断线期间 wake 不会被消费，
				// 此处 signal 让 worker 重新处理。无队列时 signal 无副作用。
				ac.signal()
			}

		case protocol.TypeAgentUnregistered:
			agentsMu.Lock()
			ac := agents[agentID]
			delete(agents, agentID)
			agentsMu.Unlock()
			if ac != nil {
				ac.stop()
			}
			logger.Info("agent_unregistered", "agent_id", agentID, "perception_queue_cleared", true)

		case protocol.TypeHeartbeat:
			logger.Debug("heartbeat", "agent_id", agentID)

		case protocol.TypeStateReport:
			var sr protocol.StateReportPayload
			if err := json.Unmarshal(payload, &sr); err != nil {
				logger.Warn("state_report parse failed", "err", err)
				return
			}
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("state_report dropped for unregistered agent", "agent_id", agentID)
				return
			}
			trigger, detail := ac.updateState(sr)
			logger.Info("state_report", "agent_id", agentID,
				"energy", sr.PhysicalState.Energy, "fatigue", sr.PhysicalState.Fatigue,
				"joint_wear", sr.PhysicalState.JointWear, "health", sr.PhysicalState.Health)
			if trigger != "" {
				go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
			}

		case protocol.TypeActionCompleted:
			var completed protocol.ActionCompletedPayload
			if err := json.Unmarshal(payload, &completed); err != nil {
				logger.Warn("action_completed parse failed", "err", err)
				return
			}
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("action_completed dropped for unregistered agent", "agent_id", agentID)
				return
			}
			queued, trigger, detail := ac.recordActionCompletion(completed)
			logger.Info("action_completed", "agent_id", agentID,
				"action_id", completed.ActionID, "result", completed.Result,
				"progress", completed.Progress, "decision_queued", queued)
			if trigger != "" {
				go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
			}

		case protocol.TypeEventNotification:
			var event protocol.EventNotificationPayload
			if err := json.Unmarshal(payload, &event); err != nil {
				logger.Warn("event_notification parse failed", "err", err)
				return
			}
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("event_notification dropped for unregistered agent", "agent_id", agentID)
				return
			}
			trigger, detail := ac.recordEventNotification(event)
			logger.Info("event_notification", "agent_id", agentID,
				"event_id", event.EventID, "perception_level", event.PerceptionLevel,
				"trigger", trigger)
			if trigger != "" {
				go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
			}

		case protocol.TypeError:
			logger.Warn("error from mock ue", "agent_id", agentID, "payload", string(payload))

		case protocol.TypePerceptionUpdate:
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("perception_update dropped for unregistered agent", "agent_id", agentID)
				return
			}
			trigger, detail, err := ac.observePerception(payload)
			if err != nil {
				logger.Warn("perception_update parse failed", "agent_id", agentID, "err", err)
				return
			}
			if trigger != "" {
				go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
			}

		default:
			logger.Debug("unhandled message type", "type", msgType, "agent_id", agentID)
		}
	})

	// ─── Start serving ─────────────────────────────────────────
	go func() {
		if err := ws.Serve(ctx); err != nil {
			logger.Error("ws server exited with error", "err", err)
			cancel()
		}
	}()

	if *httpAddr != "" {
		runHTTP(ctx, logger, server, *httpAddr, *httpAllowAnyOrigin, *mcpAPIKey, ws, kb, lookupAgent, registerAgent)
	} else {
		runStdio(ctx, logger, server)
	}
}

// runHTTP serves the MCP server over Streamable HTTP + a /status endpoint.
func runHTTP(ctx context.Context, logger *slog.Logger, server *mcp.Server, addr string, allowAnyOrigin bool, apiKey string, ws *wsserver.Server, kb *worldkb.KB, lookupAgent func(string) *agentContext, registerAgent func(string) (*agentContext, bool)) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"ok":true,"ws_connected":%v}`,
			ws.IsConnected(),
		)))
	})
	// /debug/action — 联调 debug 端点：终端通过 curl 触发 MCP 向 UE 发
	// action_command。仅联调用，无认证；生产环境应禁用或加 Bearer 校验。
	mux.HandleFunc("/debug/action", func(w http.ResponseWriter, r *http.Request) {
		handleDebugAction(ctx, logger, ws, kb, lookupAgent, w, r)
	})
	// /debug/schedule — 联调 debug 端点：给战术层注入一条单行 schedule
	// （如 "07:00-11:00: 车间装配作业"），战术层立即分解成 action 入队下发。
	// 仅联调用，无认证。
	mux.HandleFunc("/debug/schedule", func(w http.ResponseWriter, r *http.Request) {
		handleDebugSchedule(ctx, logger, ws, kb, lookupAgent, registerAgent, w, r)
	})
	// /debug/ — 浏览器 debug 控制台 HTML 页面（无外部依赖，嵌入二进制）。
	mux.HandleFunc("/debug/", func(w http.ResponseWriter, r *http.Request) {
		// 仅 /debug/ 走 UI；/debug/action 已在上面单独注册。
		if r.URL.Path == "/debug/" || r.URL.Path == "/debug" {
			handleDebugUI(w, r)
			return
		}
		// /debug/kb — 返回 world_kb 摘要供前端下拉填充。读 kbRef 而不是
		// 闭包捕获的 kb 参数，确保 worldKBSwap 后返回最新 KB。
		if r.URL.Path == "/debug/kb" {
			handleDebugKB(w, r, kbRef, logger)
			return
		}
		// /debug/cap — 返回 capability_registry 状态供 e2e 黑盒验证
		if r.URL.Path == "/debug/cap" {
			handleDebugCap(w, r, capabilityRegistryRef, logger)
			return
		}
		http.NotFound(w, r)
	})

	err := transport.RunHTTP(ctx, logger, server, transport.HTTPOptions{
		Addr:              addr,
		AllowAnyOrigin:    allowAnyOrigin,
		MCPAPIKey:         apiKey,
		Mux:               mux,
		ReadHeaderTimeout: 5 * time.Second,
		ShutdownTimeout:   5 * time.Second,
	})
	if err != nil {
		logger.Error("HTTP server exited with error", "err", err)
		os.Exit(1)
	}
}

// runStdio serves the MCP server over stdio.
func runStdio(ctx context.Context, logger *slog.Logger, server *mcp.Server) {
	logger.Info("listening on stdio")
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		logger.Error("stdio server exited with error", "err", err)
		os.Exit(1)
	}
}

// ─── /debug/action ──────────────────────────────────────────────
// 联调 debug 端点：终端通过 curl POST 触发 MCP 向 UE 发 action_command。
// 仅联调用，无认证。复用 ws.Call（含 action_started ACK 等待）。
//
// 请求体：{"agent_id":"H-01","cmd":"MoveToLocation","params":{"target":"workbench_01"}}
// cmd 支持：MoveToLocation / Speak / InteractSmartObject / Wait / ChargeAtStation /
// WorkAtWorkbench 等 14 cmd。其中 MoveToLocation 走 kb.GetPosition 解析坐标，
// 其他 cmd 直接透传 params 给 ws.Call。
//
// force（默认 true）：先发 stop_action 停掉战术层正在执行的 idle wait，
// 并短暂挂起 worker dispatch，避免手动 action 被 UE busy 拒。设为 false
// 可测试 busy 拒绝路径。

// debugActionRequest 是 /debug/action 的请求体。
type debugActionRequest struct {
	AgentID string         `json:"agent_id"`
	Cmd     string         `json:"cmd"`
	Params  map[string]any `json:"params"`
	Force   *bool          `json:"force,omitempty"` // nil/true → 强制模式（默认）；false → 直发不 stop
}

// debugActionResponse 是 /debug/action 的响应体。
type debugActionResponse struct {
	OK                   bool    `json:"ok"`
	ActionID             string  `json:"action_id,omitempty"`
	Accepted             bool    `json:"accepted,omitempty"`
	EstimatedDurationSec float64 `json:"estimated_duration_sec,omitempty"`
	Error                string  `json:"error,omitempty"`
}

// debugScheduleRequest 是 /debug/schedule 的请求体。
// schedule 支持两种形态：纯 goal（"车间装配作业"）或带时间段的
// "HH:MM-HH:MM: goal"。时间段可选，用于 prompt 提示步骤总时长。
type debugScheduleRequest struct {
	AgentID  string `json:"agent_id"`
	Schedule string `json:"schedule"`       // 单行，纯 goal 或 "HH:MM-HH:MM: goal"
	Force    *bool  `json:"force,omitempty"` // nil/true → 强制中断当前 action（默认）
}

// debugScheduleResponse 是 /debug/schedule 的响应体。
// dispatched=false 表示 actions 已入 actionQueue，实际下发由 worker 异步完成
// （handler 不自己 pop，避免 UE stop 未处理完时下发被 busy 拒）。
type debugScheduleResponse struct {
	OK           bool            `json:"ok"`
	Slot         string          `json:"slot,omitempty"`
	Goal         string          `json:"goal,omitempty"`
	Actions      []plannedAction `json:"actions,omitempty"`
	QueueLen     int             `json:"queue_len,omitempty"`
	InnerThought string          `json:"inner_thought,omitempty"`
	Dispatched   bool            `json:"dispatched"` // false=已入队异步下发
	Warning      string          `json:"warning,omitempty"` // agent 无感知时非空
	Error        string          `json:"error,omitempty"`
}

// resolveDebugMoveToLocation 把 move_to_location 的参数解析成 ws.Call 需要的完整参数。
// 与 tactical.go 的 move_to_location 分支保持一致：dest + speed。
//
// 支持两种输入模式：
//  1. 直接传坐标：params.dest = [x, y, z]（UE5 cm）。跳过 kb 解析。
//     适用于临时调试未知位置。
//  2. 传 target id：params.target = "workbench_01" / "main_workshop"。
//     走 kb.GetPosition 解析坐标。
//
// 两种模式都没有 → 报错。同时传时 dest 优先（更明确）。
func resolveDebugMoveToLocation(params map[string]any, kb *worldkb.KB) (map[string]any, error) {
	// 模式 1：直接传 dest 坐标
	if dest, ok := params["dest"]; ok && dest != nil {
		coords, err := parseDestCoords(dest)
		if err != nil {
			return nil, fmt.Errorf("parse dest: %w", err)
		}
		speed, _ := params["speed"].(string)
		if speed == "" {
			speed = "walk"
		}
		return map[string]any{
			"dest":  coords,
			"speed": speed,
		}, nil
	}

	// 模式 2：传 target id 走 kb 解析
	target, _ := params["target"].(string)
	if target == "" {
		return nil, errors.New("move_to_location requires params.dest ([x,y,z]) or params.target (kb id)")
	}
	if kb == nil {
		return nil, errors.New("world kb not loaded, cannot resolve target (use params.dest for raw coords)")
	}
	coord, _, err := kb.GetPosition(target)
	if err != nil {
		return nil, fmt.Errorf("resolve target %q: %w", target, err)
	}
	speed, _ := params["speed"].(string)
	if speed == "" {
		speed = "walk"
	}
	return map[string]any{
		"dest":  []float64{coord[0], coord[1], coord[2]},
		"speed": speed,
	}, nil
}

// parseDestCoords 把 params.dest（可能来自 JSON 的 []any / []float64 / []int）
// 规整成 []float64 三元组。校验长度和数值合法性。
func parseDestCoords(v any) ([]float64, error) {
	arr, ok := v.([]any)
	if !ok {
		// JSON 解码后 []float64 也会变成 []any，但保险起见也接受原生类型
		if farr, ok2 := v.([]float64); ok2 {
			arr = make([]any, len(farr))
			for i, f := range farr {
				arr[i] = f
			}
		} else {
			return nil, errors.New("dest must be an array of 3 numbers [x, y, z]")
		}
	}
	if len(arr) != 3 {
		return nil, fmt.Errorf("dest must have exactly 3 elements [x, y, z], got %d", len(arr))
	}
	out := make([]float64, 3)
	for i, e := range arr {
		f, err := toFloat64(e)
		if err != nil {
			return nil, fmt.Errorf("dest[%d] (%v): %w", i, e, err)
		}
		out[i] = f
	}
	return out, nil
}

// toFloat64 把 any 转 float64，支持 JSON 解码后的常见数值类型。
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("not a number: %s", n.String())
		}
		return f, nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%f", &f); err != nil {
			return 0, fmt.Errorf("not a numeric string: %q", n)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}

// mapDebugCmd 把 debug 端点的 cmd 名（tool_name, snake_case）映射到 UE cmd
// （PascalCase protocol 常量）。
//
// registry != nil 时从 EffectiveActions(agentID) 通过 CmdToToolName 反查，
// 覆盖内置 14 cmd 与 UE 通过 capability_registry 新推送的 cmd。
// registry == nil 时降级为 BuiltinToolSpecs 静态查找（向后兼容旧测试）。
// scan_area/stop 没有 UE cmd（RequiredCmd=""），不通过此路径下发。
func mapDebugCmd(cmd string, registry *CapabilityRegistry, agentID string) (protoCmd string, ok bool) {
	if registry != nil {
		for _, act := range registry.EffectiveActions(agentID) {
			if tools.CmdToToolName(act.Cmd) == cmd {
				return act.Cmd, true
			}
		}
		return "", false
	}
	for _, spec := range tools.BuiltinToolSpecs() {
		if spec.Name == cmd && spec.RequiredCmd != "" {
			return spec.RequiredCmd, true
		}
	}
	return "", false
}

// buildDebugParams 根据 cmd 处理 params：move_to_location 走 kb 解析，
// 其他直接透传（composite cmd 不再需要 name 字段，每个 cmd 独立）。
func buildDebugParams(cmd string, params map[string]any, kb *worldkb.KB) (map[string]any, error) {
	if cmd == "move_to_location" {
		return resolveDebugMoveToLocation(params, kb)
	}
	return params, nil
}

func handleDebugAction(ctx context.Context, logger *slog.Logger, ws *wsserver.Server, kb *worldkb.KB, lookupAgent func(string) *agentContext, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "method not allowed, use POST"})
		return
	}

	var req debugActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "invalid JSON body: " + err.Error()})
		return
	}
	if req.AgentID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "agent_id is required"})
		return
	}
	if req.Cmd == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "cmd is required"})
		return
	}

	protoCmd, ok := mapDebugCmd(req.Cmd, capabilityRegistryRef, req.AgentID)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: fmt.Sprintf("unknown cmd: %q", req.Cmd)})
		return
	}

	if !ws.IsConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "no mock ue connected"})
		return
	}

	params, err := buildDebugParams(req.Cmd, req.Params, kb)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: err.Error()})
		return
	}

	// force 默认 true（nil 视为 true）；显式传 false 时直发不 stop。
	force := req.Force == nil || *req.Force

	// 强制模式：暂停 worker dispatch + 发 stop_action 清 UE busy。
	// debugOverride 阻止 worker 在 stop 后的 completion 信号驱动下补 idle wait，
	// 否则手动 action 会被新 idle wait 重新 busy 拒。defer 清除并 signal 恢复。
	var stoppedActionID string
	if force && lookupAgent != nil {
		if ac := lookupAgent(req.AgentID); ac != nil {
			ac.mu.Lock()
			ac.debugOverride = true
			curID := ac.currentActionID
			ac.mu.Unlock()
			defer func() {
				ac.mu.Lock()
				ac.debugOverride = false
				ac.mu.Unlock()
				ac.signal() // 唤醒 worker 恢复正常 dispatch
			}()
			if curID != "" {
				if err := ws.SendStopAction(req.AgentID, curID); err != nil {
					logger.Warn("[debug/action] stop_action failed", "agent_id", req.AgentID, "action_id", curID, "err", err)
				} else {
					stoppedActionID = curID
					// 等 UE 处理 stop（fire-and-forget）并回 action_completed。
					// 100ms 足以让单线程 UE asyncio 跑完 _handle_stop_action；
					// debugOverride 保证期间 worker 不会补 idle wait。
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}

	logger.Info("[debug/action] manual trigger",
		"agent_id", req.AgentID, "cmd", req.Cmd, "proto_cmd", protoCmd, "params", fmt.Sprint(params),
		"force", force, "stopped", stoppedActionID)

	ack, err := ws.Call(ctx, req.AgentID, protoCmd, params)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "ws.Call failed: " + err.Error()})
		return
	}

	resp := debugActionResponse{
		OK:       true,
		ActionID: ack.ActionID,
		Accepted: ack.Accepted,
	}
	if ack.EstimatedDurationSec != nil {
		resp.EstimatedDurationSec = *ack.EstimatedDurationSec
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// parseScheduleText 解析 /debug/schedule 的 schedule 字段，支持两种形态：
//   - 带时间段："HH:MM-HH:MM: 目标描述"（与 dailyPlan 单行格式一致）→ 返回 (slot, goal)
//   - 纯 goal："目标描述"（不含时间段）→ 返回 ("", goal)
//
// 时间段用于战术层 prompt 提示步骤总时长（buildSlotDurationHint），可选。
// 判定逻辑：先尝试用 parseFormattedPlan 解析（它内置 splitPlanRange 校验），
// 解析出恰好 1 条且 slot 非空 → 用其 slot/goal；否则当作纯 goal，slot 留空。
// 调用方需保证输入是单行（多行已在 handler 层拒绝）。
func parseScheduleText(s string) (slot, goal string) {
	items := parseFormattedPlan(s)
	if len(items) == 1 && items[0].Time != "" {
		return items[0].Time, items[0].Goal
	}
	// 纯 goal 形态：整串当作 goal，trim 首尾空白
	return "", strings.TrimSpace(s)
}

// ─── /debug/schedule ─────────────────────────────────────────────
// 联调 debug 端点：给战术层注入一条单行 schedule，战术层立即分解成 3-5 个 action
// 入队，由 worker 异步下发到 UE。仅联调用，无认证。
//
// schedule 支持两种形态：
//   - 带时间段："07:00-11:00: 车间装配作业"（时间段用于 prompt 提示步骤总时长）
//   - 纯 goal："车间装配作业"（时间段可选，不填则 prompt 不提示时长）
//
// 与 /debug/action 的区别：
//   - /debug/action 直接发单个 action_command 到 UE，绕过战术层
//   - /debug/schedule 走战术层 LLM 分解流程，用于调试分解 + 下发全链路
//
// 不覆盖 ac.dailyPlan：goal/slot 直接传给 generateTacticalPlan，仅在 currentSlot
// 加 "__debug__" 前缀避免与 dailyPlan 同时段撞 redecomposeCount 限制。
//
// 互斥：复用 replanInProgress（worker main.go:311 检查后 continue），防止
// handler 调 LLM 期间 worker 并发 tacticalRefill 撞 tacticalHc session。
// debugOverride 叠加设置防止 worker 在 stop→completion 信号驱动下补 idle wait。
func handleDebugSchedule(ctx context.Context, logger *slog.Logger, ws *wsserver.Server, kb *worldkb.KB, lookupAgent func(string) *agentContext, registerAgent func(string) (*agentContext, bool), w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "method not allowed, use POST"})
		return
	}

	var req debugScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "invalid JSON body: " + err.Error()})
		return
	}
	if req.AgentID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "agent_id is required"})
		return
	}
	if req.Schedule == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "schedule is required"})
		return
	}

	// 入口日志：请求到达（字段已校验非空）。分解可能耗时 3-15s，
	// 若 LLM 超时（60s）后续无 decompose ok 日志，可凭此条追踪请求来源。
	force := req.Force == nil || *req.Force
	logger.Info("[debug/schedule] request received",
		"agent_id", req.AgentID, "schedule", req.Schedule, "force", force)

	// schedule 支持两种形态：
	//   (a) 带时间段："HH:MM-HH:MM: 目标描述"（与 dailyPlan 单行格式一致）
	//   (b) 纯 goal："目标描述"（不含时间段，slot 留空）
	// 时间段用于 prompt 提示步骤总时长（引导 LLM 给出总时长接近的步骤），
	// 调试时可选——不填则 prompt 不提示时长，LLM 自行决定步骤数和时长。
	// 多行语义不明（分解哪行？），强制单行。
	if strings.ContainsRune(req.Schedule, '\n') {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{
			Error: "schedule must be a single line",
		})
		return
	}
	slot, goal := parseScheduleText(req.Schedule)
	if goal == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{
			Error: "schedule goal is empty after parsing",
		})
		return
	}

	if !ws.IsConnected() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "no mock ue connected"})
		return
	}

	ac := lookupAgent(req.AgentID)
	if ac == nil {
		// 联调 debug 场景：wscat 手动测试时不会发 agent_registered，
		// 此处自动注册 agent，让 /debug/schedule 能独立工作。
		// 正常仿真流程仍由 UE 的 agent_registered 消息触发注册。
		if registerAgent == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "agent lookup not available"})
			return
		}
		var isNew bool
		ac, isNew = registerAgent(req.AgentID)
		if ac == nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "failed to register agent: " + req.AgentID})
			return
		}
		logger.Info("[debug/schedule] auto-registered agent", "agent_id", req.AgentID, "new", isNew)
	}

	// 互斥：检查 replanInProgress（防 worker 并发 refill 撞 tacticalHc session）；
	// 检查 tacticalHc 是否就绪。设 replanInProgress=true + debugOverride=true。
	ac.mu.Lock()
	if ac.replanInProgress {
		ac.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{
			Error: "another replan/debug in progress, retry later",
		})
		return
	}
	if ac.tacticalHc == nil {
		ac.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "tactical layer not ready"})
		return
	}
	ac.replanInProgress = true
	ac.debugOverride = true
	curID := ac.currentActionID
	ac.mu.Unlock()

	// defer：清互斥 + signal 唤醒 worker（处理入队后的 pop）。
	defer func() {
		ac.mu.Lock()
		ac.replanInProgress = false
		ac.debugOverride = false
		ac.mu.Unlock()
		ac.signal()
	}()

	// 强制中断当前 action：发 stop_action 清 UE busy，等 100ms 让单线程 UE
	// asyncio 跑完 _handle_stop_action。debugOverride 保证期间 worker 不补 idle wait。
	var stoppedActionID string
	if force && curID != "" {
		if err := ws.SendStopAction(req.AgentID, curID); err != nil {
			logger.Warn("[debug/schedule] stop_action failed",
				"agent_id", req.AgentID, "action_id", curID, "err", err)
		} else {
			stoppedActionID = curID
			time.Sleep(100 * time.Millisecond)
		}
	}

	// 取感知上下文 + 清队列 + 清在途记账（让战术层以为空闲，worker pop 不会被守卫拒）。
	ac.mu.Lock()
	zone := ac.latestZoneLocked()
	timeOfDay := ac.latestTimeOfDayLocked()
	physical := clonePhysical(ac.latestPhysical)
	tacticalHc := ac.tacticalHc
	hasPerception := len(ac.latestPerception) > 0
	ac.actionQueue = nil
	ac.currentActionID = ""
	ac.currentActionSrc = ""
	ac.currentActionCmd = ""
	ac.currentActionParams = nil
	ac.currentActionStart = time.Time{}
	ac.mu.Unlock()

	// 调战术层 LLM 分解（非流式，tacticalCallTimeout 60s 硬超时）。
	// 复用 generateTacticalPlan：它不读 dailyPlan，goal/slot 由调用方传入。
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	actions, thought, err := generateTacticalPlan(
		tacticalCtx, tacticalHc, req.AgentID,
		goal, zone, timeOfDay, slot, physical, kb, logger, "", capabilityRegistryRef,
	)
	if err != nil {
		logger.Warn("[debug/schedule] decompose failed",
			"agent_id", req.AgentID, "slot", slot, "goal", goal, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{
			Error: "decompose failed: " + err.Error(),
		})
		return
	}

	// 入队 + 记账。currentSlot 加 "__debug__" 前缀避免与 dailyPlan 同时段撞
	// redecomposeCount >= 1 限制（main.go:696），保证 worker 下次 refill 必走
	// "新时段"重置路径，注入队列执行完后回到 dailyPlan 正轨。
	ac.mu.Lock()
	ac.actionQueue = actions
	ac.currentSlot = "__debug__" + slot
	ac.redecomposeCount = 0
	queueLen := len(actions)
	ac.mu.Unlock()

	logger.Info("[debug/schedule] decompose ok",
		"agent_id", req.AgentID, "slot", slot, "goal", goal,
		"queue_len", queueLen, "thought", thought,
		"stopped", stoppedActionID)
	// 不调 popAndSendQueueAction：依赖 defer signal() 唤醒 worker 走正常 pop 路径
	// （含 currentActionID 守卫 + busy 重试），避免 UE stop 未处理完时下发被拒。

	resp := debugScheduleResponse{
		OK:           true,
		Slot:         slot,
		Goal:         goal,
		Actions:      actions,
		QueueLen:     queueLen,
		InnerThought: thought,
		Dispatched:   false, // worker 异步下发
	}
	if !hasPerception {
		resp.Warning = "agent 未上报感知，zone/timeOfDay/physical 为空，分解质量可能下降"
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Ensure the guarded adapter satisfies the tools.Executor interface.
var _ tools.Executor = (*guardedExecutor)(nil)

// errAgentWindowClosed is returned by worldKBSwap when the startup window
// has already closed (at least one agent_registered processed). Callers
// distinguish this from merge/parse errors to log appropriately.
var errAgentWindowClosed = errors.New("world_kb rejected: startup window closed (agents already registered)")

// worldKBSwap processes a world_kb WS payload: validates the startup window,
// unmarshals the generated+authored blobs, merges them via the worldkb
// pipeline, and persists the result to kbPath (+ manifest if non-empty).
// Returns the new KB for the caller to swap in.
//
// If firstAgentRegistered is true, returns errAgentWindowClosed without
// touching the payload or disk. If the payload is malformed or the merge
// fails, returns the underlying error without writing to disk.
//
// The caller is responsible for the kb pointer swap, tools.RegisterAll
// re-registration, and reactiveRunnerRef.kb assignment — these side
// effects require main()-local state that this pure function does not see.
func worldKBSwap(firstAgentRegistered bool, payload json.RawMessage, kbPath, manifestPath string) (*worldkb.KB, error) {
	if firstAgentRegistered {
		return nil, errAgentWindowClosed
	}
	var wkb protocol.WorldKBPayload
	if err := json.Unmarshal(payload, &wkb); err != nil {
		return nil, fmt.Errorf("parse world_kb payload: %w", err)
	}
	newKB, err := worldkb.MergeAndWriteBytes(wkb.Generated, wkb.Authored, kbPath, manifestPath)
	if err != nil {
		return nil, fmt.Errorf("merge world_kb: %w", err)
	}
	return newKB, nil
}
