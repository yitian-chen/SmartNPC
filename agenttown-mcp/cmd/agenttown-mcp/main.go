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

// agentContext holds per-agent state the MCP layer accumulates between
// perception turns: the latest physical state (from state_report) and any
// pending action completions to fold into the next perception narrative.
type agentContext struct {
	mu                sync.Mutex
	latestPhysical    *protocol.PhysicalState
	pendingCompletion []string // human-readable completion lines
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

func main() {
	log.EnableUTF8Console()

	var (
		showVersion  = flag.Bool("version", false, "print version and exit")
		logLevel     = flag.String("log-level", "info", "log level: debug|info|warn|error")
		httpAddr     = flag.String("http", ":8760", "MCP Streamable HTTP addr (empty = stdio)")
		wsAddr       = flag.String("ws", ":9090", "WebSocket server addr for Mock UE")
		hermesURL    = flag.String("hermes-url", "http://localhost:8642", "Hermes Gateway base URL")
		hermesAPIKey = flag.String("hermes-api-key", "agenttown-test-key", "Hermes Gateway bearer token")
		hermesModel  = flag.String("hermes-model", "deepseek-v4-flash", "Hermes model name")
		mcpAPIKey    = flag.String("mcp-api-key", "", "if set, require this Bearer token on /mcp")
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
	getAgent := func(id string) *agentContext {
		agentsMu.Lock()
		defer agentsMu.Unlock()
		a, ok := agents[id]
		if !ok {
			a = &agentContext{}
			agents[id] = a
		}
		return a
	}

	// Register all tools. They call ws.SendAction/RequestScan.
	tools.RegisterAll(server, ws, logger)

	// ─── Wire inbound message handler ──────────────────────────
	ws.SetMessageHandler(func(_ context.Context, msgType, agentID string, payload json.RawMessage) {
		switch msgType {
		case protocol.TypeAgentRegistered:
			// New connection = new day. Reset Hermes session.
			hc.ResetSession()
			getAgent(agentID) // ensure context exists
			logger.Info("agent_registered", "agent_id", agentID, "payload", string(payload))

		case protocol.TypeAgentUnregistered:
			agentsMu.Lock()
			delete(agents, agentID)
			agentsMu.Unlock()
			logger.Info("agent_unregistered", "agent_id", agentID)

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
			// Fold pending completions + latest physical state into the
			// narrative context.
			extras := ac.drainCompletions()
			text := perception.Format(payload, ac.physical(), extras)
			if text == "" {
				logger.Warn("perception format returned empty", "raw", string(payload))
				return
			}
			// Async POST — don't block the WS read loop.
			go func() {
				resp, err := hc.Send(context.Background(), text)
				if err != nil {
					logger.Error("hermes send failed", "err", err)
					return
				}
				narrative := resp.ExtractText()
				logger.Info("hermes turn complete",
					"tokens", resp.Usage.TotalTokens,
					"narrative_len", len(narrative),
				)
				if narrative != "" {
					if err := ws.SendEnvelope(agentID, "narrative", map[string]any{
						"text": narrative,
					}); err != nil {
						logger.Debug("narrative push failed", "err", err)
					}
				}
			}()

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
