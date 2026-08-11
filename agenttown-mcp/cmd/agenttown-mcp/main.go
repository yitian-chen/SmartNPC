// Command agenttown-mcp is the MCP server bridging Mock UE to the Venus
// LLM backend for the AgentTown_v3 project.
//
// Three roles in one process:
//
//  1. MCP Server (Streamable HTTP at :8760/mcp) — exposes the game tools
//     to MCP clients (currently used for debug; Hermes Gateway path removed).
//  2. WebSocket Server (:9090/ws) — Mock UE (simulating UE) connects here,
//     pushes protocol messages (perception_update / state_report / ...).
//  3. Venus LLM Client — strategic/tactical layer backend (OpenAI Chat
//     Completions API). Each call is independent (no session chain).
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/internal/log"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/transport"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

var version = "0.1.0-dev"

// agentContext holds per-agent coordination state (lifecycle, concurrency,
// timers) and embeds an *agentstate.AgentState for business state.
//
// Lock discipline (double lock, never nested):
//   - coordMu protects coordination fields: stopped, debugOverride,
//     replanInProgress, pendingActionTimeouts, completedBeforeArm,
//     lastReactiveAt, agentEpoch, online (coordination aspect).
//   - AgentState has its own internal mu protecting business fields.
//   - Callers MUST NOT hold coordMu while calling AgentState methods.
//     Pattern: coord check under coordMu → release → call a.as.Method().
//
// LLM clients (strategicHc/tacticalHc) are process-level shared, not
// per-agent business state, but kept here for worker access. They are
// immutable after construction (no lock needed).
type agentContext struct {
	// as holds all business state (persistent + transient). Access via
	// methods on *agentstate.AgentState, never direct field access.
	as *agentstate.AgentState

	// coordMu protects coordination fields below.
	coordMu               sync.Mutex
	stopped               bool
	debugOverride         bool
	replanInProgress      bool
	pendingActionTimeouts map[string]*time.Timer // action_id → timeout timer
	completedBeforeArm    map[string]struct{}    // action_id completed before timer armed
	agentEpoch            int64
	online                bool // coordination aspect (business online flag is in AgentState)

	// LLM clients (immutable after construction, no lock needed)
	strategicHc llmClient
	tacticalHc  llmClient

	// ollama is the local Ollama client for relationship-update judgments
	// (Stage 5). nil when --ollama-url="" (reactive layer disabled), in
	// which case maybeUpdateRelationship short-circuits. Immutable after
	// construction.
	ollama *ollama.Client

	// Lifecycle (no lock needed — single writer: worker goroutine for cancel,
	// stop() for stopped flag under coordMu)
	wake   chan struct{}
	cancel context.CancelFunc
}

func newAgentContext(parent context.Context, epochs ...int64) (*agentContext, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	agentEpoch := int64(1)
	if len(epochs) > 0 {
		agentEpoch = epochs[0]
	}
	return &agentContext{
		as:                    agentstate.New(),
		online:                true,
		agentEpoch:            agentEpoch,
		wake:                  make(chan struct{}, 1),
		cancel:                cancel,
		pendingActionTimeouts: make(map[string]*time.Timer),
		completedBeforeArm:    make(map[string]struct{}),
	}, ctx
}

// buildStore 构造持久化 Store。空 dsn 返回 NoopStore（内存模式，当前行为，
// 测试和 quick-smoke 无需 MySQL）；非空 dsn 建 MySQLStore（含 schema 迁移）。
// 失败时返回错误，main 调用方据此 os.Exit(1)——MySQL 不可用但用户期望持久化时
// 不应静默降级为内存模式（会导致重启后状态丢失）。
func buildStore(ctx context.Context, dsn string, logger *slog.Logger) (storage.Store, error) {
	if dsn == "" {
		logger.Info("mysql persistence disabled (in-memory mode)")
		return storage.NoopStore{}, nil
	}
	logger.Info("mysql persistence enabled", "dsn_redacted", redactDSN(dsn))
	ms, err := storage.NewMySQLStore(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("init mysql store: %w", err)
	}
	return ms, nil
}

// redactDSN 隐藏 DSN 中的密码字段，日志中只保留 host/db。
func redactDSN(dsn string) string {
	// DSN 格式: user:pass@tcp(host:port)/db?params
	// 简单遮蔽：把第一个 ':' 到 '@' 之间的内容替换为 ***。
	at := strings.IndexByte(dsn, '@')
	colon := strings.IndexByte(dsn, ':')
	if at > 0 && colon >= 0 && colon < at {
		return dsn[:colon+1] + "***" + dsn[at:]
	}
	return dsn
}

// observePerception 存储最新感知payload，供战术层 refill 时读取当前世界状态。
// 反应层：检测 zone 变化 / 新物体出现 / 周期性触发，若显著变化则返回 trigger 信息供
// message handler 异步触发 reactiveRunner.trigger。
func (a *agentContext) observePerception(payload json.RawMessage) (ReactiveTrigger, string, error) {
	// coord check under coordMu (release before calling AgentState)
	a.coordMu.Lock()
	if a.stopped {
		a.coordMu.Unlock()
		return "", "", nil
	}
	a.coordMu.Unlock()

	upd, err := a.as.SetPerception(payload)
	if err != nil {
		return "", "", err
	}
	pCount := upd.PerceptionCount

	// 检测显著变化（zone/新物体）。物理警戒带由 updateState 检测，
	// 这里 prev/cur physical 都用 latestPhysical（即上次 state_report），
	// 不重复检测物理触发。
	trigger, detail := prompt.ShouldTriggerReactive(upd.PrevZone, upd.CurZone, upd.PrevObjectIDs, upd.CurObjectIDs, upd.PrevPhysical, upd.PrevPhysical)
	// 事件类触发优先；无事件时检查周期性触发
	if trigger == "" {
		trigger, detail = prompt.ShouldTriggerPeriodic(pCount)
	}

	// 感知是 worker 的主驱动源：每次感知到达都唤醒它检查战术队列
	// （pop 下一个 / refill 新时段）。tacticalRefill 内部的守卫避免重复 LLM 调用。
	a.signal()
	return trigger, detail, nil
}

// updateState 存储权威的物理/任务状态。反应层：检测物理状态突破警戒带，
// 返回 trigger 信息供 message handler 触发 reactiveRunner。
func (a *agentContext) updateState(report protocol.StateReportPayload) (ReactiveTrigger, string) {
	a.coordMu.Lock()
	if a.stopped {
		a.coordMu.Unlock()
		return "", ""
	}
	a.coordMu.Unlock()

	physical := report.PhysicalState
	prevPhysical := a.as.SetPhysicalState(&physical, cloneTask(report.CurrentTaskProgress))

	// 检测物理警戒带突破（zone/objects 不在此检测，由 observePerception 负责）
	return prompt.ShouldTriggerReactive("", "", nil, nil, prevPhysical, &physical)
}

// recordActionCompletion 处理 action_completed。所有来源的 completion 都清
// 在途追踪并 signal worker（pop 下一个或 refill）。反应层：仅在 action 异常
// 完成（failed/interrupted/error）时触发评估——成功完成是常态，每次都问
// "要不要打断"意义不大（模型看不到战术层整体规划，只能基于贫乏信息答 continue）。
// 异常完成才是真正需要反应层介入的时机。
func (a *agentContext) recordActionCompletion(completion protocol.ActionCompletedPayload) (bool, ReactiveTrigger, string) {
	res := a.as.RecordActionCompletion(completion.ActionID)
	// Stage 4: best-effort action_history recording — only for tracked in-flight
	// actions (debug /debug/action path doesn't call recordActionStarted, so its
	// completions have WasInFlight=false and aren't recorded).
	// Stage 5: 异步触发关系更新判断（Ollama 5s 超时，不阻塞主路径）。
	if res.WasInFlight {
		a.recordActionHistory(completion, res)
		go a.maybeUpdateRelationship(completion, res)
	}
	isSelfStop := res.WasSelfStop

	// 取消 action_completed 超时 timer（约定 §5.2）。
	// 竞态处理：ACK 和 action_completed 可能同一批到达（read loop 顺序处理），
	// completion handler 可能在 SendAction 调用方 armActionTimeout 之前执行。
	// 此时 timer 尚未注册，记录到 completedBeforeArm，让 armActionTimeout 跳过 arm。
	// self-stop 的 action timer 已在 advanceSlotIfNeeded 取消，无需记到
	// completedBeforeArm（否则永不消费，内存泄漏）。
	a.coordMu.Lock()
	if timer, ok := a.pendingActionTimeouts[completion.ActionID]; ok {
		timer.Stop()
		delete(a.pendingActionTimeouts, completion.ActionID)
	} else if !isSelfStop {
		a.completedBeforeArm[completion.ActionID] = struct{}{}
	}
	a.coordMu.Unlock()

	a.signal()
	// 反应层触发：仅异常完成触发。detail 用 result 作为去抖维度（避免每次
	// action_id 不同导致去抖失效），相同 result 在 60s 内不重复触发。
	if completion.Result == protocol.ResultSuccess {
		return true, "", ""
	}
	// self-stop 引发的 interrupted 完成（slot 切换主动 stop）不触发反应层——
	// 这是计划内的打断，replan 会干扰刚下发的新 action。
	if isSelfStop {
		return true, "", ""
	}
	// 异常完成：detail 注入 reaction 层 TriggerDetail，含 UE 给出的 reason
	// （如"寻路不可达"），让 Ollama 看到 UE 侧的具体失败原因再决策。
	detail := fmt.Sprintf("result=%s reason=%s progress=%.2f",
		completion.Result, completion.Reason, completion.Progress)
	return true, TriggerActionDone, detail
}

// recordActionHistory saves a single action_history row at action completion.
// Best-effort: 5s timeout, logs warn on error, never blocks the decision
// pipeline. Uses slog.Default() (agentContext doesn't hold a logger) matching
// the persistSchedule pattern in pkg/agentstate. Skipped in in-memory mode
// (store == nil).
func (a *agentContext) recordActionHistory(completion protocol.ActionCompletedPayload, res agentstate.CompletionResult) {
	store := a.as.Store()
	if store == nil {
		return // in-memory mode
	}
	agentID := a.as.AgentID()
	rec := storage.ActionRecord{
		AgentID:     agentID,
		ActionID:    completion.ActionID,
		Cmd:         res.Cmd,
		Params:      res.Params,
		Source:      string(res.Src),
		StartedAt:   res.Start,
		CompletedAt: time.Now(),
		Result:      completion.Result,
		DurationMs:  int(completion.DurationMs),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.SaveActionRecord(ctx, agentID, rec); err != nil {
		slog.Default().Warn("[action_history] save failed",
			"agent_id", agentID, "action_id", completion.ActionID, "err", err)
	}
}

// loadTacticalMemories fetches top-3 recent memories from the store and
// formats them as a bullet list for the tactical prompt 【过往经验】段.
// Returns "" when store is nil (in-memory mode) or no memories exist.
// Called once per tactical refill (~7 times/game day); indexed query, no
// caching needed.
func (a *agentContext) loadTacticalMemories(ctx context.Context, agentID string) string {
	store := a.as.Store()
	if store == nil {
		return ""
	}
	memories, err := store.LoadRecentMemories(ctx, agentID, 3)
	if err != nil {
		slog.Default().Warn("[记忆层] 加载近期记忆失败",
			"agent_id", agentID, "err", err)
		return ""
	}
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&sb, "- %s（%s）\n", m.Content, m.MemoryType)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// maybeUpdateRelationship judges whether a completed action warrants a
// relationship bump and, if so, updates both directional rows (A→B and B→A).
// Called asynchronously from recordActionCompletion so the 5s Ollama call
// never blocks the main completion path. Best-effort: any error (nil client,
// nil store, Ollama timeout, save failure) logs a warning and gives up.
//
// Stage 5 design: Ollama judges whether the cmd+params constitute a direct
// social interaction with the target agent (yes/no). On "yes", familiarity
// is bumped by 1 in both directions (A→B and B→A); affection is unchanged
// in the initial rule. Self-targeting (target == agentID) is skipped.
func (a *agentContext) maybeUpdateRelationship(completion protocol.ActionCompletedPayload, res agentstate.CompletionResult) {
	if a.ollama == nil {
		return // reactive layer disabled (--ollama-url="")
	}
	store := a.as.Store()
	if store == nil {
		return // in-memory mode
	}
	target, _ := res.Params["target_agent_id"].(string)
	if target == "" {
		return // not an agent-directed action
	}
	agentID := a.as.AgentID()
	if target == agentID {
		return // self-targeting guard
	}
	ctx, cancel := context.WithTimeout(context.Background(), relationshipJudgeTimeout)
	defer cancel()
	if !shouldUpdateRelationship(ctx, a.ollama, res.Cmd, res.Params, target) {
		return
	}
	// 双向更新：A→B + B→A 各一行，各自独立 upsert。affection 初期不动（delta=0）。
	if err := store.SaveRelationship(ctx, agentID, target, 1, 0); err != nil {
		slog.Default().Warn("[关系层] save A→B failed",
			"agent_id", agentID, "target", target, "err", err)
	}
	if err := store.SaveRelationship(ctx, target, agentID, 1, 0); err != nil {
		slog.Default().Warn("[关系层] save B→A failed",
			"agent_id", target, "target", agentID, "err", err)
	}
}

// loadRelationships fetches the agent's relationship rows and formats them
// for the tactical prompt【人际关系】段. Returns "" when store is nil, no
// relationships exist, or the KB has only one agent (single-NPC scenario
// doesn't benefit from a relationship segment). Called once per tactical
// refill; indexed query, no caching needed.
func (a *agentContext) loadRelationships(ctx context.Context, agentID string, kb *worldkb.KB) string {
	store := a.as.Store()
	if store == nil {
		return ""
	}
	// Single-NPC guard: skip when KB has ≤1 agent (no possible relationships).
	if kb == nil || len(kb.Agents) <= 1 {
		return ""
	}
	rels, err := store.LoadRelationships(ctx, agentID, 10)
	if err != nil {
		slog.Default().Warn("[关系层] 加载关系列表失败",
			"agent_id", agentID, "err", err)
		return ""
	}
	return formatRelationshipsForPrompt(rels, agentID)
}

// advanceSlotIfNeeded 检查当前 game_time 是否已超出 currentSlot 结束时间，
// 若是则清队列 + 清 currentSlot + 清在途追踪，让 worker 下一轮走 tacticalRefill
// 自然选新 slot 分解新队列。
//
// 延迟 stop 策略：本方法**不立即发 stop_action**。若旧 action 是长复合动作，
// 把 actionID 记到 pendingStopActionID，由 popAndSendQueueAction 在下发新 action
// 前补发。这样 NPC 在战术层 LLM 调用期间继续旧动作（仍在车间装配/充电），
// 不会愣住。若旧 action 在 LLM 期间自然完成，recordActionCompletion 清除
// pendingStopActionID，popAndSendQueueAction 跳过 stop。
//
// 这是"长复合动作唯一打断路径"：长复合动作不设超时（IsCompositeCmd 跳过
// armActionTimeout），持续执行到时段切换由本方法记录待 stop。
//
// 不在此处调 LLM——只打扫战场，新队列由 worker 下一轮 tacticalRefill 生成。
// 反应层 replan 进行中（replanInProgress=true）时本方法仍可执行：schedule
// 切换优先级高于反应层 replan，清掉的 in-flight 状态不会干扰 replan
// （replan 自己会重新规划，且 replanInProgress 由 replan 路径自己清除）。
func (a *agentContext) advanceSlotIfNeeded(ws *wsserver.Server, agentID string, logger *slog.Logger) {
	// 检查 slot 是否过期（AgentState 内部持锁判断）
	_, slot, _ := a.as.SnapshotSchedule()
	tod := a.as.LatestTimeOfDay()
	if !prompt.SlotExpired(slot, tod) {
		return
	}
	info := a.as.ClearForSlotSwitch()
	actionID := info.ActionID
	actionCmd := info.ActionCmd
	queueLen := info.QueueLen

	// 只对长复合动作记录 pendingStop：短动作 ~100ms 自然完成，会在 LLM 期间
	// 被 recordActionCompletion 清除；若 LLM 极快返回仍发 stop 会触发
	// STOP_ID_MISMATCH（UE 侧短动作不设 busy_action_id）。
	if actionID != "" && isCompositeCmdDynamic(actionCmd, capabilityRegistryRef) {
		a.as.SetPendingStopActionID(actionID)
	}
	// 取消旧 action 的超时 timer（若有）
	if actionID != "" {
		a.coordMu.Lock()
		if timer, ok := a.pendingActionTimeouts[actionID]; ok {
			timer.Stop()
			delete(a.pendingActionTimeouts, actionID)
		}
		a.coordMu.Unlock()
	}

	logger.Info("[战术层] schedule 时段切换，清队列（stop 延迟到分解完成后）",
		"agent_id", agentID, "expired_slot", slot, "game_time", tod,
		"action_id", actionID, "pending_stop", actionID != "" && isCompositeCmdDynamic(actionCmd, capabilityRegistryRef),
		"queue_len", queueLen)

	// 不在此处发 stop_action —— 等 tacticalRefill 分解完成后，
	// 由 popAndSendQueueAction 在下发新 action 前补发 stop。
	// signal worker 下一轮走 tacticalRefill 选新 slot
	a.signal()
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
	a.coordMu.Lock()
	if a.stopped {
		a.coordMu.Unlock()
		return
	}
	a.stopped = true
	a.online = false
	// 停止所有 pending action 超时 timer
	for _, timer := range a.pendingActionTimeouts {
		timer.Stop()
	}
	a.pendingActionTimeouts = make(map[string]*time.Timer)
	cancel := a.cancel
	a.coordMu.Unlock()

	// 清理业务状态（AgentState 内部锁）
	a.as.Stop()
	cancel()
}

func (a *agentContext) recordActionStarted(actionID, cmd string, params map[string]any, decisionEpoch int64, src actionSource) {
	_ = decisionEpoch // 反应层移除后不再记录到 recentActions，保留参数兼容调用方
	// 竞态处理：超短动作（如 Speak, 4ms）的 ACK 和 action_completed 可能在
	// 同一批 WS 消息中到达。read loop 按顺序处理，completion 在本函数之前
	// 执行（SendAction 返回 ACK 后调用方才调本函数）。completion handler
	// 把 actionID 存入 completedBeforeArm，且 RecordActionCompletion 看到
	// currentActionID="" → wasInFlight=false → 不清 currentActionID（本来就是空）。
	// 若此处仍 RecordActionStarted 设置 currentActionID，它将永远不会被清——
	// completion 已经过去了，worker 的 hasInFlightAction() 永远为 true，
	// 队列里后续 action（如长复合动作 work_shift）永远不会被 pop，NPC 卡死。
	//
	// 修复：若 completedBeforeArm 已有此 actionID，动作已完成。跳过
	// RecordActionStarted（不设 currentActionID），signal worker 继续下一个。
	// 保留 completedBeforeArm 条目让 armActionTimeout 消费（它会跳过 arm）。
	a.coordMu.Lock()
	if _, alreadyDone := a.completedBeforeArm[actionID]; alreadyDone {
		a.coordMu.Unlock()
		a.signal()
		return
	}
	a.coordMu.Unlock()

	a.as.RecordActionStarted(actionID, cmd, params, src)

	// TOCTOU 兜底：completion 可能在上面检查和 RecordActionStarted 之间到达。
	// 此时 currentActionID 刚被设置但 completion 已以 wasInFlight=false 跑过。
	// 清掉陈旧的 currentActionID，留 completedBeforeArm 条目给 armActionTimeout。
	a.coordMu.Lock()
	if _, alreadyDone := a.completedBeforeArm[actionID]; alreadyDone {
		a.coordMu.Unlock()
		a.as.ClearInFlightAction(actionID)
		a.signal()
		return
	}
	a.coordMu.Unlock()
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
// 时段切换检测 → pop 队列下一个 action；队列空则 tacticalRefill。
// 长复合动作不设超时，持续执行到时段切换由 advanceSlotIfNeeded 打断。
// 反应层仅 continue/observe/replan：replan 通过 tacticalRefillForReplan
// 重规划并打断在途 action，不打断 worker 正常的 pop/refill 循环。
func runPerceptionWorker(
	ctx context.Context,
	agentID string,
	ac *agentContext,
	ws *wsserver.Server,
	kb *worldkb.KB,
	profiles map[string]*profile.Profile,
	logger *slog.Logger,
) {
	// 战略层：进入感知循环前生成当日计划。
	// 自动规划关闭（--auto-plan=false）时跳过，dailyPlan 保持空，战术层 refill
	// 也不会被调，UE 端不会收到任何自动 action_command。手动模式仅响应
	// /debug/schedule 注入和 /debug/action 下发。
	if autoPlanEnabled {
		plan := generateDailyPlan(ctx, ac.strategicHc, agentID, kb, profiles, capabilityRegistryRef, logger, "")
		// 同步 currentDay：若首条 perception 已到则用其 day_count，否则保持 -1
		// （由 detectDayRollover 在首条 perception 到达时同步）。
		ac.as.SetDailyPlan(plan, ac.as.LatestDayCount())
	} else {
		logger.Info("[自动规划已禁用] 跳过战略层每日计划生成", "agent_id", agentID)
		ac.as.SetDailyPlan("", -1)
	}

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

		// 时段切换检测：game_time 已超出 currentSlot 结束时间 → 打断长复合动作
		// + 清队列 + 清 slot，让本轮后续走 tacticalRefill 选新 slot 重新分解。
		// 这是长复合动作的唯一打断路径（它们不设超时）。
		ac.advanceSlotIfNeeded(ws, agentID, logger)

		// 多日循环：检测 day_count 递增（跨日），重新调用战略层生成新一天的 dailyPlan。
		// 通常在 06:00-07:00 规划窗口内触发（selectCurrentGoal 已屏蔽战术层分解，
		// NPC 仍在执行夜间睡眠 composite）。仅替换 dailyPlan，不打断在途睡眠——
		// 让 NPC 自然睡到 07:00，由 advanceSlotIfNeeded 打断后走 tacticalRefill
		// 选新计划 slot。手动模式（autoPlanEnabled=false）跳过。
		if autoPlanEnabled {
			if rollover, prevDay, newDay := ac.detectDayRollover(); rollover {
				logger.Info("[战略层] 检测到跨日，重新生成当日计划",
					"agent_id", agentID, "prev_day", prevDay, "new_day", newDay)
				// Stage 4: 日终记忆生成——从昨日 action_history 总结出
				// narrative（注入战略层 prompt）+ 结构化 memories（存 DB）。
				// 失败/冷启动返回 ""，generateDailyPlan 内部回退到常量。
				narrative := generateDailyMemories(ctx, ac.strategicHc, ac.as.Store(), agentID, kb, profiles, logger)
				plan := generateDailyPlan(ctx, ac.strategicHc, agentID, kb, profiles, capabilityRegistryRef, logger, narrative)
				// 不清 currentSlot/actionQueue/currentActionID：让 NPC 自然睡眠到 07:00，
				// 由 advanceSlotIfNeeded 打断后走 tacticalRefill 选新计划 slot。
				// currentDay 已由 detectDayRollover 更新为 newDay。
				ac.as.SetDailyPlan(plan, newDay)
			}
		}

		// replan 规划进行中：跳过 pop/refill，等 tacticalRefillForReplan 完成后
		// signal 唤醒。避免规划期间 worker 抢先 pop 旧队列剩余 action 或 refill
		// 覆盖正在生成的新队列。
		ac.coordMu.Lock()
		replanBusy := ac.replanInProgress
		ac.coordMu.Unlock()
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
		// busy，若此刻 worker 被唤醒会立刻补一个新 action 重新占用，导致手动
		// action 被 busy 拒。debugOverride 由 handleDebugAction 设置/清除。
		ac.coordMu.Lock()
		override := ac.debugOverride
		ac.coordMu.Unlock()
		if override {
			continue
		}

	if ac.hasQueueNext() {
		// 队列还有下一个：pop 并直发。
		// 手动模式下 /debug/schedule 注入的 action 进队列后由 ac.signal() 唤醒
		// worker 走这条路径下发，所以 popAndSendQueueAction 不受 autoPlanEnabled 限制。
		ac.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
	} else if autoPlanEnabled {
		// 队列空 + 自动规划开启：尝试战术 refill。refill 返回 false（无 goal /
		// redecomposeCount 达上限）时不主动发任何指令，阻塞等下一次感知唤醒——
		// 感知推进 game_time 后会进入新 slot，redecomposeCount 重置即可重新分解。
		// 不再发 idle wait：长复合动作应持续到时段切换，短动作队列空时让
		// 战术层重新分解（tacticalRefill 内部会在重分解时注入"未安排长动作"hint）。
		ac.tacticalRefill(ctx, agentID, ws, kb, profiles, logger)
	}
	// 队列空 + 自动规划关闭：不主动下发，阻塞在 wake 等手动注入 signal。
	}
}

// extractTimeOfDay 从 perception_update payload 中提取游戏时间，返回 "HH:MM" 格式。
// 按约定 19，UE 推送 environment.time_of_day_sec（当天秒数 0-86400）作为派生字段，
// 这里转为 "HH:MM" 供战术层时段判断（selectCurrentGoal / idleWaitSeconds）与 LLM prompt
// 展示使用。失败或字段非法返回空串。
func extractTimeOfDay(raw json.RawMessage) string {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return formatTodSec(p.Environment.TimeOfDaySec)
}

// formatTodSec 把 time_of_day_sec（0-86400）转为 "HH:MM"。越界返回空串。
func formatTodSec(todSec float64) string {
	if todSec < 0 || todSec >= 86400 {
		return ""
	}
	totalSec := int(todSec)
	hh := totalSec / 3600
	mm := (totalSec % 3600) / 60
	return fmt.Sprintf("%02d:%02d", hh, mm)
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
	ack, err := g.ws.SendAction(ctx, agentID, cmd, params, shouldAutoQueue(cmd))
	if err == nil && ack != nil {
		ac.recordActionStarted(ack.ActionID, cmd, params, decisionEpoch, sourceTool)
		// 长复合动作不设超时：它们持续执行直到下一 schedule 时段切换
		// （advanceSlotIfNeeded 主动 stop + 重规划），自己不会超时。
		// 用动态判断兜底 UE5 新推送的复合 cmd（如 WorkShift/SelfMaintenance）。
		if !isCompositeCmdDynamic(cmd, g.caps) {
			ac.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, g.ws, agentID, g.lookup)
		}
	}
	return ack, err
}

// shouldAutoQueue reports whether the given cmd targets a Smart Object
// that may be occupied by other NPCs.
//
// 2026-08-11 迁移后 auto_queue 作为 params 内字段传（按 UE5 capability_registry
// 声明的类型：ChargeAtStation=string "true"，InteractSmartObject=bool true），
// envelope-level AutoQueue 字段不再使用——保留旧 true 会让 UE5 收到两个
// auto_queue 字段（envelope bool + params string），与 schema 冲突导致 UE5
// 拒绝排队、直接回 action_completed{result:failed, reason:object_occupied_queueable}。
// 这里永远返回 false 让 envelope 字段 omit，auto_queue 仅走 params 路径。
func shouldAutoQueue(cmd string) bool {
	_ = cmd
	return false
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
		actionID = ac.as.CurrentActionID()
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
			// 业务状态清理走 AgentState
			ac.as.RecordActionCompletion(actionID)
			// 协调字段清理走 coordMu
			ac.coordMu.Lock()
			delete(ac.pendingActionTimeouts, actionID)
			ac.coordMu.Unlock()
			// 触发重新决策（下次 perception 会处理）
			select {
			case ac.wake <- struct{}{}:
			default:
			}
		}
	})

	a.coordMu.Lock()
	// 竞态处理：如果 action_completed 已先于 ACK 到达（read loop 同批处理），
	// recordActionCompletion 会把它记到 completedBeforeArm。此时不应 arm timer，
	// 否则永不会被取消，180s 后盲触发 stop_action（STOP_ID_MISMATCH）。
	if _, alreadyDone := a.completedBeforeArm[actionID]; alreadyDone {
		delete(a.completedBeforeArm, actionID)
		a.coordMu.Unlock()
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
	a.coordMu.Unlock()
}

// ─── 战术层队列辅助方法 ────────────────────────────────────────

// hasQueueNext 返回队列是否还有待执行 action。
func (a *agentContext) hasQueueNext() bool {
	return a.as.HasQueueNext()
}

// hasInFlightAction 返回是否有在途 action（已下发未 completion）。
// worker 用它在主循环跳过 pop/refill，避免 UE busy 拒绝循环。
func (a *agentContext) hasInFlightAction() bool {
	return a.as.HasInFlightAction()
}

// queueLen 返回队列长度。
func (a *agentContext) queueLen() int {
	return a.as.QueueLen()
}

// snapshotSchedule 返回当日计划的快照，供 /debug/plan 端点展示当日 schedule。
// plan 是格式化多行字符串，slot 是当前时段 "HH:MM-HH:MM"（或 "__debug__" 前缀），
// idx 是当前执行到第几个 item（-1=未命中）。
func (a *agentContext) snapshotSchedule() (plan string, slot string, idx int) {
	return a.as.SnapshotSchedule()
}

// latestTimeOfDay 从 latestPerception 提取 "HH:MM" 游戏时间。
func (a *agentContext) latestTimeOfDay() string {
	return a.as.LatestTimeOfDay()
}

// detectDayRollover 检测 day_count 递增（跨日），更新 currentDay 并返回是否
// 需要重新调用战略层 generateDailyPlan。
//
// 返回 (rollover, prevDay, newDay)：
//   - rollover=false：同一天（day == prev）或 prev<0 时的首次同步（仅更新
//     currentDay，不触发重新规划——worker 启动时已规划过当天）。
//   - rollover=true：真正的跨日（prev>=0 且 day>prev），调用方应重新调用
//     generateDailyPlan 并替换 dailyPlan。
//
// 重新规划时机选在 06:00-07:00 规划窗口内（selectCurrentGoal 已屏蔽战术层
// 分解），此时 NPC 通常在执行夜间睡眠 composite。detectDayRollover 仅替换
// dailyPlan，**不打断**在途睡眠——让 NPC 自然睡到 07:00，由 advanceSlotIfNeeded
// 打断后走 tacticalRefill 选新计划 slot。这样避免了"06:00 强制打断睡眠后
// NPC 空等 1 小时"的尴尬。
func (a *agentContext) detectDayRollover() (rollover bool, prevDay, newDay int) {
	return a.as.DetectDayRollover()
}

// latestZone 从 latestPerception 提取当前区域 id。
func (a *agentContext) latestZone() string {
	return a.as.LatestZone()
}

// idleWaitSeconds 与 sendIdleWait 已移除：长复合动作持续到时段切换由
// advanceSlotIfNeeded 打断，短动作队列空时由 tacticalRefill 重新分解
// （内部注入"未安排长动作"hint），不再发 idle wait。

// isCompositeCmdDynamic 判断 cmd 是否为长复合动作（不设 action_completed 超时，
// 持续执行到下一 schedule 时段切换由 advanceSlotIfNeeded 主动打断）。
//
// 先查 protocol.IsCompositeCmd 硬编码的 6 个内置复合 cmd（向后兼容），
// 再查 capability_registry 的 Kind 字段兜底——UE5 通过 capability_registry
// 新推送的复合 cmd（如 WorkShift/SelfMaintenance/RestAtResidence/SurfInternet）
// 不在硬编码列表里，但 Kind=="composite"，此处兜底识别。
//
// registry == nil 时退化为仅查硬编码列表（向后兼容旧测试）。
func isCompositeCmdDynamic(cmd string, registry *CapabilityRegistry) bool {
	if protocol.IsCompositeCmd(cmd) {
		return true
	}
	if registry == nil {
		return false
	}
	for _, act := range registry.EffectiveActions("") {
		if act.Cmd == cmd && act.Kind == "composite" {
			return true
		}
	}
	return false
}

// popAndSendQueueAction 从队列 pop 一个 action，映射后直发 ws.SendAction。
// 不经过 MCP 工具 / guardedExecutor（无活跃 decision_epoch）。
// 手动 recordActionStarted + armActionTimeout，source=tactical。
func (a *agentContext) popAndSendQueueAction(ctx context.Context, agentID string,
	ws *wsserver.Server, kb *worldkb.KB, logger *slog.Logger) {

	pa, pendingStop, ok := a.as.PopActionIfIdle()
	if !ok {
		return
	}

	// slot 切换后首次下发：先发 stop 停掉旧复合动作（UE 仍 busy），
	// 再发新 action_command。WS 顺序保证 UE 先处理 stop 清 busy 再处理新 action。
	// 标记 selfStopInProgress：等 stop 引发的 action_completed(interrupted) 到达时
	// 由 recordActionCompletion 抑制反应层触发（slot 切换是计划内，不应 replan）。
	if pendingStop != "" {
		logger.Info("[战术层] slot 切换后补发 stop 再下发新 action",
			"agent_id", agentID, "stop_action_id", pendingStop, "new_action", pa.Action)
		if err := ws.SendStopAction(agentID, pendingStop); err != nil {
			logger.Warn("[战术层] 补发 stop_action 失败",
				"agent_id", agentID, "action_id", pendingStop, "err", err)
		}
		a.as.SetSelfStopInProgress(pendingStop)
	}

	cmd, params, err := mapTacticalAction(pa, agentID, kb, capabilityRegistryRef)
	if err != nil {
		logger.Warn("[战术层] action 映射失败，跳过", "agent_id", agentID, "action", pa.Action, "err", err)
		// 跳过这一个，signal 让 worker 处理下一个（若队列空则触发 refill）
		a.signal()
		return
	}

	logger.Info("[战术层] 下发 action", "agent_id", agentID, "action", pa.Action, "cmd", cmd, "queue_left", a.queueLen())
	ack, err := ws.SendAction(ctx, agentID, cmd, params, shouldAutoQueue(cmd))
	if err != nil {
		// 区分两种失败：
		//   (a) UE 在途 composite 未完成 → 回填队首，等在途 action_completed 唤醒
		//   (b) UE 断线 / 真错误 → 无在途 action 会触发 completion，signal 让
		//       worker 下一轮重试 pop / refill
		if a.as.CurrentActionSrc() == sourceTactical {
			a.as.PrependAction(pa)
			logger.Warn("[战术层] 下发失败（在途 action 占用），回填队首等待 completion",
				"agent_id", agentID, "action", pa.Action, "err", err)
			return
		}
		logger.Warn("[战术层] 下发失败，signal worker 下一轮重试", "agent_id", agentID, "err", err)
		a.signal()
		return
	}
	if ack != nil {
		// 复用现有记账 + 超时机制；source=tactical 让 completion 走队列路径
		a.recordActionStarted(ack.ActionID, cmd, params, 0 /*无 decision_epoch*/, sourceTactical)
		// 长复合动作不设超时：持续执行到下一 schedule 时段切换由
		// advanceSlotIfNeeded 主动打断，自己不会超时。
		// 用动态判断兜底 UE5 新推送的复合 cmd（如 WorkShift/SelfMaintenance）。
		if !isCompositeCmdDynamic(cmd, capabilityRegistryRef) {
			// lookup 返回 a 自身——超时回滚只需清当前 agent 状态
			a.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, ws, agentID, func(string) *agentContext { return a })
		}
	}
}

// tacticalStreamingEnabled controls whether tacticalRefill uses streaming LLM
// calls (generateTacticalPlanStreaming) or non-streaming (generateTacticalPlan).
// Set from --tactical-stream flag. Default false: streaming only helps when the
// upstream LLM emits tokens incrementally; if it buffers the full response
// (DeepSeek peak-hour queueing behavior), streaming adds SSE overhead with no
// latency benefit.
var tacticalStreamingEnabled bool

// autoPlanEnabled 是自动规划总开关，由 --auto-plan flag 解引用设置。
// false 时 worker 跳过战略层 generateDailyPlan / 战术层 tacticalRefill / sendIdleWait，
// WS handler 跳过反应层 trigger 调用。仅 /debug/schedule 和 /debug/action 驱动 action。
// 默认 true 保持现有行为。
var autoPlanEnabled bool

// tacticalCallTimeout 是单次战术层 LLM 调用（流式或非流式）的硬超时。
// 由 --tactical-timeout flag 配置，默认 60s。
// 之前直接用进程 ctx，导致 Venus 后端排队时单次调用最长卡 120s，
// 整个游戏时段空转。超时后由调用方发 idle wait，下一个感知周期重新尝试，
// 比死等更划算。
var tacticalCallTimeout = 60 * time.Second

// llmClient 是战略层/战术层 LLM 客户端的统一接口。
// *venus.Client（Venus 代理，OpenAI Chat Completions 协议）实现此接口。
type llmClient interface {
	SendWithSummary(ctx context.Context, input, summary string) (*llmtypes.Response, error)
	SendStreaming(ctx context.Context, input string, onDelta func(string)) (*llmtypes.Response, error)
	ResetSession()
}

// 编译期断言：venus.Client 满足 llmClient 接口。
var _ llmClient = (*venus.Client)(nil)

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

// ueErrorEntries 是 UE 上报 error 消息的环形缓冲（package-level 供
// /debug/ue-errors handler 引用）。仅保留最近 maxUEErrorEntries 条，
// 供 debug 控制台查看 UE 侧报错（main.go 的 TypeError 分支写入）。
const maxUEErrorEntries = 50

var (
	ueErrorMu      sync.Mutex
	ueErrorEntries []ueErrorEntry
)

// ueErrorEntry 是一条 UE 上报错误记录，供 /debug/ue-errors 端点返回。
type ueErrorEntry struct {
	ReceivedAt time.Time      `json:"received_at"` // wall-clock 收到时间
	AgentID    string         `json:"agent_id"`
	ErrorCode  string         `json:"error_code"`
	Message    string         `json:"message"`
	ActionID   string         `json:"action_id,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
}

// recordUEError 把一条 UE error 消息追加进环形缓冲，超过上限丢弃最旧条目。
func recordUEError(agentID string, ep protocol.ErrorPayload) {
	ueErrorMu.Lock()
	defer ueErrorMu.Unlock()
	ueErrorEntries = append(ueErrorEntries, ueErrorEntry{
		ReceivedAt: time.Now(),
		AgentID:    agentID,
		ErrorCode:  ep.ErrorCode,
		Message:    ep.Message,
		ActionID:   ep.ActionID,
		Context:    ep.Context,
	})
	if len(ueErrorEntries) > maxUEErrorEntries {
		ueErrorEntries = ueErrorEntries[len(ueErrorEntries)-maxUEErrorEntries:]
	}
}

// snapshotUEErrors 返回当前环形缓冲的拷贝（可安全 JSON 编码）。
func snapshotUEErrors() []ueErrorEntry {
	ueErrorMu.Lock()
	defer ueErrorMu.Unlock()
	out := make([]ueErrorEntry, len(ueErrorEntries))
	copy(out, ueErrorEntries)
	return out
}

// tacticalRefill 调战术层 LLM 流式分解当前时段 goal，边接收边入队，
// 首 action 在流式期间即提前下发以降低体感延迟。成功返回 true。
func (a *agentContext) tacticalRefill(ctx context.Context, agentID string,
	ws *wsserver.Server, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) bool {

	// 1. 取当前时段 goal（先读快照，再原子守卫检查+清队列）
	plan, _, _ := a.as.SnapshotSchedule()
	tod := a.as.LatestTimeOfDay()
	goal, slot, idx := selectCurrentGoal(plan, tod)
	prep := a.as.BeginTacticalRefill(goal, slot, idx, a.tacticalHc != nil)
	if prep.ShouldSkip {
		return false
	}
	goal, slot, idx = prep.Goal, prep.Slot, prep.Index
	zone := prep.Zone
	physical := prep.Physical
	hint := prep.Hint
	tacticalHc := a.tacticalHc
	kbRef := kb

	var actions []plannedAction
	var err error

	// Stage 4: 加载近期记忆注入战术层 prompt（top-3 by created_at DESC）。
	memories := a.loadTacticalMemories(ctx, agentID)
	// Stage 5: 加载关系列表注入战术层 prompt【人际关系】段。
	// 单 agent 场景（kb.Agents ≤ 1）返回空串，不污染 prompt。
	relationships := a.loadRelationships(ctx, agentID, kbRef)

	// 战术层 LLM 调用统一 30s 硬超时：避免 Venus 后端排队时单次调用卡 120s
	// 拖死整个游戏时段。超时后调用方发 idle wait，下一感知周期重试。
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	if tacticalStreamingEnabled {
		// 流式路径：onAction 回调逐个入队 + 首 action 提前下发。
		_, _, err = generateTacticalPlanStreaming(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, profiles, logger, hint, memories, relationships, capabilityRegistryRef,
			func(pa plannedAction) {
				a.as.AppendQueueAction(pa)
				if a.as.ShouldDispatchFirst() {
					logger.Info("[战术层] 流式下发首 action", "agent_id", agentID, "action", pa.Action)
					a.popAndSendQueueAction(ctx, agentID, ws, kb, logger)
				}
			},
		)
	} else {
		// 非流式路径（默认）：等完整响应后一次性填充队列。
		actions, _, err = generateTacticalPlan(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, profiles, logger, hint, memories, relationships, capabilityRegistryRef)
		if err == nil {
			a.as.ReplaceQueue(actions)
		}
	}

	// 3. LLM 调用结束后的记账（流式/非流式共用）
	if err != nil {
		queued := a.as.QueueLen()
		logger.Warn("[战术层] 分解失败，保留已入队 action",
			"agent_id", agentID, "queued", queued, "err", err)
		return false
	}
	a.as.CommitTacticalRefill(slot, idx, prep.IsRedecompose)
	queueLen := a.as.QueueLen()
	redecomposeCount := a.as.RedecomposeCountSnapshot()
	queuedActions := a.as.QueueSnapshot()

	actionsJSON, _ := json.Marshal(queuedActions)
	logger.Info("[战术层] 队列已填充",
		"agent_id", agentID, "slot", slot, "queue_len", queueLen,
		"redecompose", prep.IsRedecompose, "redecompose_count", redecomposeCount,
		"replan_hint", hint, "actions", string(actionsJSON))

	// 5. 补发：非流式路径总有首 action 要 pop；流式路径若首 action 已在回调中
	// 下发则此处 no-op（队列空或在途 action 占用）。
	if a.as.NeedFallbackDispatch() {
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
	kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger, replanHint string,
) bool {
	// 1. 取当前时段 goal —— 不检查 currentActionID（replan 允许在途规划）
	if a.tacticalHc == nil {
		return false
	}
	plan, _, _ := a.as.SnapshotSchedule()
	tod := a.as.LatestTimeOfDay()
	goal, slot, idx := selectCurrentGoal(plan, tod)
	if goal == "" {
		logger.Warn("[战术层/replan] 无当前时段 goal，无法 replan",
			"agent_id", agentID)
		return false
	}
	prep := a.as.BeginReplan(goal, slot, idx, a.tacticalHc != nil)
	goal, slot, idx = prep.Goal, prep.Slot, prep.Index
	zone := prep.Zone
	physical := prep.Physical
	tacticalHc := a.tacticalHc
	kbRef := kb
	hint := replanHint

	// 物理告警 goal override：反应层 upgradeIfPhysicalAlert 触发的 replan
	// 含"物理状态告警"标记，此时原 goal（如"车间装配"）应被替换为恢复类
	// goal（如"前往充电站休息"），否则 LLM 仍按原 goal 规划工作动作。
	overrideGoal, isOverride := physicalAlertOverrideGoal(hint, goal, physical)
	if isOverride {
		logger.Info("[战术层/replan] 物理告警 goal override",
			"agent_id", agentID, "orig_goal", goal, "override_goal", overrideGoal, "hint", hint)
		goal = overrideGoal
	}

	// 2. LLM 调用（30s 硬超时，与 tacticalRefill 一致）
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	// Stage 4: 加载近期记忆注入战术层 prompt（top-3 by created_at DESC）。
	memories := a.loadTacticalMemories(ctx, agentID)
	// Stage 5: 加载关系列表注入战术层 prompt【人际关系】段。
	// 单 agent 场景（kb.Agents ≤ 1）返回空串，不污染 prompt。
	relationships := a.loadRelationships(ctx, agentID, kbRef)

	var actions []plannedAction
	var err error

	if tacticalStreamingEnabled {
		// 流式路径：回调收集到 local slice（不直接修改 a.actionQueue），
		// 成功后才覆盖旧队列。失败则旧队列不受影响。
		var collected []plannedAction
		_, _, err = generateTacticalPlanStreaming(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, profiles, logger, hint, memories, relationships, capabilityRegistryRef,
			func(pa plannedAction) {
				collected = append(collected, pa)
			},
		)
		if err == nil {
			actions = collected
		}
	} else {
		actions, _, err = generateTacticalPlan(tacticalCtx, tacticalHc, agentID, goal, zone, a.latestTimeOfDay(), slot, physical, kbRef, profiles, logger, hint, memories, relationships, capabilityRegistryRef)
	}

	// 3. 失败处理：保留旧队列（不清空），调用方保持原 action
	if err != nil {
		queued := a.as.QueueLen()
		logger.Warn("[战术层/replan] 规划失败，保留原队列和原 action",
			"agent_id", agentID, "queued", queued, "err", err)
		return false
	}

	// 4. 成功：原子完成——覆盖旧队列、重置计数、清 hint、signal worker
	a.as.CommitReplan(actions, slot, idx)
	queueLen := a.as.QueueLen()
	queuedActions := a.as.QueueSnapshot()

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
		logLevel           = flag.String("log-level", "debug", "log level: debug|info|warn|error")
		httpAddr           = flag.String("http", ":8760", "MCP Streamable HTTP addr (empty = stdio)")
		wsAddr             = flag.String("ws", ":9090", "WebSocket server addr for Mock UE")
		mcpAPIKey          = flag.String("mcp-api-key", "", "if set, require this Bearer token on /mcp")
		httpAllowAnyOrigin = flag.Bool("http-allow-any-origin", true,
			"disable origin / localhost restrictions so cross-host clients can connect")
	worldKBPath = flag.String("world-kb", "assets/world_kb.yaml", "path to world_kb.yaml (required, fail-fast on error)")
	worldKBManifest    = flag.String("world-kb-manifest", "assets/world_kb.manifest.json",
		"path to write world_kb.manifest.json (empty skips manifest; written when UE pushes world_kb)")
	// profilesDir 指向存放 NPC profile.md 的目录（每个文件名 = agentID.md）。
	// 空串=禁用 profile override，AgentRole 仅走 KB → hardcoded fallback。
	// 启动时一次性加载，UE 推送 world_kb 不触发重载（与 kb 启动时适配一致）。
	profilesDir = flag.String("profiles-dir", "assets/profiles",
		"directory of NPC profile.md files (filename = <agentID>.md; empty disables profile override)")
	tacticalStream = flag.Bool("tactical-stream", false,
		"enable streaming for tactical layer LLM calls (experimental: only helps if upstream LLM emits tokens incrementally)")
	ollamaURL = flag.String("ollama-url", "",
		"Ollama base URL for reactive layer (empty by default disables reactive layer; set to http://localhost:11434 to enable)")
	ollamaModel = flag.String("ollama-model", "qwen2.5:7b-instruct-q4_K_M",
		"Ollama model name for reactive layer decisions")
	ollamaNumThread = flag.Int("ollama-num-thread", 16,
		"CPU threads for Ollama inference (0=use default 16, -1=let Ollama decide). "+
			"CPU inference on high-core-count machines often regresses past ~16 threads; "+
			"benchmark to find the optimum for your host.")
	// ─── 战略层/战术层 LLM backend ───────────────────────────────
	// Venus 直连（OpenAI Chat Completions API），是唯一的战略/战术层后端。
	venusURL = flag.String("venus-url", "http://v2.open.venus.oa.com/llmproxy",
		"Venus LLM proxy base URL (OpenAI Chat Completions API compatible)")
	venusAPIKey = flag.String("venus-api-key", "",
		"Venus API key (overrides VENUS_API_KEY env var)")
	venusModel = flag.String("venus-model", "deepseek-v4-flash",
		"Venus model name (used for tactical layer)")
	venusStrategicModel = flag.String("venus-strategic-model", "deepseek-v4-flash",
		"Venus model name for strategic layer (daily plan generation). "+
			"Set to empty to fall back to --venus-model.")
	venusTimeout = flag.Duration("venus-timeout", 60*time.Second,
		"Venus HTTP timeout per call")
	tacticalTimeout = flag.Duration("tactical-timeout", 60*time.Second,
		"hard timeout for a single tactical-layer LLM call (streaming or not)")
	// autoPlanFlag 是自动规划总开关。关闭（false）时 MCP 进入手动模式：
	// 不调战略层 generateDailyPlan、不调战术层 tacticalRefill、不主动发 idle wait、
	// 不触发反应层 Ollama 决策。仅 /debug/schedule 注入和 /debug/action 手动下发
	// 才会驱动 action。适合联调时隔离 UE 端、单独验证 MCP 行为。
	// 解引用后赋给 package-level autoPlanEnabled（worker / WS handler 读此变量）。
	autoPlanFlag = flag.Bool("auto-plan", true,
		"enable auto planning (strategic + tactical + reactive). false = manual mode, only /debug/schedule and /debug/action drive actions")
	// mysqlDSN 控制 MySQL 持久化层。空串 = 内存模式（NoopStore，当前行为），
	// 非空 = 启用 MySQL 持久化（Stage 3：4 个调度字段 write-through 落盘 +
	// 预埋 Stage 4/5 表骨架）。DSN 需含 parseTime=true 以正确扫描 DATETIME。
	mysqlDSN = flag.String("mysql-dsn", "",
		"MySQL DSN for state persistence (empty = in-memory mode, no persistence). "+
			"Example: user:pass@tcp(127.0.0.1:3306)/agenttown?parseTime=true&charset=utf8mb4")
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
	autoPlanEnabled = *autoPlanFlag

	// ─── Persistence store (Stage 3) ──────────────────────────
	// 空 DSN → NoopStore（内存模式，当前行为）；非空 → MySQLStore（含迁移）。
	// env MYSQL_DSN 作为 flag 空值时的回退（类比 VENUS_API_KEY 模式）。
	dsn := *mysqlDSN
	if dsn == "" {
		dsn = os.Getenv("MYSQL_DSN")
	}
	store, err := buildStore(context.Background(), dsn, logger)
	if err != nil {
		logger.Error("init storage failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()
	logger.Info("starting agenttown-mcp",
		"version", version,
		"http", *httpAddr,
		"ws", *wsAddr,
		"venus_url", *venusURL,
		"venus_model", *venusModel,
		"venus_strategic_model", *venusStrategicModel,
		"tactical_stream", tacticalStreamingEnabled,
		"tactical_timeout", tacticalCallTimeout,
		"ollama_url", *ollamaURL,
		"ollama_model", *ollamaModel,
		"auto_plan", autoPlanEnabled,
		"mysql_persistence", dsn != "",
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

	// ─── NPC profile.md 加载 ─────────────────────────────────────
	// 扫描 *profilesDir 下 *.md，按文件名映射 agentID → Profile。空目录或
	// 空串 → profiles=nil，AgentRole 仅走 KB → fallback，行为不变。
	// profiles 是进程级只读 map（启动加载，UE 推 world_kb 不影响），与 kb
	// 同级参数化传递，不入 agentContext。
	var profiles map[string]*profile.Profile
	if *profilesDir != "" {
		profiles, err = profile.LoadDir(*profilesDir)
		if err != nil {
			logger.Error("failed to load profiles dir", "path", *profilesDir, "err", err)
			os.Exit(1)
		}
	}
	logger.Info("profiles loaded",
		"path", *profilesDir,
		"count", len(profiles),
	)

	// ─── 反应层 Ollama 客户端 ────────────────────────────────────
	// --ollama-url="" 显式禁用反应层；否则初始化客户端（即使 Ollama 进程
	// 不在跑也不报错——Chat 调用失败时反应层静默降级为 continue）。
	// ollamaClient 提升到外层作用域，供 registerAgent 闭包捕获注入
	// agentContext.ollama（Stage 5 关系层判断用）。nil 表示禁用。
	var ollamaClient *ollama.Client
	if *ollamaURL != "" {
		ollamaClient = ollama.New(ollama.Options{
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
		reactiveRunnerRef = newReactiveRunner(ollamaClient, ws, kb, profiles, logger)
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
	// listAgentIDs 返回当前已注册的全部 agent ID（按字典序），供 debug 端点做默认值
	// 选择（如 /debug/plan 在未显式指定 agent_id 时回落到首个注册 agent，而非硬编码）。
	listAgentIDs := func() []string {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		ids := make([]string, 0, len(agents))
		for id := range agents {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
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
		// Stage 3: 绑定持久化身份 + 从 DB 恢复 4 个调度字段。
		// store==nil（内存模式）时 SetIdentity 仍记录 agentID 但持久化调用跳过；
		// LoadPersistent 在 NoopStore 下返回 ErrNotFound→保持默认值（cold start）。
		ac.as.SetIdentity(id, store)
		if err := ac.as.LoadPersistent(ctx); err != nil {
			logger.Warn("[main] load persistent state failed, continuing with cold start",
				"agent_id", id, "err", err)
		}
		// Stage 5: 从 KB 种子导入关系到 DB（冷启动，INSERT IGNORE 不覆盖
		// 既有交互累积）。重连走 else 分支（:1344）提前 return，不会重复导入。
		if store != nil {
			if err := seedRelationshipsFromKB(ctx, kb, store, id, logger); err != nil {
				logger.Warn("[关系层] KB 种子导入失败", "agent_id", id, "err", err)
			}
		}
		// 战略层/战术层各用一个独立 LLM client 实例。
		// Venus 直连（OpenAI Chat Completions API），是唯一的战略/战术层后端。
		venusAPIKeyValue := *venusAPIKey
		if venusAPIKeyValue == "" {
			venusAPIKeyValue = os.Getenv("VENUS_API_KEY")
		}
		// 战略层 model：优先 --venus-strategic-model，空值回退到 --venus-model。
		strategicModel := *venusStrategicModel
		if strategicModel == "" {
			strategicModel = *venusModel
		}
		ac.strategicHc = venus.New(venus.Config{
			BaseURL: *venusURL,
			APIKey:  venusAPIKeyValue,
			Model:   strategicModel,
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
	// Stage 5: 注入 Ollama 客户端供关系层判断。nil 表示 --ollama-url=""
	// 禁用反应层时，maybeUpdateRelationship 会早返回不调用 Ollama。
	ac.ollama = ollamaClient
	agents[id] = ac
		go runPerceptionWorker(workerCtx, id, ac, ws, kb, profiles, logger)
		return ac, true
	}

	// Capability registry seeded with built-in defaults so the system
	// works even if UE never sends a capability_registry message. UE
	// (e.g. mock_ue) is expected to send one on connect to declare its
	// actually-implemented cmds, overwriting this seed.
	capabilityRegistry := NewCapabilityRegistry(logger)
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
			newKB, normalizeChanges, err := worldKBSwap(firstAgentRegistered, payload, *worldKBPath, *worldKBManifest)
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
			if len(normalizeChanges) > 0 {
				logger.Info("world_kb entity ids normalized to lowercase",
					"changes", normalizeChanges)
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
			if trigger != "" && autoPlanEnabled {
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
			"reason", completed.Reason, "progress", completed.Progress,
			"decision_queued", queued)
		if trigger != "" && autoPlanEnabled {
			go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
		}

		case protocol.TypeActionQueued:
			var aq protocol.ActionQueuedPayload
			if err := json.Unmarshal(payload, &aq); err != nil {
				logger.Warn("action_queued parse failed", "err", err)
				return
			}
			ac := lookupAgent(agentID)
			if ac == nil {
				logger.Warn("action_queued dropped for unregistered agent", "agent_id", agentID)
				return
			}
			ac.as.RecordQueueStatus(aq)
			logger.Info("action_queued", "agent_id", agentID,
				"action_id", aq.ActionID, "status", aq.Status,
				"group", aq.Group, "position", aq.Position)
			// 不触发反应层：queued 是信息性状态，下次 periodic/action_done 等
			// 触发时反应层 prompt 的【排队状态】段会带上。timeout 后 UE 会补
			// action_completed{failed, queue_timeout}，走现有 TriggerActionDone 路径。

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
			if trigger != "" && autoPlanEnabled {
				go reactiveRunnerRef.trigger(agentID, ac, trigger, detail)
			}

		case protocol.TypeError:
			var ep protocol.ErrorPayload
			if err := json.Unmarshal(payload, &ep); err != nil {
				logger.Warn("error from ue (payload parse failed)",
					"agent_id", agentID, "raw", string(payload), "err", err)
			} else {
				logger.Warn("error from ue",
					"agent_id", agentID,
					"error_code", ep.ErrorCode, "message", ep.Message,
					"action_id", ep.ActionID)
				recordUEError(agentID, ep)
			}

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
			if trigger != "" && autoPlanEnabled {
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
		runHTTP(ctx, logger, server, *httpAddr, *httpAllowAnyOrigin, *mcpAPIKey, ws, kb, lookupAgent, listAgentIDs, registerAgent)
	} else {
		runStdio(ctx, logger, server)
	}
}

// runHTTP serves the MCP server over Streamable HTTP + a /status endpoint.
func runHTTP(ctx context.Context, logger *slog.Logger, server *mcp.Server, addr string, allowAnyOrigin bool, apiKey string, ws *wsserver.Server, kb *worldkb.KB, lookupAgent func(string) *agentContext, listAgentIDs func() []string, registerAgent func(string) (*agentContext, bool)) {
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
		// /debug/ue-errors — 返回最近 UE 上报的 error 消息（环形缓冲，最多 50 条）
		if r.URL.Path == "/debug/ue-errors" {
			handleDebugUEErrors(w, r, logger)
			return
		}
		// /debug/logs — 返回最近 MCP 日志条目（环形缓冲，最多 500 条）
		if r.URL.Path == "/debug/logs" {
			handleDebugLogs(w, r, logger)
			return
		}
		// /debug/plan — 返回当日 dailyPlan（战略层生成的 7 时段 goal），
		// 供 debug 控制台右侧 schedule 面板展示。读 per-agent agentContext。
		if r.URL.Path == "/debug/plan" {
			handleDebugPlan(w, r, lookupAgent, listAgentIDs, logger)
			return
		}
		// /debug/agents — 返回已注册 agent ID 列表，供前端填充 agent 下拉。
		// 数据源是 agents map（agent_registered 写入），不是 capability_registry。
		if r.URL.Path == "/debug/agents" {
			handleDebugAgents(w, r, listAgentIDs, logger)
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

// mapDebugCmd 把 debug 端点的 cmd 名（tool_name, snake_case）映射到 UE cmd
// （PascalCase protocol 常量）。
//
// registry != nil 时从 EffectiveActions(agentID) 通过 CmdToToolName 反查，
// 覆盖内置 12 cmd 与 UE 通过 capability_registry 新推送的 cmd。
// registry == nil 时降级为 BuiltinToolSpecs 静态查找（向后兼容旧测试）。
// scan_area/stop 没有 UE cmd（RequiredCmd=""），不通过此路径下发。
//
// 兼容两种 cmd 形式：
//   - raw cmd（PascalCase，如 "MoveTo"）：与 capability_registry 中的 Cmd 字段一致
//   - tool_name（snake_case，如 "move_to"）：与 MCP 工具名一致，旧版硬编码下拉用此形式
//
// 优先匹配 raw cmd；若无再匹配 tool_name，避免歧义（理论上两者不会碰撞：
// raw cmd 必含大写字母或为单词，tool_name 必含下划线或全小写）。
func mapDebugCmd(cmd string, registry *CapabilityRegistry, agentID string) (protoCmd string, ok bool) {
	if registry != nil {
		for _, act := range registry.EffectiveActions(agentID) {
			if act.Cmd == cmd {
				return act.Cmd, true
			}
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

// buildDebugParams 根据 cmd 处理 params。新 12 cmd 体系下 MoveTo 由 UE 自己
// 解析 target_type+target_id/target_position，MCP 不再做 KB 坐标解析，所有
// cmd 的 params 直接透传。
func buildDebugParams(cmd string, params map[string]any, kb *worldkb.KB) (map[string]any, error) {
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

	// stop 是合成 cmd：不发 action_command，而是发 stop_action 控制消息停止
	// 当前在途动作。始终可用，不依赖 capability_registry 注册（也不进
	// EffectiveActions，避免污染战术层 prompt 与 ReconcileTools）。前端下拉
	// 始终展示此项（handleDebugCap 注入合成 Stop 能力项）。
	if req.Cmd == "stop" || req.Cmd == "Stop" {
		if !ws.IsConnected() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "no ue connected"})
			return
		}
		if err := ws.SendStopAction(req.AgentID, ""); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "stop_action failed: " + err.Error()})
			return
		}
		logger.Info("[debug/action] manual stop",
			"agent_id", req.AgentID, "cmd", req.Cmd)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(debugActionResponse{OK: true, Accepted: true})
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
		_ = json.NewEncoder(w).Encode(debugActionResponse{Error: "no ue connected"})
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
			ac.coordMu.Lock()
			ac.debugOverride = true
			curID := ac.as.CurrentActionID()
			ac.coordMu.Unlock()
			defer func() {
				ac.coordMu.Lock()
				ac.debugOverride = false
				ac.coordMu.Unlock()
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

	// Debug path: auto_queue=false (manual debug doesn't queue; tester
	// wants to see the immediate rejected/accepted behavior).
	ack, err := ws.Call(ctx, req.AgentID, protoCmd, params, false)
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
// 联调 debug 端点：给战术层注入一条单行 schedule，战术层立即分解成 1-5 个 action
// 入队（复合优先：匹配复合动作时 1-2 步，否则 2-5 个原子动作），由 worker 异步下发到 UE。仅联调用，无认证。
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
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "no ue connected"})
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
	ac.coordMu.Lock()
	if ac.replanInProgress {
		ac.coordMu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{
			Error: "another replan/debug in progress, retry later",
		})
		return
	}
	if ac.tacticalHc == nil {
		ac.coordMu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(debugScheduleResponse{Error: "tactical layer not ready"})
		return
	}
	ac.replanInProgress = true
	ac.debugOverride = true
	curID := ac.as.CurrentActionID()
	ac.coordMu.Unlock()

	// defer：清互斥 + signal 唤醒 worker（处理入队后的 pop）。
	defer func() {
		ac.coordMu.Lock()
		ac.replanInProgress = false
		ac.debugOverride = false
		ac.coordMu.Unlock()
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
	snap := ac.as.Snapshot()
	zone := snap.LatestZone()
	timeOfDay := snap.LatestTimeOfDay()
	physical := snap.LatestPhysical
	tacticalHc := ac.tacticalHc
	hasPerception := len(snap.LatestPerception) > 0
	ac.as.ClearForSlotSwitch()

	// 调战术层 LLM 分解（非流式，tacticalCallTimeout 60s 硬超时）。
	// 复用 generateTacticalPlan：它不读 dailyPlan，goal/slot 由调用方传入。
	tacticalCtx, tacticalCancel := context.WithTimeout(ctx, tacticalCallTimeout)
	defer tacticalCancel()

	actions, thought, err := generateTacticalPlan(
		tacticalCtx, tacticalHc, req.AgentID,
		goal, zone, timeOfDay, slot, physical, kb, nil, logger, "", "", "", capabilityRegistryRef,
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
	// redecomposeCount >= 1 限制，保证 worker 下次 refill 必走"新时段"重置路径，
	// 注入队列执行完后回到 dailyPlan 正轨。
	ac.as.RefillQueue(actions, "__debug__"+slot)
	ac.as.ResetRedecomposeCount()
	queueLen := len(actions)

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
func worldKBSwap(firstAgentRegistered bool, payload json.RawMessage, kbPath, manifestPath string) (*worldkb.KB, []string, error) {
	if firstAgentRegistered {
		return nil, nil, errAgentWindowClosed
	}
	var wkb protocol.WorldKBPayload
	if err := json.Unmarshal(payload, &wkb); err != nil {
		return nil, nil, fmt.Errorf("parse world_kb payload: %w", err)
	}
	newKB, changes, err := worldkb.MergeAndWriteBytes(wkb.Generated, wkb.Authored, kbPath, manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("merge world_kb: %w", err)
	}
	return newKB, changes, nil
}
