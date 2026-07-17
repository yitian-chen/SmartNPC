// Command agenttown-mcp is the MCP server bridging Mock UE to Hermes Gateway
// for the AgentTown_v3 project.
//
// Three roles in one process:
//
//  1. MCP Server (Streamable HTTP at :8760/mcp) — Hermes connects here as
//     a standard MCP client, discovers the 10 game tools, and calls them
//     during a turn.
//  2. WebSocket Server (:9000/ws) — Mock UE connects here, pushes JSON
//     perception events. MCP converts them to natural language and POSTs
//     to Hermes /v1/responses.
//  3. Hermes HTTP Client — owns the per-game-day session via
//     previous_response_id.
//
// Tool flow: Hermes calls a tool → MCP logs to console → MCP forwards the
// call to Mock UE over WS → Mock UE simulates and returns ActionResult →
// MCP returns the result to Hermes.
//
// IMPORTANT: in stdio mode, never write logs to stdout — it would corrupt
// the MCP stream. All logging goes through stderr.
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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/perception"
	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/internal/log"
	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"github.com/AgentTown/agenttown-mcp/pkg/transport"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

var version = "0.1.0-dev"

func main() {
	log.EnableUTF8Console()

	var (
		showVersion  = flag.Bool("version", false, "print version and exit")
		logLevel     = flag.String("log-level", "info", "log level: debug|info|warn|error")
		httpAddr     = flag.String("http", ":8760", "MCP Streamable HTTP addr (empty = stdio)")
		wsAddr       = flag.String("ws", ":9000", "WebSocket server addr for Mock UE")
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

	// Register all 10 tools. They call ws.Call(...) to reach Mock UE.
	tools.RegisterAll(server, ws, logger)

	// ─── Wire perception flow ──────────────────────────────────
	// Mock UE pushes perception_update events → format NL → POST Hermes.
	// day_started → reset Hermes session for a fresh conversation.
	ws.SetEventHandler(func(_ context.Context, name string, data json.RawMessage) {
		switch name {
		case wsserver.EventPerceptionUpdate:
			text := perception.Format(data)
			if text == "" {
				logger.Warn("perception format returned empty", "raw", string(data))
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
				// Push the narrative to Mock UE so the operator can see
				// what the NPC is "saying" in real time.
				if narrative != "" {
					if err := ws.Broadcast("narrative", map[string]any{
						"text": narrative,
					}); err != nil {
						logger.Debug("narrative push to mock ue failed", "err", err)
					}
				}
			}()
		case wsserver.EventDayStarted:
			hc.ResetSession()
			logger.Info("day started event received")
		case wsserver.EventDayEnded:
			logger.Info("day ended event received")
		default:
			logger.Debug("unknown event", "name", name)
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

// runHTTP serves the MCP server over Streamable HTTP and adds a /status
// endpoint for observability.
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
