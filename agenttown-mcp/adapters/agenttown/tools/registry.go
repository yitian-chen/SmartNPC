// Package tools registers the 10 AgentTown MCP tools that Hermes calls
// during a turn. Each tool:
//
//  1. Validates its input struct.
//  2. Logs the call to stderr (structured slog) + logs/mcp/tool_calls.log.
//  3. Forwards the call to Mock UE via the MockUE interface (WebSocket
//     request/response).
//  4. Unmarshals Mock UE's ActionResult into the typed Output struct and
//     returns it.
//
// Tools are stateless from the MCP side — Mock UE is the source of truth
// for NPC state (position, zone, energy, etc.).
package tools

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// MockUE is the interface tools use to reach Mock UE. The concrete
// implementation is *wsserver.Server; tests can substitute an in-memory
// fake.
type MockUE interface {
	Call(ctx context.Context, action string, params any) (json.RawMessage, error)
}

// RegisterAll installs all 10 AgentTown tools onto the given mcp.Server.
func RegisterAll(s *mcp.Server, ue MockUE, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	registerMovement(s, ue, logger)     // move_to, turn_to
	registerWork(s, ue, logger)         // work_assemble, interact_with
	registerMaintenance(s, ue, logger)  // charge_at, self_check
	registerSocial(s, ue, logger)       // speak, emote
	registerState(s, ue, logger)        // wait
	registerPlanning(s, ue, logger)     // update_plan
}

// ensure the *wsserver.Server satisfies MockUE at compile time.
var _ MockUE = (*wsserver.Server)(nil)
