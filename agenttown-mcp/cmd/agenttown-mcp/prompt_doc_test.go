package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

func TestDumpPromptDoc_FirstCallPerLayerPerProcess(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")
	resetPromptDocForTest(doc)

	// 第一次战略层调用：落盘。
	dumpPromptDoc("H-01", "strategic", "SYS-STRATEGIC", "USER-STRATEGIC", nil, testLogger())
	// 第二次战略层调用：不重复落盘。
	dumpPromptDoc("H-01", "strategic", "SYS-STRATEGIC-2", "USER-STRATEGIC-2", nil, testLogger())
	// 第一次战术层调用：落盘（与战略层独立计数）。
	dumpPromptDoc("H-01", "tactical", "SYS-TACTICAL", "USER-TACTICAL", nil, testLogger())
	// 非 H-01 的调用：忽略。
	dumpPromptDoc("H-02", "strategic", "SYS-H02", "USER-H02", nil, testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"H-01 首次战略层 prompt",
		"H-01 首次战术层 prompt",
		"SYS-STRATEGIC", "USER-STRATEGIC",
		"SYS-TACTICAL", "USER-TACTICAL",
		"### System Prompt", "### User Prompt",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("doc missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "SYS-STRATEGIC-2") {
		t.Errorf("second strategic call should not be dumped:\n%s", s)
	}
	if strings.Contains(s, "H-02") {
		t.Errorf("non-H-01 calls should be ignored:\n%s", s)
	}
}

func TestDumpPromptDoc_DisabledWhenPathEmpty(t *testing.T) {
	resetPromptDocForTest("")
	dumpPromptDoc("H-01", "strategic", "SYS", "USER", nil, testLogger())
	// 无 panic、无写入即为通过（路径为空时直接返回）。
}

func TestDumpPromptDoc_OverwritesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")

	// 模拟第一次仿真（进程 1）。
	resetPromptDocForTest(doc)
	dumpPromptDoc("H-01", "strategic", "SYS-RUN1", "USER-RUN1", nil, testLogger())
	// 模拟服务器重启（进程 2）：重置进程级状态后再次落盘应覆盖旧内容，
	// 文档只反映最新一次仿真。
	resetPromptDocForTest(doc)
	dumpPromptDoc("H-01", "strategic", "SYS-RUN2", "USER-RUN2", nil, testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "SYS-RUN2") {
		t.Errorf("doc should contain latest run:\n%s", s)
	}
	if strings.Contains(s, "SYS-RUN1") {
		t.Errorf("doc should be overwritten, stale run must not remain:\n%s", s)
	}
}

func TestDumpPromptDoc_ToolsSection(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")
	resetPromptDocForTest(doc)

	tools := []venus.Tool{{
		Type: "function",
		Function: venus.ToolFunction{
			Name:        "work_shift",
			Description: "去指定设施执行工作",
		},
	}}
	dumpPromptDoc("H-01", "tactical", "SYS", "USER", tools, testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "### Tools") {
		t.Errorf("doc should contain a Tools section when tools are provided:\n%s", s)
	}
	if !strings.Contains(s, `"name": "work_shift"`) {
		t.Errorf("Tools section should serialize the tool name:\n%s", s)
	}
}

// resetPromptDocForTest 重置 prompt_doc 的进程级状态（测试隔离用）。
func resetPromptDocForTest(path string) {
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	promptDocPath = path
	promptDocDumped = map[string]bool{}
	promptDocInitialized = false
}

// testLogger 复用 reactive_runner_test.go 中的实现（丢弃输出的 slog logger）。
