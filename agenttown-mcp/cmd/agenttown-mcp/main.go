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

// agentContext holds per-agent state accumulated between perception turns.
// Perception delivery uses a latest-wins queue: at most one Hermes request is
// in flight per agent, and while it runs only the newest pending perception is
// retained. This prevents fast UE updates from building an unbounded backlog.
type agentContext struct {
	mu                sync.Mutex
	latestPhysical    *protocol.PhysicalState
	pendingCompletion []string // human-readable completion lines
	pendingPerception json.RawMessage
	wake              chan struct{}
	cancel            context.CancelFunc
	stopped           bool
}

func newAgentContext(parent context.Context) (*agentContext, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	return &agentContext{
		wake:   make(chan struct{}, 1),
		cancel: cancel,
	}, ctx
}

func (a *agentContext) addCompletion(line string) {
	a.mu.Lock()
	a.pendingCompletion = append(a.pendingCompletion, line)
	a.mu.Unlock()
}

func (a *agentContext) drainCompletions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.pendingCompletion
	a.pendingCompletion = nil
	return c
}

func (a *agentContext) setPhysical(p protocol.PhysicalState) {
	a.mu.Lock()
	a.latestPhysical = &p
	a.mu.Unlock()
}

func (a *agentContext) physical() *protocol.PhysicalState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.latestPhysical
}

// enqueuePerception replaces any queued (not currently processing) perception
// and wakes the worker. It returns true when an older queued perception was
// replaced.
func (a *agentContext) enqueuePerception(payload json.RawMessage) bool {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return false
	}
	replaced := a.pendingPerception != nil
	a.pendingPerception = append(a.pendingPerception[:0], payload...)
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
	return replaced
}

func (a *agentContext) takePerception() json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload := a.pendingPerception
	a.pendingPerception = nil
	return payload
}

func (a *agentContext) stop() {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return
	}
	a.stopped = true
	a.pendingPerception = nil
	cancel := a.cancel
	a.mu.Unlock()
	cancel()
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

		payload := ac.takePerception()
		if payload == nil {
			continue
		}
		extras := ac.drainCompletions()
		text := perception.Format(payload, ac.physical(), extras)
		if text == "" {
			logger.Warn("perception format returned empty", "agent_id", agentID, "raw", string(payload))
			continue
		}
		display := text
		if len(display) > 500 {
			display = display[:500] + "..."
		}
		logger.Info("[MCP→Hermes/PERCEPTION]", "agent_id", agentID, "text", display)

		resp, err := hc.Send(ctx, text)
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
			getAgent(agentID).setPhysical(sr.PhysicalState)
			logger.Info("state_report", "agent_id", agentID,
				"energy", sr.PhysicalState.Energy, "fatigue", sr.PhysicalState.Fatigue,
				"joint_wear", sr.PhysicalState.JointWear, "health", sr.PhysicalState.Health)

		case protocol.TypeActionCompleted:
			var ac protocol.ActionCompletedPayload
			if err := json.Unmarshal(payload, &ac); err != nil {
				logger.Warn("action_completed parse failed", "err", err)
				return
			}
			line := fmt.Sprintf("动作 %s 已完成（%s）", ac.ActionID, ac.Result)
			getAgent(agentID).addCompletion(line)
			logger.Info("action_completed", "agent_id", agentID,
				"action_id", ac.ActionID, "result", ac.Result, "progress", ac.Progress)

		case protocol.TypeError:
			logger.Warn("error from mock ue", "agent_id", agentID, "payload", string(payload))

		case protocol.TypePerceptionUpdate:
			ac := getAgent(agentID)
			if ac.enqueuePerception(payload) {
				logger.Info("perception coalesced (older pending update replaced)", "agent_id", agentID)
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
