package tools

import (
	"encoding/json"
	"log/slog"
	"sync"
)

// toolsLogger is set by RegisterAll and used by logToolCall to emit
// tool invocation records through the MCP's main structured logger
// (which writes to stderr, redirected to logs/YYYY-MM-DD/sim.log via 2>&1).
// When nil, logToolCall is a no-op (e.g. during tests without a logger).
var (
	toolsLoggerMu sync.Mutex
	toolsLogger   *slog.Logger
)

// setToolsLogger stores the logger for tool call logging. Idempotent;
// the first call wins.
func setToolsLogger(l *slog.Logger) {
	toolsLoggerMu.Lock()
	defer toolsLoggerMu.Unlock()
	if toolsLogger == nil {
		toolsLogger = l
	}
}

// logToolCall writes a structured record of a tool invocation through
// the MCP's main logger so the record appears in the unified log stream
// (logs/YYYY-MM-DD/sim.log) alongside WS and Hermes communication entries.
//
// agentID and decisionEpoch are emitted as top-level structured fields so a
// tool call can be correlated with the [MCP→Hermes/PERCEPTION] and
// [Hermes→MCP/RESPONSE] entries of the same decision turn by matching
// agent_id + decision_epoch.
func logToolCall(name, agentID string, decisionEpoch int64, input any) {
	toolsLoggerMu.Lock()
	l := toolsLogger
	toolsLoggerMu.Unlock()
	if l == nil {
		return
	}
	payload, _ := json.Marshal(input)
	l.Info("[Hermes→MCP/TOOL]",
		"tool", name,
		"agent_id", agentID,
		"decision_epoch", decisionEpoch,
		"input", string(payload),
	)
}
