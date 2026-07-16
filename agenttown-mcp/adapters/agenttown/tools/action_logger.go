package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// toolCallLog is the structured logger that records every tool invocation
// to logs/mcp/tool_calls.log (one JSON line per call). Mirrors the SmartNPC
// logToolCall pattern.
var (
	toolLogOnce sync.Once
	toolLogger  *slog.Logger
	toolLogFile *os.File
)

// initToolLogger lazily opens the tool_calls.log file. Safe to call from
// multiple tool handlers; the first call wins.
func initToolLogger() {
	toolLogOnce.Do(func() {
		dir := "logs/mcp"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			// Fall back to the default logger; we don't want log setup
			// failures to break tool calls.
			toolLogger = slog.Default()
			return
		}
		fname := filepath.Join(dir, "tool_calls.log")
		f, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			toolLogger = slog.Default()
			return
		}
		toolLogFile = f
		toolLogger = slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{}))
	})
}

// logToolCall writes a structured record of a tool invocation. Called at
// the start of every tool handler, before the Mock UE round-trip.
func logToolCall(name string, input any) {
	initToolLogger()
	payload, _ := json.Marshal(input)
	toolLogger.Info("tool_call",
		"tool", name,
		"input", string(payload),
		"ts", time.Now().UTC().Format(time.RFC3339Nano),
	)
	// Also emit to stderr so the operator sees calls in real time.
	fmt.Fprintf(os.Stderr, "[TOOL] %s | %s\n", name, string(payload))
}
