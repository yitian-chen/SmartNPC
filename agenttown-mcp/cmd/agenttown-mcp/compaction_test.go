package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
)

func mkRound() []llmtypes.Message {
	return []llmtypes.Message{
		{Role: "user", Content: "tactical prompt"},
		{Role: "assistant", ToolCalls: []llmtypes.ToolCall{{ID: "c1", Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{}`}}}},
		{Role: "tool", Content: "result=pending", ToolCallID: "c1"},
		{Role: "tool", Content: "result=pending", ToolCallID: "c2"},
	}
}

func mkRounds(n int) []llmtypes.Message {
	var out []llmtypes.Message
	for i := 0; i < n; i++ {
		out = append(out, mkRound()...)
	}
	return out
}

func countAssistant(msgs []llmtypes.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func TestSplitTailRounds_NoopWhenBelowLimit(t *testing.T) {
	history := mkRounds(2)
	evict, tail := splitTailRounds(history, 4)
	if len(evict) != 0 {
		t.Fatalf("evict len = %d, want 0", len(evict))
	}
	if len(tail) != len(history) {
		t.Fatalf("tail len = %d, want %d", len(tail), len(history))
	}
}

func TestSplitTailRounds_KeepsLastNRounds(t *testing.T) {
	history := mkRounds(5)
	evict, tail := splitTailRounds(history, 2)
	if len(evict) == 0 || len(tail) == 0 {
		t.Fatalf("evict=%d tail=%d, both should be non-empty", len(evict), len(tail))
	}
	// 尾部起点必须是 assistant（不能以孤儿 tool 开头，否则破坏协议配对）。
	if tail[0].Role != "assistant" {
		t.Fatalf("tail[0].Role = %q, want assistant", tail[0].Role)
	}
	if got := countAssistant(tail); got != 2 {
		t.Fatalf("assistants in tail = %d, want 2", got)
	}
	// 尾部首个 assistant 的 tool 占位仍在尾部（配对未拆断）。
	if tail[1].Role != "tool" || tail[1].ToolCallID != "c1" {
		t.Fatalf("tail[1] = %+v, want tool c1", tail[1])
	}
}

func TestSplitTailRounds_NonPositiveKeepsAll(t *testing.T) {
	history := mkRounds(3)
	evict, tail := splitTailRounds(history, 0)
	if len(evict) != 0 || len(tail) != len(history) {
		t.Fatalf("rounds=0: evict=%d tail=%d, want 0/%d", len(evict), len(tail), len(history))
	}
}

func TestSplitTailRounds_Empty(t *testing.T) {
	evict, tail := splitTailRounds(nil, 4)
	if len(evict) != 0 || len(tail) != 0 {
		t.Fatalf("empty: evict=%d tail=%d, want 0/0", len(evict), len(tail))
	}
}

func TestFormatConversationForSummary(t *testing.T) {
	msgs := []llmtypes.Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", ToolCalls: []llmtypes.ToolCall{{Function: llmtypes.ToolFunction{Name: "speak", Arguments: `{"content":"x"}`}}}},
		{Role: "tool", Content: "result=pending", ToolCallID: "c1"},
	}
	got := formatConversationForSummary(msgs)
	for _, want := range []string{"用户：你好", "助手调用工具 speak(", `{"content":"x"}`, "工具结果[c1]：result=pending"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatConversationForSummary missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatConversationForSummary_Empty(t *testing.T) {
	if got := formatConversationForSummary(nil); got != "（空）" {
		t.Fatalf("empty = %q, want （空）", got)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	system := "你好" // 2 wide = 2
	tools := []venus.Tool{{
		Function: venus.ToolFunction{
			Name:        "speak",               // 5 narrow → 2
			Description: "说话",                  // 2 wide = 2
			Parameters:  json.RawMessage(`{}`), // 2 narrow → 1
		},
	}}
	history := []llmtypes.Message{{Role: "user", Content: "你好"}} // 2
	user := "你好"                                                 // 2

	got := estimateInputTokens(system, tools, "", history, user)
	want := 1 + 4 + 0 + 1 + 1 // 7
	if got != want {
		t.Fatalf("estimateInputTokens = %d, want %d", got, want)
	}
}

// ─── 2026-09-18 修复测试：摘要累积 + 并发互斥 ────────────────────

// mkFatRounds builds rounds whose user content is realistically sized
// (战术层 user prompt ~2000+ 字符），so a few dozen rounds cross the 8k
// estimate threshold — the tiny mkRound messages (est ~4.5/条) need
// 1800+ messages to trigger.
func mkFatRounds(n int) []llmtypes.Message {
	var out []llmtypes.Message
	for i := 0; i < n; i++ {
		r := mkRound()
		r[0].Content = strings.Repeat("装", 400)
		out = append(out, r...)
	}
	return out
}

// compactFakeLLM captures SendWithSummary calls (first-call semantics for the
// summarizer) and returns a scripted summary body.
type compactFakeLLM struct {
	fakeStrategicCaller
	mu       sync.Mutex
	prompts  []string
	response string
	block    chan struct{} // 非 nil 时阻塞直至 close
}

func (f *compactFakeLLM) SendWithSummary(_ context.Context, _, user string, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, user)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return makeStrategicResponse(f.response), nil
}

// TestCompaction_MergeTemplateCarriesPrevSummary pins fix 1: a second
// compaction must feed the OLD summary into the summarizer (merge), not
// discard it — successive compactions accumulate instead of forgetting.
func TestCompaction_MergeTemplateCarriesPrevSummary(t *testing.T) {
	ac := tacticalCtxForTest(nil)
	fake := &compactFakeLLM{response: "合并后的摘要"}
	ac.tacticalHc = fake

	_, err := ac.summarizeConversation(mkRounds(2), "旧摘要：上午装配了 3 小时", "H-01", nil, nil, testLogger())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("one call expected, got %d", len(fake.prompts))
	}
	p := fake.prompts[0]
	for _, want := range []string{"此前已有一份摘要", "旧摘要：上午装配了 3 小时", "【新一段历史原文】"} {
		if !strings.Contains(p, want) {
			t.Errorf("merge prompt missing %q:\n%s", want, p)
		}
	}

	// 无旧摘要 → 首轮模板。
	_, err = ac.summarizeConversation(mkRounds(2), "", "H-01", nil, nil, testLogger())
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if strings.Contains(fake.prompts[1], "此前已有一份摘要") {
		t.Errorf("first-round prompt must not use the merge template:\n%s", fake.prompts[1])
	}
}

// TestCompaction_SequentialCompactionsAccumulate drives the full flow twice:
// the second compaction's summarizer input carries the first compaction's
// summary — the "rolling forget" hole is closed.
func TestCompaction_SequentialCompactionsAccumulate(t *testing.T) {
	ac := tacticalCtxForTest(nil)
	fake := &compactFakeLLM{response: "第一段摘要：晨练后装配"}
	ac.tacticalHc = fake
	// 10 轮历史触发阈值（每轮 ~几十 token，10 轮 + 无 userContent 足够？用
	// 显式大历史保证：mkRounds(40)）。
	for _, m := range mkFatRounds(45) {
		ac.as.AppendConversationMessage(m)
	}
	ac.maybeCompactConversation("sys", nil, "user", "H-01", nil, nil, testLogger())
	if got := len(ac.as.Conversation()); got >= len(mkFatRounds(45)) {
		t.Fatalf("first compaction should shrink history, got %d msgs", got)
	}
	firstSummary := ac.as.ConversationSummary()
	if firstSummary == "" {
		t.Fatalf("first compaction should produce a summary")
	}

	// 攒出新历史后再压：prompt 必须携带第一段摘要。
	fake.response = "合并后摘要"
	for _, m := range mkFatRounds(45) {
		ac.as.AppendConversationMessage(m)
	}
	ac.maybeCompactConversation("sys", nil, "user", "H-01", nil, nil, testLogger())
	last := fake.prompts[len(fake.prompts)-1]
	if !strings.Contains(last, firstSummary) {
		t.Fatalf("second compaction must carry the previous summary into the merge input:\n%s", last)
	}
}

// TestCompaction_InFlightGuardSkips pins fix 2: while a compaction is in
// flight, a concurrent caller skips (no second summarize, no stale
// overwrite) and retries on a later call.
func TestCompaction_InFlightGuardSkips(t *testing.T) {
	ac := tacticalCtxForTest(nil)
	block := make(chan struct{})
	fake := &compactFakeLLM{response: "摘要", block: block}
	ac.tacticalHc = fake
	for _, m := range mkFatRounds(45) {
		ac.as.AppendConversationMessage(m)
	}

	// 第一个调用阻塞在摘要 LLM 上（in-flight）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		ac.maybeCompactConversation("sys", nil, "user", "H-01", nil, nil, testLogger())
	}()
	// 等它真正进入 in-flight（标志已置位）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ac.compacting.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ac.compacting.Load() {
		t.Fatalf("first compaction should be in flight")
	}

	// 并发调用：跳过（不产生第二条 COMPACT-PROMPT）。
	ac.maybeCompactConversation("sys", nil, "user", "H-01", nil, nil, testLogger())
	fake.mu.Lock()
	prompts := len(fake.prompts)
	fake.mu.Unlock()
	if prompts != 1 {
		t.Fatalf("in-flight guard must skip the concurrent compaction, got %d prompts", prompts)
	}

	// 释放第一个调用 → 完成 + 标志复位。
	close(block)
	<-done
	if ac.compacting.Load() {
		t.Fatalf("flag must reset after completion")
	}
	// 复位后再攒历史可正常进入（上次压缩后 tail 在阈值下，不触发是正常的）。
	fake.block = nil
	for _, m := range mkFatRounds(45) {
		ac.as.AppendConversationMessage(m)
	}
	ac.maybeCompactConversation("sys", nil, "user", "H-01", nil, nil, testLogger())
	fake.mu.Lock()
	prompts = len(fake.prompts)
	fake.mu.Unlock()
	if prompts < 2 {
		t.Fatalf("post-release call should be allowed to compact again, prompts=%d", prompts)
	}
}
