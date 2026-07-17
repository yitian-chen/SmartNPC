package tools

import (
	"encoding/json"
	"log/slog"
	"sync"
)

// toolsLogger is set by RegisterAll and used by logToolCall to emit
// tool invocation records through the MCP's main structured logger
// (which writes to stderr, redirected to logs/mcp.log via 2>&1).
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
// (logs/mcp.log) alongside WS and Hermes communication entries.
func logToolCall(name string, input any) {
	toolsLoggerMu.Lock()
	l := toolsLogger
	toolsLoggerMu.Unlock()
	if l == nil {
		return
	}
	payload, _ := json.Marshal(input)
	l.Info("[Hermes→MCP/TOOL]",
		"tool", name,
		"input", string(payload),
	)
}
