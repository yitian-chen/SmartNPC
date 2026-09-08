// Package log centralizes structured logging configuration.
//
// All logs MUST go to stderr. The MCP stdio transport reserves stdout for the
// JSON-RPC stream; writing logs there will corrupt the protocol.
package log

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger that writes JSON to stderr,同时把每条日志
// 捕获到内存环形缓冲供 debug web (/debug/logs) 浏览。
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	inner := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
		// AddSource 关闭：方向标记 [UE→MCP] 等已隐含来源，
		// source 字段每条多 ~150 字节，对排查帮助有限。
	})
	return slog.New(&capturingHandler{inner: inner})
}
