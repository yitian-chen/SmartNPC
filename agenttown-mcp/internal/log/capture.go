// Package log — capture.go
//
// 在 slog 默认 JSON handler 之上包一层 capturingHandler，把每条日志
// 同时写入 stderr 和内存环形缓冲，供 debug web (/debug/logs) 浏览。
//
// 设计权衡：
//   - 环形缓冲上限 500 条，per-string-attr 值超过 500 字符截断。debug web
//     用于快速浏览排查方向，不是完整日志重放——完整内容仍去 sim.log 看。
//   - 大型 payload（如 world_kb / perception_update）的字段值会被截断，
//     避免几条大日志占满整个缓冲。
//   - capturingHandler.WithAttrs/WithGroup 转发给 inner 并返回新 wrapper，
//     保证不破坏 slog 链式语义。当前代码库未使用 logger.With(...)，但
//     slog 内部可能用，所以正确实现而非 stub。

package log

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry 是一条捕获的日志记录，供 debug web 展示。
type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"msg"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

const (
	captureMaxEntries = 500
	captureMaxAttrLen = 500 // per-string-value truncation threshold
)

var (
	captureMu  sync.Mutex
	captureBuf []Entry
)

// Snapshot 返回最近日志条目的拷贝（按时间正序，最旧在前）。
// 返回的 slice 可安全修改。
func Snapshot() []Entry {
	captureMu.Lock()
	defer captureMu.Unlock()
	out := make([]Entry, len(captureBuf))
	copy(out, captureBuf)
	return out
}

// capturingHandler 包装一个 slog.Handler，在转发给 inner 之前把每条
// record 写入内存环形缓冲。
type capturingHandler struct {
	inner slog.Handler
}

func (h *capturingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *capturingHandler) Handle(ctx context.Context, record slog.Record) error {
	// 先捕获（即使 inner 失败也不丢条目）。
	attrs := make(map[string]any, record.NumAttrs())
	record.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = truncateAttrValue(a.Value.Any())
		return true
	})
	entry := Entry{
		Time:    record.Time,
		Level:   record.Level.String(),
		Message: record.Message,
		Attrs:   attrs,
	}
	captureMu.Lock()
	captureBuf = append(captureBuf, entry)
	if len(captureBuf) > captureMaxEntries {
		captureBuf = captureBuf[len(captureBuf)-captureMaxEntries:]
	}
	captureMu.Unlock()
	return h.inner.Handle(ctx, record)
}

func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &capturingHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *capturingHandler) WithGroup(name string) slog.Handler {
	return &capturingHandler{inner: h.inner.WithGroup(name)}
}

// truncateAttrValue 限制超长 string 值，保持环形缓冲可控。
// 仅处理顶层 string；非 string 值（number/bool/nil）原样返回。
func truncateAttrValue(v any) any {
	if s, ok := v.(string); ok && len(s) > captureMaxAttrLen {
		return s[:captureMaxAttrLen] + "...(truncated)"
	}
	return v
}
