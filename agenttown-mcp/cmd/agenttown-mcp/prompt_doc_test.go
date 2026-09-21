package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpPromptDoc_LatestRequestBody(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")
	resetPromptDocForTest(doc)

	bodyStrategic1 := []byte(`{"model":"m-s","messages":[{"role":"system","content":"SYS-STRATEGIC"},{"role":"user","content":"USER-STRATEGIC"}]}`)
	bodyStrategic2 := []byte(`{"model":"m-s","messages":[{"role":"system","content":"SYS-STRATEGIC-2"}]}`)
	bodyTactical := []byte(`{"model":"m-t","messages":[{"role":"system","content":"SYS-TACTICAL"}],"tools":[{"type":"function","function":{"name":"work_shift"}}],"tool_choice":"required"}`)

	// 第一次战略层：落盘。
	dumpPromptDoc("H-01", "strategic", bodyStrategic1, testLogger())
	// 第二次战略层：覆盖（保留最新）。
	dumpPromptDoc("H-01", "strategic", bodyStrategic2, testLogger())
	// 战术层：独立保留最新。
	dumpPromptDoc("H-01", "tactical", bodyTactical, testLogger())
	// 非 H-01 的调用：忽略。
	dumpPromptDoc("H-02", "strategic", []byte(`{"h02":true}`), testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"H-01 最新战略层请求体",
		"H-01 最新战术层请求体",
		"SYS-STRATEGIC-2", // 第二次调用覆盖，保留最新
		"SYS-TACTICAL",
		`"name": "work_shift"`,
		`"tool_choice": "required"`,
		`"role": "system"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("doc missing %q:\n%s", want, s)
		}
	}
	// 第一次战略层 body 被覆盖，不应残留。
	if strings.Contains(s, "USER-STRATEGIC") {
		t.Errorf("stale strategic body should be overwritten by latest call:\n%s", s)
	}
	if strings.Contains(s, "H-02") {
		t.Errorf("non-H-01 calls should be ignored:\n%s", s)
	}
}

func TestDumpPromptDoc_DisabledWhenPathEmpty(t *testing.T) {
	resetPromptDocForTest("")
	dumpPromptDoc("H-01", "strategic", []byte(`{"a":1}`), testLogger())
	// 无 panic、无写入即为通过（路径为空时直接返回）。
}

func TestDumpPromptDoc_OverwritesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")

	// 模拟第一次仿真（进程 1）。
	resetPromptDocForTest(doc)
	dumpPromptDoc("H-01", "strategic", []byte(`{"run":"1"}`), testLogger())
	// 模拟服务器重启（进程 2）：重置进程级状态后再次落盘应覆盖旧内容。
	resetPromptDocForTest(doc)
	dumpPromptDoc("H-01", "strategic", []byte(`{"run":"2"}`), testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `"run": "2"`) {
		t.Errorf("doc should contain latest run:\n%s", s)
	}
	if strings.Contains(s, `"run": "1"`) {
		t.Errorf("doc should be overwritten, stale run must not remain:\n%s", s)
	}
}

// resetPromptDocForTest 重置 prompt_doc 的进程级状态（测试隔离用）。
func resetPromptDocForTest(path string) {
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	promptDocPath = path
	promptDocBodies = map[string][]byte{}
	promptDocTimes = map[string]string{}
}

// testLogger 复用 reactive_runner_test.go 中的实现（丢弃输出的 slog logger）。

// TestExtractSystemUser 验证无状态栏时取最后一条 user（原行为不变）。
func TestExtractSystemUser(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"sys内容\n第二行"},
		{"role":"user","content":"历史user1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"本次user\n含换行"}
	]}`)
	sys, usr := extractSystemUser(body)
	if sys != "sys内容\n第二行" {
		t.Errorf("system = %q, want sys内容\\n第二行", sys)
	}
	// 取最后一条 user
	if usr != "本次user\n含换行" {
		t.Errorf("user = %q, want 本次user\\n含换行", usr)
	}
}

// TestExtractSystemUser_TrailingStateBar 验证末条 user 是 <agent_state>
// 状态栏时捡拾倒数两条：本轮 user prompt + 状态栏拼接，避免预览只剩
// 状态栏。
func TestExtractSystemUser_TrailingStateBar(t *testing.T) {
	bar := agentStateBarOpen + "\n当前游戏时间：D12 10:47:03\n" + agentStateBarClose
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]string{
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "历史user1"},
			{"role": "assistant", "content": "a1"},
			{"role": "user", "content": "本次战术分解指令"},
			{"role": "user", "content": bar},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sys, usr := extractSystemUser(body)
	if sys != "sys" {
		t.Errorf("system = %q, want sys", sys)
	}
	want := "本次战术分解指令" + "\n\n" + bar
	if usr != want {
		t.Errorf("user = %q, want 本次战术分解指令+\\n\\n+状态栏", usr)
	}
	// 历史 user 不进预览。
	if strings.Contains(usr, "历史user1") {
		t.Errorf("preview should not carry historical user messages:\n%s", usr)
	}
}

func TestExtractSystemUser_InvalidJSON(t *testing.T) {
	sys, usr := extractSystemUser([]byte("not json"))
	if sys != "" || usr != "" {
		t.Errorf("invalid JSON should return empty, got %q/%q", sys, usr)
	}
}

// TestDumpPromptDoc_JevBodyPreview verifies the router layer's judgment-API
// request body ({model, state, questions} — no messages) renders a readable
// "state" section instead of silently losing the preview, while the raw
// JSON section keeps the full body.
func TestDumpPromptDoc_JevBodyPreview(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "actual_prompts.md")
	resetPromptDocForTest(doc)

	// 战术层（messages 形态）+ 路由层（jev 形态）同落一份文档。
	dumpPromptDoc("H-01", "tactical",
		[]byte(`{"model":"m-t","messages":[{"role":"system","content":"SYS-TACTICAL"},{"role":"user","content":"U"}]}`),
		testLogger())
	dumpPromptDoc("H-01", "router", []byte(`{"model":"jev-1.13.0","state":"STATE-判决上下文","questions":{"should_interrupt":{"type":"noul","instructions":"应该打断吗？"}}}`), testLogger())

	got, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("doc not written: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		"### 事件路由 · state",
		"STATE-判决上下文",
		"H-01 最新事件路由请求体",
		`"should_interrupt"`,
		"### 战术层 · user",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("doc missing %q:\n%s", want, s)
		}
	}
	// jev body 没有 system/user 消息，不得渲染旧的双段预览。
	if strings.Contains(s, "### 事件路由 · system") || strings.Contains(s, "### 事件路由 · user") {
		t.Errorf("jev body must not render the system/user preview sections:\n%s", s)
	}
}

// TestExtractJevState pins the shape discrimination: state+questions without
// messages is a judgment body; chat-completions bodies (with messages) and
// malformed input are not.
func TestExtractJevState(t *testing.T) {
	if got := extractJevState([]byte(`{"model":"m","state":"S","questions":{"q":{"type":"noul"}}}`)); got != "S" {
		t.Errorf("jev body state = %q, want S", got)
	}
	for name, body := range map[string]string{
		"chat completions": `{"model":"m","messages":[{"role":"user","content":"x"}],"state":"S","questions":{"q":{}}}`,
		"no questions":     `{"model":"m","state":"S"}`,
		"empty state":      `{"model":"m","state":"","questions":{"q":{}}}`,
		"not json":         `nonsense`,
	} {
		if got := extractJevState([]byte(body)); got != "" {
			t.Errorf("%s: extractJevState = %q, want empty", name, got)
		}
	}
}
