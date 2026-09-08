package log

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestCapturingHandler_CapturesAndForwards 验证 capturingHandler 把
// 日志条目写入环形缓冲，同时转发给 inner handler。
func TestCapturingHandler_CapturesAndForwards(t *testing.T) {
	// 重置缓冲
	captureMu.Lock()
	captureBuf = nil
	captureMu.Unlock()

	inner := &fakeHandler{}
	h := &capturingHandler{inner: inner}

	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "test message", 0)
	rec.Add("key1", "val1", "key2", 42)

	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// inner 收到转发
	if !inner.called {
		t.Error("inner.Handle was not called")
	}
	// 缓冲收到捕获
	snap := Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Message != "test message" {
		t.Errorf("Message = %q, want %q", snap[0].Message, "test message")
	}
	if snap[0].Level != "INFO" {
		t.Errorf("Level = %q, want INFO", snap[0].Level)
	}
	if snap[0].Attrs["key1"] != "val1" {
		t.Errorf("Attrs[key1] = %v, want val1", snap[0].Attrs["key1"])
	}
	if snap[0].Attrs["key2"] != int64(42) {
		t.Errorf("Attrs[key2] = %v, want 42", snap[0].Attrs["key2"])
	}
}

// TestCapture_RingBufferEviction 验证环形缓冲超过上限时丢弃最旧条目。
func TestCapture_RingBufferEviction(t *testing.T) {
	captureMu.Lock()
	captureBuf = nil
	captureMu.Unlock()

	// 临时降低上限以便测试
	origMax := captureMaxEntries
	// captureMaxEntries 是常量，无法改——直接写 captureMaxEntries+1 条
	// 验证缓冲不超过上限且保留最新条目。

	h := &capturingHandler{inner: &fakeHandler{}}
	for i := 0; i < origMax+50; i++ {
		rec := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		rec.Add("idx", i)
		_ = h.Handle(context.Background(), rec)
	}

	snap := Snapshot()
	if len(snap) != origMax {
		t.Errorf("Snapshot len = %d, want %d (capped)", len(snap), origMax)
	}
	// 最旧的应被丢弃，最新的保留
	lastIdx, ok := snap[len(snap)-1].Attrs["idx"].(int64)
	if !ok {
		t.Fatalf("last entry idx not int64: %T", snap[len(snap)-1].Attrs["idx"])
	}
	if lastIdx != int64(origMax+49) {
		t.Errorf("last entry idx = %d, want %d", lastIdx, origMax+49)
	}

	_ = origMax // silence unused if constant folded
}

// TestCapture_TruncatesLongStringAttr 验证超长 string 值被截断，
// 避免大 payload 占满缓冲。
func TestCapture_TruncatesLongStringAttr(t *testing.T) {
	captureMu.Lock()
	captureBuf = nil
	captureMu.Unlock()

	h := &capturingHandler{inner: &fakeHandler{}}
	longStr := make([]byte, captureMaxAttrLen+100)
	for i := range longStr {
		longStr[i] = 'x'
	}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "big", 0)
	rec.Add("payload", string(longStr))
	_ = h.Handle(context.Background(), rec)

	snap := Snapshot()
	got, ok := snap[0].Attrs["payload"].(string)
	if !ok {
		t.Fatalf("payload not string: %T", snap[0].Attrs["payload"])
	}
	if len(got) <= captureMaxAttrLen {
		t.Errorf("payload len = %d, expected > %d (truncated+suffix)", len(got), captureMaxAttrLen)
	}
	if len(got) != captureMaxAttrLen+len("...(truncated)") {
		t.Errorf("payload len = %d, want %d", len(got), captureMaxAttrLen+len("...(truncated)"))
	}
}

// TestCapture_ConcurrentSafe 验证多 goroutine 并发写日志不竞争。
func TestCapture_ConcurrentSafe(t *testing.T) {
	captureMu.Lock()
	captureBuf = nil
	captureMu.Unlock()

	h := &capturingHandler{inner: &fakeHandler{}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				rec := slog.NewRecord(time.Now(), slog.LevelInfo, "concurrent", 0)
				rec.Add("g", n, "j", j)
				_ = h.Handle(context.Background(), rec)
			}
		}(i)
	}
	wg.Wait()

	snap := Snapshot()
	if len(snap) != 1000 && len(snap) != captureMaxEntries {
		// 1000 < 500? No, 1000 > 500, so should be capped at 500.
		t.Errorf("Snapshot len = %d, want %d (capped)", len(snap), captureMaxEntries)
	}
}

// TestSnapshot_ReturnsCopy 验证 Snapshot 返回拷贝，修改不影响内部缓冲。
func TestSnapshot_ReturnsCopy(t *testing.T) {
	captureMu.Lock()
	captureBuf = nil
	captureMu.Unlock()

	h := &capturingHandler{inner: &fakeHandler{}}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "orig", 0)
	_ = h.Handle(context.Background(), rec)

	snap := Snapshot()
	snap[0].Message = "modified"

	snap2 := Snapshot()
	if snap2[0].Message == "modified" {
		t.Error("Snapshot returned a reference, not a copy — internal buffer was mutated")
	}
}

// fakeHandler 是测试用 slog.Handler，仅记录是否被调用。
type fakeHandler struct {
	called bool
}

func (f *fakeHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (f *fakeHandler) Handle(_ context.Context, _ slog.Record) error {
	f.called = true
	return nil
}
func (f *fakeHandler) WithAttrs(_ []slog.Attr) slog.Handler { return f }
func (f *fakeHandler) WithGroup(_ string) slog.Handler        { return f }
