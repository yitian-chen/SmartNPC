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

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/perception"
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
	mu                       sync.Mutex
	online                   bool
	agentEpoch               int64
	decisionEpoch            int64
	decisionActive           bool
	activeScanFollowup       bool
	pendingScanID            string
	pendingScanFollowup      bool
	latestPhysical           *protocol.PhysicalState
	latestPerception         json.RawMessage
	currentTask              *protocol.CurrentTaskProgress
	observedSnapshot         *observedSnapshot
	pendingPerception        json.RawMessage
	pendingReasons           []string
	recentEnvironmentEvents  []string // pending extras for the next decision
	summaryEnvironmentEvents []string // rolling authoritative environment history
	recentActions            []localActionSummary
	currentActionID          string // 当前执行中的 action_id（mu 保护），空表示无执行中动作；用于 stop_action ID 匹配（约定9）
	pendingActionTimeouts    map[string]*time.Timer // action_id → 超时 timer（mu 保护），约定 §5.2 action_completed 1.5× 估值超时
	llmFailures              int  // 连续 LLM 失败次数（mu 保护），约定 §5.3 安全模式
	inSafeMode               bool // 是否处于安全模式（mu 保护），约定 §5.3
	dailyPlan                string        // mu 保护，战略层生成的每日计划（格式化字符串），空=未生成或失败
	lastPlanSlot             string        // mu 保护，上次注入完整计划时所在的时段（"HH:MM-HH:MM"），用于按需注入
	strategicHc              *hermes.Client // mu 保护，战略层专用 Hermes client（独立 session，不污染决策链）
	wake                     chan struct{}
	cancel                   context.CancelFunc
	stopped                  bool
}

type decisionWork struct {
	perception   json.RawMessage
	physical     *protocol.PhysicalState
	currentTask  *protocol.CurrentTaskProgress
	reasons      []string
	extras       []string
	localSummary string
	scanFollowup bool
	dailyPlan    string
	timeOfDay    string // 从 perception 提取的 "HH:MM"，用于按需注入计划
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

// observePerception stores every valid payload as the latest world state. A
// decision is queued only when the comparable snapshot crosses a gate. If a
// decision is already pending, even a non-triggering update replaces its
// payload so the worker always receives the newest perception.
func (a *agentContext) observePerception(payload json.RawMessage) ([]string, bool, error) {
	perceptionPayload, snapshot, err := parsePerceptionSnapshot(payload)
	if err != nil {
		return nil, false, err
	}

	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil, false, nil
	}
	reasons := perceptionTriggerReasons(perceptionPayload, snapshot, a.observedSnapshot)
	matchedScanResponse := perceptionPayload.ScanID != "" && perceptionPayload.ScanID == a.pendingScanID
	if matchedScanResponse {
		// Consume the token and mark a scan-followup decision as pending so
		// the perception worker starts a fresh turn once the current turn
		// finishes. We deliberately do NOT clear decisionActive here: doing
		// so would reject subsequent tool_calls that the LLM may issue in
		// the same response (e.g., scan_area → move_to), which used to
		// trigger Hermes' circuit breaker after 3 stale rejections. The
		// current turn keeps running with its original epoch; the new
		// epoch is bumped only when runPerceptionWorker calls
		// beginDecisionWithScan for the follow-up turn.
		a.pendingScanID = "" // consume once: duplicate scan responses are ordinary snapshots
		a.pendingScanFollowup = true
		reasons = mergeUnique(reasons, reasonScanResponse)
	}
	a.observedSnapshot = &snapshot
	a.latestPerception = cloneRawMessage(payload)
	if containsReason(reasons, reasonAudibleEvent) {
		events := audibleEventExtras(perceptionPayload.AudibleEvents)
		a.recentEnvironmentEvents = append(a.recentEnvironmentEvents, events...)
		for _, event := range events {
			a.summaryEnvironmentEvents = appendRolling(a.summaryEnvironmentEvents, truncateText(event, 256), 8)
		}
	}
	replaced := a.pendingPerception != nil
	if len(reasons) > 0 {
		a.pendingReasons = mergeUnique(a.pendingReasons, reasons...)
	}
	shouldWake := len(a.pendingReasons) > 0
	if shouldWake {
		a.pendingPerception = cloneRawMessage(a.latestPerception)
	}
	a.mu.Unlock()
	if shouldWake {
		a.signal()
	}
	return reasons, replaced, nil
}

// updateState stores authoritative physical/task state and queues a decision
// only for task lifecycle changes or physical alert-band crossings.
func (a *agentContext) updateState(report protocol.StateReportPayload) []string {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}

	reasons := taskLifecycleReasons(a.currentTask, report.CurrentTaskProgress)
	if a.latestPhysical != nil {
		reasons = append(reasons, physicalCrossingReasons(*a.latestPhysical, report.PhysicalState)...)
	}
	physical := report.PhysicalState
	a.latestPhysical = &physical
	a.currentTask = cloneTask(report.CurrentTaskProgress)
	a.queueDecisionLocked(reasons, nil)
	a.mu.Unlock()
	if len(reasons) > 0 {
		a.signal()
	}
	return reasons
}

func (a *agentContext) recordActionCompletion(completion protocol.ActionCompletedPayload) bool {
	details, _ := json.Marshal(completion.Details)
	extra := fmt.Sprintf("动作完成 action_id=%s result=%s progress=%g details=%s",
		completion.ActionID, completion.Result, completion.Progress, details)
	reason := fmt.Sprintf("动作完成:%s", completion.ActionID)
	a.mu.Lock()
	updated := false
	for i := len(a.recentActions) - 1; i >= 0; i-- {
		if a.recentActions[i].ActionID == completion.ActionID {
			a.recentActions[i].Result = completion.Result
			a.recentActions[i].DurationMs = completion.DurationMs
			a.recentActions[i].Progress = completion.Progress
			updated = true
			break
		}
	}
	if !updated {
		a.recentActions = appendRolling(a.recentActions, localActionSummary{
			ActionID: completion.ActionID, Result: completion.Result,
			DurationMs: completion.DurationMs, Progress: completion.Progress,
		}, 8)
	}
	if a.currentTask != nil && a.currentTask.ActionID == completion.ActionID {
		a.currentTask = nil
	}
	if a.currentActionID == completion.ActionID {
		a.currentActionID = "" // 动作完成，清空当前 action 追踪
	}
	// 取消 action_completed 超时 timer（约定 §5.2）
	if timer, ok := a.pendingActionTimeouts[completion.ActionID]; ok {
		timer.Stop()
		delete(a.pendingActionTimeouts, completion.ActionID)
	}
	a.mu.Unlock()
	return a.queueExternalEvent(reason, extra)
}

func (a *agentContext) recordEventNotification(event protocol.EventNotificationPayload) bool {
	details, _ := json.Marshal(event.Event)
	extra := fmt.Sprintf("环境事件 event_id=%s perception_level=%s event=%s",
		event.EventID, event.PerceptionLevel, details)
	reason := fmt.Sprintf("事件通知:%s", event.EventID)
	a.mu.Lock()
	a.summaryEnvironmentEvents = appendRolling(a.summaryEnvironmentEvents, truncateText(extra, 256), 8)
	a.mu.Unlock()
	return a.queueExternalEvent(reason, extra)
}

func (a *agentContext) queueExternalEvent(reason, extra string) bool {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return false
	}
	a.queueDecisionLocked([]string{reason}, []string{extra})
	queued := a.pendingPerception != nil
	a.mu.Unlock()
	if queued {
		a.signal()
	}
	return queued
}

func (a *agentContext) queueDecisionLocked(reasons, extras []string) {
	a.pendingReasons = mergeUnique(a.pendingReasons, reasons...)
	for _, extra := range extras {
		if extra != "" {
			a.recentEnvironmentEvents = append(a.recentEnvironmentEvents, extra)
		}
	}
	if len(a.pendingReasons) > 0 && a.latestPerception != nil {
		a.pendingPerception = cloneRawMessage(a.latestPerception)
	}
}

func (a *agentContext) takeDecision() *decisionWork {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingPerception == nil {
		return nil
	}
	work := &decisionWork{
		perception:   cloneRawMessage(a.pendingPerception),
		physical:     clonePhysical(a.latestPhysical),
		currentTask:  cloneTask(a.currentTask),
		reasons:      append([]string(nil), a.pendingReasons...),
		extras:       append([]string(nil), a.recentEnvironmentEvents...),
		scanFollowup: a.pendingScanFollowup,
		dailyPlan:    a.dailyPlan,
		timeOfDay:    extractTimeOfDay(a.pendingPerception),
	}
	work.localSummary = buildLocalSummary(
		work.perception, work.physical, work.currentTask,
		a.recentActions, a.summaryEnvironmentEvents, a.dailyPlan,
	)
	a.pendingPerception = nil
	a.pendingReasons = nil
	a.pendingScanFollowup = false
	a.recentEnvironmentEvents = nil
	return work
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
	a.decisionActive = false
	a.activeScanFollowup = false
	a.pendingScanID = ""
	a.pendingScanFollowup = false
	a.latestPerception = nil
	a.pendingPerception = nil
	a.pendingReasons = nil
	a.recentEnvironmentEvents = nil
	a.currentActionID = "" // agent 下线时清空（避免残留）
	// 停止所有 pending action 超时 timer
	for _, timer := range a.pendingActionTimeouts {
		timer.Stop()
	}
	a.pendingActionTimeouts = make(map[string]*time.Timer)
	cancel := a.cancel
	a.mu.Unlock()
	cancel()
}

func (a *agentContext) beginDecision() (agentEpoch, decisionEpoch int64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || !a.online {
		return 0, 0, false
	}
	a.decisionEpoch++
	a.decisionActive = true
	return a.agentEpoch, a.decisionEpoch, true
}

func (a *agentContext) beginDecisionWithScan(scanFollowup bool) (agentEpoch, decisionEpoch int64, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || !a.online {
		return 0, 0, false
	}
	a.decisionEpoch++
	a.decisionActive = true
	a.activeScanFollowup = scanFollowup
	return a.agentEpoch, a.decisionEpoch, true
}

func (a *agentContext) endDecision(epoch int64) {
	a.mu.Lock()
	if a.decisionEpoch == epoch {
		a.decisionActive = false
		a.activeScanFollowup = false
	}
	a.mu.Unlock()
}

func (a *agentContext) validateDecision(epoch int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.stopped || !a.online:
		return errors.New("agent offline")
	case epoch <= 0:
		return errors.New("missing decision_epoch")
	case !a.decisionActive || epoch != a.decisionEpoch:
		return fmt.Errorf("stale decision_epoch: got %d, current %d", epoch, a.decisionEpoch)
	default:
		return nil
	}
}

func (a *agentContext) armScan(decisionEpoch int64, scanID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.stopped || !a.online:
		return errors.New("agent offline")
	case decisionEpoch <= 0:
		return errors.New("missing decision_epoch")
	case !a.decisionActive || decisionEpoch != a.decisionEpoch:
		return fmt.Errorf("stale decision_epoch: got %d, current %d", decisionEpoch, a.decisionEpoch)
	case a.activeScanFollowup:
		return errors.New("scan_area unavailable during scan-response decision")
	case a.pendingScanID != "":
		return errors.New("scan_area already pending")
	}
	a.pendingScanID = scanID
	return nil
}

func (a *agentContext) disarmScan(scanID string) {
	a.mu.Lock()
	if a.pendingScanID == scanID {
		a.pendingScanID = ""
	}
	a.mu.Unlock()
}

func (a *agentContext) retryCurrentSnapshotOnError() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || a.latestPerception == nil {
		return false
	}
	a.pendingPerception = cloneRawMessage(a.latestPerception)
	a.pendingReasons = mergeUnique(a.pendingReasons, "上游错误恢复重试")
	// clear any stale scan followup — this is a fresh decision
	a.pendingScanFollowup = false
	return true
}

func (a *agentContext) recordActionStarted(actionID, cmd string, params map[string]any, decisionEpoch int64) {
	encoded, _ := json.Marshal(params)
	a.mu.Lock()
	a.currentActionID = actionID // 追踪当前执行中的 action（约定9 stop_action ID 匹配）
	a.recentActions = appendRolling(a.recentActions, localActionSummary{
		ActionID: actionID, Cmd: cmd, Params: string(encoded),
		DecisionEpoch: decisionEpoch, Result: "started",
	}, 8)
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

func containsReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}

func taskLifecycleReasons(previous, current *protocol.CurrentTaskProgress) []string {
	switch {
	case previous == nil && current != nil:
		return []string{fmt.Sprintf("任务开始:%s", current.ActionID)}
	case previous != nil && current == nil:
		return []string{fmt.Sprintf("任务结束:%s", previous.ActionID)}
	case previous != nil && current != nil && previous.ActionID != current.ActionID:
		return []string{fmt.Sprintf("任务切换:%s→%s", previous.ActionID, current.ActionID)}
	default:
		return nil
	}
}

func physicalCrossingReasons(previous, current protocol.PhysicalState) []string {
	checks := []struct {
		name     string
		wasAlert bool
		isAlert  bool
	}{
		{name: "energy<=25", wasAlert: previous.Energy <= 25, isAlert: current.Energy <= 25},
		{name: "fatigue>=80", wasAlert: previous.Fatigue >= 80, isAlert: current.Fatigue >= 80},
		{name: "joint_wear>=80", wasAlert: previous.JointWear >= 80, isAlert: current.JointWear >= 80},
		{name: "health<=50", wasAlert: previous.Health <= 50, isAlert: current.Health <= 50},
	}
	reasons := make([]string, 0, len(checks))
	for _, check := range checks {
		if check.wasAlert == check.isAlert {
			continue
		}
		direction := "离开"
		if check.isAlert {
			direction = "进入"
		}
		reasons = append(reasons, fmt.Sprintf("物理状态%s警戒带:%s", direction, check.name))
	}
	return reasons
}

// runPerceptionWorker serially sends perceptions to Hermes. While one request
// is in flight, enqueuePerception overwrites the pending slot, so after the
// request completes the worker processes only the newest world state.
func runPerceptionWorker(
	ctx context.Context,
	agentID string,
	ac *agentContext,
	hc *hermes.Client,
	ws *wsserver.Server,
	kb *worldkb.KB,
	logger *slog.Logger,
) {
	// 战略层：进入感知循环前生成当日计划。生成期间感知走 latest-wins
	// 队列暂存，生成完毕后 for 循环立即处理最新感知。
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

		work := ac.takeDecision()
		if work == nil {
			continue
		}

		// 安全模式检查（约定 §5.3）：连续 5 次 LLM 失败后进入安全模式，
		// 不调 LLM，只发 idle wait，持续上报感知，等管理员介入重启 MCP。
		if ac.IsInSafeMode() {
			logger.Warn("[SafeMode] skipping LLM, sending idle wait", "agent_id", agentID)
			if ws.IsConnected() {
				// 发 idle wait 5 分钟（不调 LLM，避免持续失败循环）
				if _, err := ws.SendAction(ctx, agentID, protocol.CmdWait, map[string]any{"duration_sec": 300}); err != nil {
					logger.Debug("[SafeMode] idle wait send failed", "agent_id", agentID, "err", err)
				}
			}
			// inSafeMode 保持 true，等管理员介入重启；llmFailures 已在 EnterSafeMode 清零
			continue
		}

		agentEpoch, decisionEpoch, ok := ac.beginDecisionWithScan(work.scanFollowup)
		if !ok {
			continue
		}
		text := perception.Format(work.perception, work.physical, work.extras, kb)
		if text == "" {
			ac.endDecision(decisionEpoch)
			logger.Warn("perception format returned empty", "agent_id", agentID, "raw", string(work.perception))
			continue
		}
		// 按需注入每日计划：时段边界跨越时注入完整计划，否则只注入当前时段。
		ac.mu.Lock()
		planInjection, newSlot := selectPlanInjection(work.dailyPlan, work.timeOfDay, ac.lastPlanSlot)
		ac.lastPlanSlot = newSlot
		ac.mu.Unlock()
		text = formatDecisionPrompt(text, agentID, agentEpoch, decisionEpoch, work.reasons, work.currentTask, planInjection)
	logger.Info("[MCP→Hermes/PERCEPTION]", "agent_id", agentID,
		"agent_epoch", agentEpoch, "decision_epoch", decisionEpoch, "text", text)

		resp, err := hc.SendWithSummary(ctx, text, work.localSummary)
		ac.endDecision(decisionEpoch)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("Hermes request canceled", "agent_id", agentID)
				return
			}
			// 累加连续 LLM 失败次数（约定 §5.3 安全模式）
			failures := ac.IncLLMFailures()
			if errors.Is(err, hermes.ErrUpstreamError) {
				// The session was already cleared by the client; immediately
				// retry with the same snapshot so that the NPC gets a clean
				// decision turn without waiting for the next external event.
				logger.Warn("[Hermes→MCP] upstream error — retrying with fresh session",
					"agent_id", agentID, "consecutive_failures", failures)
				if ac.retryCurrentSnapshotOnError() {
					continue
				}
			}
			// 连续 5 次失败进入安全模式（约定 §5.3）
			if failures >= 5 {
				ac.EnterSafeMode()
				logger.Error("[SafeMode] entering safe mode after 5 consecutive LLM failures",
					"agent_id", agentID, "failures", failures,
					"hint", "restart MCP process to exit safe mode")
				continue
			}
			logger.Error("hermes send failed", "agent_id", agentID, "err", err,
				"consecutive_failures", failures)
			continue
		}
		// LLM 调用成功，清零失败计数并退出安全模式（如果之前在安全模式）
		ac.ResetLLMFailures()
		narrative := resp.ExtractText()
		disp := strings.ReplaceAll(narrative, "\n", "\\n")
		if len(disp) > 100 {
			disp = disp[:100] + "..."
		}
		logger.Info("[Hermes→MCP/RESPONSE]",
			"agent_id", agentID,
			"agent_epoch", agentEpoch,
			"decision_epoch", decisionEpoch,
			"tokens", resp.Usage.TotalTokens,
			"narrative_len", len(narrative),
			"narrative", disp,
		)
		if narrative != "" {
			if err := ws.SendEnvelope(agentID, "narrative", map[string]any{"text": narrative}); err != nil {
				logger.Debug("narrative push failed", "agent_id", agentID, "err", err)
			}
		}
	}
}

// extractTimeOfDay 从 perception_update payload 中提取 "HH:MM" 格式的游戏时间。
// 用于按需注入每日计划（判断时段边界跨越）。失败返回空串。
func extractTimeOfDay(raw json.RawMessage) string {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Environment.TimeOfDay
}

func formatDecisionPrompt(perceptionText, agentID string, agentEpoch, decisionEpoch int64, reasons []string, task *protocol.CurrentTaskProgress, dailyPlan string) string {
	lines := []string{
		fmt.Sprintf("[decision_context] agent_id=%s agent_epoch=%d decision_epoch=%d", agentID, agentEpoch, decisionEpoch),
		"[决策触发原因] " + strings.Join(reasons, "；"),
	}
	if dailyPlan != "" {
		lines = append(lines, dailyPlan)
	}
	if task == nil {
		lines = append(lines, "[当前任务] 无")
	} else {
		lines = append(lines, fmt.Sprintf("[当前任务] action_id=%s progress=%g", task.ActionID, task.Progress))
	}
	lines = append(lines, perceptionText)
	lines = append(lines, "【决策约束】每轮只选择一个主行为；不要同时发起多个主行为。")
	return strings.Join(lines, "\n")
}

type guardedExecutor struct {
	ws     *wsserver.Server
	lookup func(string) *agentContext
}

func (g *guardedExecutor) validate(agentID string, decisionEpoch int64) (*agentContext, error) {
	ac := g.lookup(agentID)
	if ac == nil {
		return nil, fmt.Errorf("unknown agent: %s", agentID)
	}
	if err := ac.validateDecision(decisionEpoch); err != nil {
		return nil, err
	}
	if !g.ws.IsConnected() {
		return nil, errors.New("UE disconnected")
	}
	return ac, nil
}

func (g *guardedExecutor) SendAction(ctx context.Context, agentID string, decisionEpoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error) {
	ac, err := g.validate(agentID, decisionEpoch)
	if err != nil {
		return nil, err
	}
	ack, err := g.ws.SendAction(ctx, agentID, cmd, params)
	if err == nil && ack != nil {
		ac.recordActionStarted(ack.ActionID, cmd, params, decisionEpoch)
		// 注册 action_completed 超时 timer（约定 §5.2：estimated_duration × 1.5）
		ac.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, g.ws, agentID, g.lookup)
	}
	return ack, err
}

func (g *guardedExecutor) RequestScan(ctx context.Context, agentID string, decisionEpoch int64) error {
	ac, err := g.validate(agentID, decisionEpoch)
	if err != nil {
		return err
	}
	scanID := "scan_" + uuid.NewString()
	// Arm before writing: Mock UE can respond immediately, so registering the
	// token after the send would race the matching perception_update.
	if err := ac.armScan(decisionEpoch, scanID); err != nil {
		return err
	}
	if err := g.ws.RequestScan(ctx, agentID, scanID); err != nil {
		ac.disarmScan(scanID)
		return err
	}
	return nil
}

// LookupCurrentActionID 返回 agent 当前执行中的 action_id，空表示无动作。
// 用于 stop 工具实现约定9 的 stop_action ID 匹配。
func (g *guardedExecutor) LookupCurrentActionID(agentID string) string {
	ac := g.lookup(agentID)
	if ac == nil {
		return ""
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.currentActionID
}

// ClearCurrentActionID 清除 agent 的当前动作追踪（stop_action 发送后调用）。
func (g *guardedExecutor) ClearCurrentActionID(agentID string) {
	ac := g.lookup(agentID)
	if ac == nil {
		return
	}
	ac.mu.Lock()
	ac.currentActionID = ""
	ac.mu.Unlock()
}

// SendStopAction 发送 stop_action 控制消息到 UE（约定9）。
// fire-and-forget：不等 ACK，UE 侧比对 action_id 匹配才执行。
func (g *guardedExecutor) SendStopAction(_ context.Context, agentID, actionID string) error {
	if !g.ws.IsConnected() {
		return errors.New("UE disconnected")
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

// IncLLMFailures 累加连续 LLM 失败次数并返回累加后的值（约定 §5.3 安全模式）。
func (a *agentContext) IncLLMFailures() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.llmFailures++
	return a.llmFailures
}

// ResetLLMFailures 清零连续失败计数并退出安全模式（LLM 调用成功时调用）。
func (a *agentContext) ResetLLMFailures() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.llmFailures = 0
	a.inSafeMode = false
}

// EnterSafeMode 进入安全模式（不调 LLM，只发 idle，等管理员介入）。
func (a *agentContext) EnterSafeMode() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inSafeMode = true
	a.llmFailures = 0 // 清零避免重复触发
}

// IsInSafeMode 返回是否处于安全模式。
func (a *agentContext) IsInSafeMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inSafeMode
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

	hc := hermes.New(hermes.Config{
		URL:    *hermesURL,
		APIKey: *hermesAPIKey,
		Model:  *hermesModel,
		Logger: logger,
	})

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
		agents[id] = ac
		go runPerceptionWorker(workerCtx, id, ac, hc, ws, kb, logger)
		return ac, true
	}

	// All tools pass through the online/decision-epoch guard before WS send.
	executor := &guardedExecutor{ws: ws, lookup: lookupAgent}
	tools.RegisterAll(server, executor, kb, logger)

	// ─── Wire inbound message handler ──────────────────────────
	ws.SetMessageHandler(func(_ context.Context, msgType, agentID string, payload json.RawMessage) {
		switch msgType {
		case protocol.TypeAgentRegistered:
			// First registration = new day → reset Hermes session.
			// Re-registration after reconnect = restore, keep the session
			// (§4.2: match by agent_id, don't wipe Agent Mind state).
			ac, isNew := registerAgent(agentID)
			if isNew {
				hc.ResetSession()
				logger.Info("agent_registered (new day)", "agent_id", agentID,
					"agent_epoch", ac.agentEpoch, "payload", string(payload))
			} else {
				logger.Info("agent_registered (reconnect, session kept)", "agent_id", agentID,
					"agent_epoch", ac.agentEpoch)
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
			reasons := ac.updateState(sr)
			logger.Info("state_report", "agent_id", agentID,
				"energy", sr.PhysicalState.Energy, "fatigue", sr.PhysicalState.Fatigue,
				"joint_wear", sr.PhysicalState.JointWear, "health", sr.PhysicalState.Health,
				"decision_reasons", reasons)

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
			reasons, replaced, err := ac.observePerception(payload)
			if err != nil {
				logger.Warn("perception_update parse failed", "agent_id", agentID, "err", err)
				return
			}
			if replaced {
				logger.Info("perception coalesced (older pending update replaced)", "agent_id", agentID)
			}
			if len(reasons) > 0 {
				logger.Info("perception decision triggered", "agent_id", agentID, "reasons", reasons)
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
		runHTTP(ctx, logger, server, *httpAddr, *httpAllowAnyOrigin, *mcpAPIKey, ws, hc)
	} else {
		runStdio(ctx, logger, server)
	}
}

// runHTTP serves the MCP server over Streamable HTTP + a /status endpoint.
func runHTTP(ctx context.Context, logger *slog.Logger, server *mcp.Server, addr string, allowAnyOrigin bool, apiKey string, ws *wsserver.Server, hc *hermes.Client) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"ok":true,"ws_connected":%v,"hermes_session":%q}`,
			ws.IsConnected(),
			hc.SessionID(),
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
