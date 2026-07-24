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
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/internal/log"
	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/transport"
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
	currentActionID       string // 当前执行中的 action_id（mu 保护），空表示无执行中动作
	pendingActionTimeouts map[string]*time.Timer // action_id → 超时 timer（mu 保护），约定 §5.2 action_completed 1.5× 估值超时
	dailyPlan             string         // mu 保护，战略层生成的每日计划（格式化字符串），空=未生成或失败
	strategicHc           *hermes.Client // mu 保护，战略层专用 Hermes client（独立 session）
	tacticalHc            *hermes.Client // mu 保护，战术层专用 Hermes client（独立 session）
	actionQueue           []plannedAction // mu 保护，战术层分解出的待执行 action（FIFO）
	currentActionSrc      actionSource    // mu 保护，当前在途 action 的来源（hermes/tactical/空）
	currentPlanIndex      int             // mu 保护，当前执行到 daily_plan 第几个 item（记账用）
	currentSlot           string          // mu 保护，当前分解的时段 "HH:MM-HH:MM"（防同时段重复分解）
	redecomposeCount      int             // mu 保护，当前时段已重复分解次数（防死循环）
	wake                  chan struct{}
	cancel                context.CancelFunc
	stopped               bool
}

func newAgentContext(parent context.Context, epochs ...int64) (*agentContext, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	agentEpoch := int64(1)
	if len(epochs) > 0 {
		agentEpoch = epochs[0]
	}
	return &agentContext{
		online: true, agentEpoch: agentEpoch,
		wake:                  make(chan struct{}, 1), cancel: cancel,
		pendingActionTimeouts: make(map[string]*time.Timer),
	}, ctx
}

// observePerception 存储最新感知payload，供战术层 refill 时读取当前世界状态。
// 反应层移除后：感知不再触发 LLM 决策，仅更新 latestPerception。
func (a *agentContext) observePerception(payload json.RawMessage) error {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("parse perception: %w", err)
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.latestPerception = cloneRawMessage(payload)
	a.mu.Unlock()
	// 感知是 worker 的主驱动源：每次感知到达都唤醒它检查战术队列
	// （pop 下一个 / refill 新时段）。tacticalRefill 内部的守卫避免重复 LLM 调用。
	a.signal()
	return nil
}

// updateState 存储权威的物理/任务状态。反应层移除后：不再触发决策，
// 物理警阈带由下一时段 tacticalRefill 自纠正。
func (a *agentContext) updateState(report protocol.StateReportPayload) {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	physical := report.PhysicalState
	a.latestPhysical = &physical
	a.currentTask = cloneTask(report.CurrentTaskProgress)
	a.mu.Unlock()
}

// recordActionCompletion 处理 action_completed。反应层移除后：所有来源的
// completion 都清在途追踪并 signal worker（pop 下一个或 refill）。
func (a *agentContext) recordActionCompletion(completion protocol.ActionCompletedPayload) bool {
	a.mu.Lock()
	if a.currentTask != nil && a.currentTask.ActionID == completion.ActionID {
		a.currentTask = nil
	}
	if a.currentActionID == completion.ActionID {
		a.currentActionID = ""
	}
	// 取消 action_completed 超时 timer（约定 §5.2）
	if timer, ok := a.pendingActionTimeouts[completion.ActionID]; ok {
		timer.Stop()
		delete(a.pendingActionTimeouts, completion.ActionID)
	}
	a.currentActionSrc = ""
	a.mu.Unlock()
	a.signal()
	return true
}

// recordEventNotification 处理环境事件通知。反应层移除后：环境事件不打断
// 战术队列，仅记录日志（WS handler 已记录），等下一时段 tacticalRefill 自纠正。
func (a *agentContext) recordEventNotification(event protocol.EventNotificationPayload) bool {
	_ = event
	return false
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
	_ = cmd
	_ = params
	a.mu.Lock()
	a.currentActionID = actionID // 追踪当前执行中的 action（约定9 stop_action ID 匹配）
	a.currentActionSrc = src     // 记录来源：completion 时按来源路由（tactical→signal pop 下一个）
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
// 反应层移除后：感知/事件不再触发 LLM 决策，下一时段由 tacticalRefill 自纠正。
func runPerceptionWorker(
	ctx context.Context,
	agentID string,
	ac *agentContext,
	ws *wsserver.Server,
	kb *worldkb.KB,
	logger *slog.Logger,
) {
	// 战略层：进入感知循环前生成当日计划。
	plan := generateDailyPlan(ctx, ac.strategicHc, agentID, logger)
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

		// 在途 action（composite 执行中）时跳过 pop/refill：UE 正忙，pop 出的
		// action 会被 busy 拒，refill 出的队列也会被拒。等 action_completed 自然
		// 唤醒 worker（completion 路径会 signal 并清 currentActionID）。
		if ac.hasInFlightAction() {
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
	ack, err := g.ws.SendAction(ctx, agentID, cmd, params)
	if err == nil && ack != nil {
		ac.recordActionStarted(ack.ActionID, cmd, params, decisionEpoch, sourceHermes)
		// 注册 action_completed 超时 timer（约定 §5.2：estimated_duration × 1.5）
		ac.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, g.ws, agentID, g.lookup)
	}
	return ack, err
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
	if _, err := ws.SendAction(ctx, agentID, protocol.CmdWait, map[string]any{"duration_sec": 60}); err != nil {
		logger.Debug("[战术层] idle wait 发送失败", "agent_id", agentID, "err", err)
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

	cmd, params, err := mapTacticalAction(pa, kb)
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
	// 队列空且已 redecompose ≥2 次才放弃，调用方发 idle wait 等下一时段。
	if slot == a.currentSlot {
		if len(a.actionQueue) > 0 {
			a.mu.Unlock()
			return false // 队列有剩余，等它们执行完再考虑 redecompose
		}
		if a.redecomposeCount >= 2 {
			a.mu.Unlock()
			return false
		}
	}
	zone := a.latestZoneLocked()
	physical := clonePhysical(a.latestPhysical)
	tacticalHc := a.tacticalHc
	kbRef := kb
	// worker 仅在 hasQueueNext=false 时调用 tacticalRefill，队列此时为空。
	// 显式置 nil 以防边界竞态，流式 onAction 回调会逐个 append。
	a.actionQueue = nil
	a.mu.Unlock()

	// 2. 流式调战术层 LLM（不持锁）。onAction 回调：逐个入队 + 首 action 提前下发。
	// 回调在 SendStreaming 的 onDelta 调用栈里同步执行（worker 仍阻塞在 tacticalRefill），
	// 不跨回调持有 mu。
	_, thought, err := generateTacticalPlanStreaming(ctx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), physical, kbRef, logger,
		func(pa plannedAction) {
			a.mu.Lock()
			a.actionQueue = append(a.actionQueue, pa)
			// 首 action 且无在途 action 时立即下发，降低体感延迟。
			shouldDispatch := a.currentActionID == "" && len(a.actionQueue) == 1
			a.mu.Unlock()
			if shouldDispatch {
				logger.Info("[战术层] 流式下发首 action", "agent_id", agentID, "action", pa.Action)
				a.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
			}
		},
	)

	// 3. 流结束后的记账
	a.mu.Lock()
	if err != nil {
		// 流式失败：保留已入队 action（已在途的首 action 正常执行），不更新
		// slot/redecomposeCount（让下一轮可重试）。
		queued := len(a.actionQueue)
		a.mu.Unlock()
		logger.Warn("[战术层] 流式分解失败，保留已入队 action",
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
		"actions", string(actionsJSON))

	// 4. 推送独白（整个时段一次）
	if thought != "" {
		if err := ws.SendEnvelope(agentID, "narrative", map[string]any{"text": thought}); err != nil {
			logger.Debug("[战术层] 独白推送失败", "agent_id", agentID, "err", err)
		}
	}

	// 5. 补发：若流期间未成功 dispatch 首 action（如 send 失败被 signal 掉），
	// 且队列仍有待执行 action 且无在途 action，在此补发。
	a.mu.Lock()
	needFallback := a.currentActionID == "" && len(a.actionQueue) > 0
	a.mu.Unlock()
	if needFallback {
		a.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
	}
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
	)
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stderr, version)
		return
	}

	logger := log.New(*logLevel)
	slog.SetDefault(logger)
	logger.Info("starting agenttown-mcp",
		"version", version,
		"http", *httpAddr,
		"ws", *wsAddr,
		"hermes_url", *hermesURL,
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
	kb, err := worldkb.Load(*worldKBPath)
	if err != nil {
		logger.Error("failed to load world_kb", "path", *worldKBPath, "err", err)
		os.Exit(1)
	}
	logger.Info("world kb loaded",
		"path", *worldKBPath,
		"zones", len(kb.Zones),
		"locations", len(kb.Locations),
		"objects", len(kb.Objects),
		"agents", len(kb.Agents),
	)

	// Per-agent context (Phase 1: single agent, but keyed for multi-NPC).
	var agentsMu sync.Mutex
	var nextAgentEpoch int64
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
		nextAgentEpoch++
		ac, workerCtx := newAgentContext(ctx, nextAgentEpoch)
		ac.strategicHc = hermes.New(hermes.Config{
			URL:    *hermesURL,
			APIKey: *hermesAPIKey,
			Model:  *hermesModel,
			Logger: logger,
		})
		ac.tacticalHc = hermes.New(hermes.Config{
			URL:    *hermesURL,
			APIKey: *hermesAPIKey,
			Model:  *hermesModel,
			Logger: logger,
		})
		agents[id] = ac
		go runPerceptionWorker(workerCtx, id, ac, ws, kb, logger)
		return ac, true
	}

	// All tools pass through the online/decision-epoch guard before WS send.
	executor := &guardedExecutor{ws: ws, lookup: lookupAgent}
	tools.RegisterAll(server, executor, kb, logger)

	// ─── Wire inbound message handler ──────────────────────────
	ws.SetMessageHandler(func(_ context.Context, msgType, agentID string, payload json.RawMessage) {
		switch msgType {
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
			ac.updateState(sr)
			logger.Info("state_report", "agent_id", agentID,
				"energy", sr.PhysicalState.Energy, "fatigue", sr.PhysicalState.Fatigue,
				"joint_wear", sr.PhysicalState.JointWear, "health", sr.PhysicalState.Health)

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
			queued := ac.recordActionCompletion(completed)
			logger.Info("action_completed", "agent_id", agentID,
				"action_id", completed.ActionID, "result", completed.Result,
				"progress", completed.Progress, "decision_queued", queued)

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
			queued := ac.recordEventNotification(event)
			logger.Info("event_notification", "agent_id", agentID,
				"event_id", event.EventID, "perception_level", event.PerceptionLevel,
				"decision_queued", queued)

		case protocol.TypeError:
			logger.Warn("error from mock ue", "agent_id", agentID, "payload", string(payload))

		case protocol.TypePerceptionUpdate:
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("perception_update dropped for unregistered agent", "agent_id", agentID)
				return
			}
			if err := ac.observePerception(payload); err != nil {
				logger.Warn("perception_update parse failed", "agent_id", agentID, "err", err)
				return
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
		runHTTP(ctx, logger, server, *httpAddr, *httpAllowAnyOrigin, *mcpAPIKey, ws)
	} else {
		runStdio(ctx, logger, server)
	}
}

// runHTTP serves the MCP server over Streamable HTTP + a /status endpoint.
func runHTTP(ctx context.Context, logger *slog.Logger, server *mcp.Server, addr string, allowAnyOrigin bool, apiKey string, ws *wsserver.Server) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"ok":true,"ws_connected":%v}`,
			ws.IsConnected(),
		)))
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

// Ensure the guarded adapter satisfies the tools.Executor interface.
var _ tools.Executor = (*guardedExecutor)(nil)
