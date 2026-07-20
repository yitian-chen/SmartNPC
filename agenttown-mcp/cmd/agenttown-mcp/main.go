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

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/perception"
	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/internal/log"
	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/transport"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

var version = "0.1.0-dev"

// agentContext holds per-agent state accumulated between decision turns.
// Delivery uses a latest-wins queue: at most one Hermes request is in flight
// per agent, pending trigger reasons merge, and only the newest perception is
// retained while that request runs.
type agentContext struct {
	mu                       sync.Mutex
	latestPhysical           *protocol.PhysicalState
	latestPerception         json.RawMessage
	currentTask              *protocol.CurrentTaskProgress
	observedSnapshot         *observedSnapshot
	pendingPerception        json.RawMessage
	pendingReasons           []string
	recentEnvironmentEvents  []string // pending extras for the next decision
	summaryEnvironmentEvents []string // rolling authoritative environment history
	recentActions            []localActionSummary
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
}

func newAgentContext(parent context.Context) (*agentContext, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	return &agentContext{
		wake:   make(chan struct{}, 1),
		cancel: cancel,
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
	a.recentActions = appendRolling(a.recentActions, localActionSummary{
		ActionID: completion.ActionID, Result: completion.Result,
		DurationMs: completion.DurationMs, Progress: completion.Progress,
	}, 8)
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
		perception:  cloneRawMessage(a.pendingPerception),
		physical:    clonePhysical(a.latestPhysical),
		currentTask: cloneTask(a.currentTask),
		reasons:     append([]string(nil), a.pendingReasons...),
		extras:      append([]string(nil), a.recentEnvironmentEvents...),
	}
	work.localSummary = buildLocalSummary(
		work.perception, work.physical, work.currentTask,
		a.recentActions, a.summaryEnvironmentEvents,
	)
	a.pendingPerception = nil
	a.pendingReasons = nil
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
	a.latestPerception = nil
	a.pendingPerception = nil
	a.pendingReasons = nil
	a.recentEnvironmentEvents = nil
	cancel := a.cancel
	a.mu.Unlock()
	cancel()
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
	logger *slog.Logger,
) {
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
		text := perception.Format(work.perception, work.physical, work.extras)
		if text == "" {
			logger.Warn("perception format returned empty", "agent_id", agentID, "raw", string(work.perception))
			continue
		}
		text = formatDecisionPrompt(text, work.reasons, work.currentTask)
		display := text
		if len(display) > 500 {
			display = display[:500] + "..."
		}
		logger.Info("[MCP→Hermes/PERCEPTION]", "agent_id", agentID, "text", display)

		resp, err := hc.SendWithSummary(ctx, text, work.localSummary)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("Hermes request canceled", "agent_id", agentID)
				return
			}
			logger.Error("hermes send failed", "agent_id", agentID, "err", err)
			continue
		}
		narrative := resp.ExtractText()
		disp := narrative
		if len(disp) > 500 {
			disp = disp[:500] + "..."
		}
		logger.Info("[Hermes→MCP/RESPONSE]",
			"agent_id", agentID,
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

func formatDecisionPrompt(perceptionText string, reasons []string, task *protocol.CurrentTaskProgress) string {
	lines := []string{"[决策触发原因] " + strings.Join(reasons, "；")}
	if task == nil {
		lines = append(lines, "[当前任务] 无")
	} else {
		lines = append(lines, fmt.Sprintf("[当前任务] action_id=%s progress=%g", task.ActionID, task.Progress))
	}
	lines = append(lines, perceptionText)
	lines = append(lines, "【决策约束】每轮只选择一个主行为；不要同时发起多个主行为。")
	return strings.Join(lines, "\n")
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
		hermesModel        = flag.String("hermes-model", "deepseek-v4-flash", "Hermes model name")
		mcpAPIKey          = flag.String("mcp-api-key", "", "if set, require this Bearer token on /mcp")
		httpAllowAnyOrigin = flag.Bool("http-allow-any-origin", true,
			"disable origin / localhost restrictions so cross-host clients can connect")
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

	// Per-agent context (Phase 1: single agent, but keyed for multi-NPC).
	var agentsMu sync.Mutex
	agents := make(map[string]*agentContext)
	createAgentLocked := func(id string) *agentContext {
		a, workerCtx := newAgentContext(ctx)
		agents[id] = a
		go runPerceptionWorker(workerCtx, id, a, hc, ws, logger)
		return a
	}
	getAgent := func(id string) *agentContext {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		a, ok := agents[id]
		if !ok {
			a = createAgentLocked(id)
		}
		return a
	}
	// registerAgent returns true if this is the first registration for the
	// agent_id (a new day → session reset); false if it's a re-registration
	// after reconnect (restore, don't reset — §4.2).
	registerAgent := func(id string) bool {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		if _, ok := agents[id]; ok {
			return false
		}
		createAgentLocked(id)
		return true
	}

	// Register all tools. They call ws.SendAction/RequestScan.
	tools.RegisterAll(server, ws, logger)

	// ─── Wire inbound message handler ──────────────────────────
	ws.SetMessageHandler(func(_ context.Context, msgType, agentID string, payload json.RawMessage) {
		switch msgType {
		case protocol.TypeAgentRegistered:
			// First registration = new day → reset Hermes session.
			// Re-registration after reconnect = restore, keep the session
			// (§4.2: match by agent_id, don't wipe Agent Mind state).
			if registerAgent(agentID) {
				hc.ResetSession()
				logger.Info("agent_registered (new day)", "agent_id", agentID, "payload", string(payload))
			} else {
				logger.Info("agent_registered (reconnect, session kept)", "agent_id", agentID)
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
			reasons := getAgent(agentID).updateState(sr)
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
			queued := getAgent(agentID).recordActionCompletion(completed)
			logger.Info("action_completed", "agent_id", agentID,
				"action_id", completed.ActionID, "result", completed.Result,
				"progress", completed.Progress, "decision_queued", queued)

		case protocol.TypeEventNotification:
			var event protocol.EventNotificationPayload
			if err := json.Unmarshal(payload, &event); err != nil {
				logger.Warn("event_notification parse failed", "err", err)
				return
			}
			queued := getAgent(agentID).recordEventNotification(event)
			logger.Info("event_notification", "agent_id", agentID,
				"event_id", event.EventID, "perception_level", event.PerceptionLevel,
				"decision_queued", queued)

		case protocol.TypeError:
			logger.Warn("error from mock ue", "agent_id", agentID, "payload", string(payload))

		case protocol.TypePerceptionUpdate:
			ac := getAgent(agentID)
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

// ensure ws satisfies the tools.Executor interface at compile time.
var _ tools.Executor = (*wsserver.Server)(nil)
