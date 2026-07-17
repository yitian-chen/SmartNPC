// Package wsserver hosts the WebSocket server that Mock UE connects to.
//
// Mock UE is the client; agenttown-mcp is the server. Messages use the
// 7-field Envelope defined in pkg/protocol.
//
// Phase 1: transport-level envelope + seq + agent_id routing. The action
// lifecycle (command → ACK → completed) is introduced in Phase 5; until
// then, Call() uses a request/response correlation keyed by msg_id for
// backward compatibility with the existing tool layer.
package wsserver

import "github.com/AgentTown/agenttown-mcp/pkg/protocol"

// Re-export protocol constants used by callers within this package and by
// the tools layer, so they don't all need to import pkg/protocol directly.
const (
	TypePerceptionUpdate  = protocol.TypePerceptionUpdate
	TypeActionCommand     = protocol.TypeActionCommand
	TypeActionStarted     = protocol.TypeActionStarted
	TypeActionCompleted   = protocol.TypeActionCompleted
	TypeStopAction        = protocol.TypeStopAction
	TypeStateReport       = protocol.TypeStateReport
	TypeAgentRegistered   = protocol.TypeAgentRegistered
	TypeAgentUnregistered = protocol.TypeAgentUnregistered
	TypeHeartbeat         = protocol.TypeHeartbeat
	TypeError             = protocol.TypeError
	TypeScanArea          = protocol.TypeScanArea
)
