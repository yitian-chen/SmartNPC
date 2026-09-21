package main

import (
	"log/slog"
	"os"
)

// testLogger returns a discard-output slog logger for tests.
// Originally in reactive_runner_test.go（P4-12 退役删除后迁到此处，
// 被多个测试文件引用）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}
