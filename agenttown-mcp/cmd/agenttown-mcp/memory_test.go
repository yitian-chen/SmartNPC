package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"log/slog"
)

// TestParseMemoryGenerationResult_Valid verifies a well-formed JSON response
// parses into narrative + memories.
func TestParseMemoryGenerationResult_Valid(t *testing.T) {
	raw := `{
  "narrative": "今天在车间装配了一整天，下午有些疲劳。",
  "memories": [
    {"type":"event","content":"完成装配任务","importance":60,"related_object_id":"workbench_01"},
    {"type":"skill","content":"学会使用新工具","importance":40}
  ]
}`
	result, err := parseMemoryGenerationResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Narrative != "今天在车间装配了一整天，下午有些疲劳。" {
		t.Errorf("narrative = %q", result.Narrative)
	}
	if len(result.Memories) != 2 {
		t.Fatalf("memories len = %d, want 2", len(result.Memories))
	}
	if result.Memories[0].Type != "event" || result.Memories[0].RelatedObjectID != "workbench_01" {
		t.Errorf("memories[0] = %+v", result.Memories[0])
	}
	if result.Memories[1].Importance != 40 {
		t.Errorf("memories[1] importance = %d, want 40", result.Memories[1].Importance)
	}
}

// TestParseMemoryGenerationResult_MarkdownFence verifies a response wrapped
// in ```json fence parses correctly.
func TestParseMemoryGenerationResult_MarkdownFence(t *testing.T) {
	raw := "```json\n" + `{"narrative":"总结","memories":[{"type":"daily_summary","content":"一天结束","importance":50}]}` + "\n```"
	result, err := parseMemoryGenerationResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Narrative != "总结" {
		t.Errorf("narrative = %q", result.Narrative)
	}
	if len(result.Memories) != 1 {
		t.Fatalf("memories len = %d, want 1", len(result.Memories))
	}
}

// TestParseMemoryGenerationResult_NoJSON verifies garbage input returns error.
func TestParseMemoryGenerationResult_NoJSON(t *testing.T) {
	_, err := parseMemoryGenerationResult("今天天气不错，没有 JSON 对象")
	if err == nil {
		t.Fatal("expected error for non-JSON input, got nil")
	}
}

// TestParseMemoryGenerationResult_TrailingProse verifies JSON with leading/trailing
// prose is extracted (finds first { and last }).
func TestParseMemoryGenerationResult_TrailingProse(t *testing.T) {
	raw := `好的，这是我的总结：
{"narrative":"完工","memories":[{"type":"event","content":"装配","importance":55}]}
希望对你有帮助。`
	result, err := parseMemoryGenerationResult(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if result.Narrative != "完工" {
		t.Errorf("narrative = %q", result.Narrative)
	}
}

// TestFormatActionHistoryForPrompt verifies the chronological numbered list format.
func TestFormatActionHistoryForPrompt(t *testing.T) {
	records := []storage.ActionRecord{
		{Cmd: "MoveTo", Params: map[string]any{"target": "workbench_01"}, Source: "tactical", Result: "success", StartedAt: time.Date(2026, 8, 10, 8, 30, 0, 0, time.UTC)},
		{Cmd: "WorkAtWorkbench", Params: map[string]any{"target_object_id": "workbench_01", "duration_sec": 3600}, Source: "tactical", Result: "success", StartedAt: time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)},
	}
	out := formatActionHistoryForPrompt(records)
	if !strings.Contains(out, "1. 08:30 MoveTo(target=workbench_01) [tactical] -> success") {
		t.Errorf("line 1 missing or wrong: %q", out)
	}
	if !strings.Contains(out, "2. 09:00 WorkAtWorkbench(") {
		t.Errorf("line 2 missing or wrong: %q", out)
	}
	if !strings.Contains(out, "[tactical] -> success") {
		t.Errorf("missing source/result marker: %q", out)
	}
}

// TestFormatActionHistoryForPrompt_Empty verifies empty input returns placeholder.
func TestFormatActionHistoryForPrompt_Empty(t *testing.T) {
	out := formatActionHistoryForPrompt(nil)
	if out != "（无行动记录）" {
		t.Errorf("empty = %q, want placeholder", out)
	}
}

// TestFormatParamsShort verifies params formatting + truncation.
func TestFormatParamsShort(t *testing.T) {
	if got := formatParamsShort(nil); got != "" {
		t.Errorf("nil params = %q, want empty", got)
	}
	if got := formatParamsShort(map[string]any{"target": "workbench_01"}); got != "target=workbench_01" {
		t.Errorf("simple params = %q", got)
	}
	// Truncation: long params should be cut at 80 chars + "...".
	longParams := map[string]any{"key": strings.Repeat("x", 100)}
	got := formatParamsShort(longParams)
	if len(got) > 84 { // 80 + "..." = 83, plus possible prefix variance
		t.Errorf("truncated params too long: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated params should end with ...: %q", got)
	}
}

// ─── generateDailyMemories ───────────────────────────────────────

// TestGenerateDailyMemories_NilStore verifies nil store returns "" without panic.
func TestGenerateDailyMemories_NilStore(t *testing.T) {
	sc := &fakeStrategicCaller{}
	out := generateDailyMemories(context.Background(), sc, nil, "H-01", nil, nil, slog.Default())
	if out != "" {
		t.Errorf("nil store = %q, want empty", out)
	}
}

// TestGenerateDailyMemories_EmptyHistory verifies store with no history returns "".
func TestGenerateDailyMemories_EmptyHistory(t *testing.T) {
	sc := &fakeStrategicCaller{}
	store := &memoryFakeStore{} // returns empty for LoadActionHistory
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default())
	if out != "" {
		t.Errorf("empty history = %q, want empty", out)
	}
}

// TestGenerateDailyMemories_LLMFail verifies LLM error returns "".
func TestGenerateDailyMemories_LLMFail(t *testing.T) {
	sc := &fakeStrategicCaller{err: errors.New("llm down")}
	store := &memoryFakeStore{
		actions: []storage.ActionRecord{{Cmd: "MoveTo", Source: "tactical", Result: "success", StartedAt: time.Now()}},
	}
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default())
	if out != "" {
		t.Errorf("llm fail = %q, want empty", out)
	}
}

// TestGenerateDailyMemories_Success verifies successful generation returns
// narrative and saves memories to store.
func TestGenerateDailyMemories_Success(t *testing.T) {
	raw := `{"narrative":"今天装配顺利","memories":[
		{"type":"event","content":"完成装配","importance":70,"related_object_id":"workbench_01"},
		{"type":"skill","content":"熟练使用工具","importance":45}
	]}`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	store := &memoryFakeStore{
		actions: []storage.ActionRecord{
			{Cmd: "WorkAtWorkbench", Source: "tactical", Result: "success", StartedAt: time.Now()},
		},
	}
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default())
	if out != "今天装配顺利" {
		t.Errorf("narrative = %q, want '今天装配顺利'", out)
	}
	if len(store.savedMemories) != 2 {
		t.Fatalf("saved memories = %d, want 2", len(store.savedMemories))
	}
	if store.savedMemories[0].MemoryType != "event" || store.savedMemories[0].Importance != 70 {
		t.Errorf("memory[0] = %+v", store.savedMemories[0])
	}
	// Importance 0 should default to 50.
	if store.savedMemories[1].Importance != 45 {
		t.Errorf("memory[1] importance = %d, want 45", store.savedMemories[1].Importance)
	}
}

// TestGenerateDailyMemories_ImportanceDefault verifies importance=0 defaults to 50.
func TestGenerateDailyMemories_ImportanceDefault(t *testing.T) {
	raw := `{"narrative":"总结","memories":[{"type":"event","content":"事件","importance":0}]}`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	store := &memoryFakeStore{
		actions: []storage.ActionRecord{{Cmd: "MoveTo", Source: "tactical", Result: "success", StartedAt: time.Now()}},
	}
	_ = generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default())
	if len(store.savedMemories) != 1 {
		t.Fatalf("saved = %d, want 1", len(store.savedMemories))
	}
	if store.savedMemories[0].Importance != 50 {
		t.Errorf("importance = %d, want 50 (default)", store.savedMemories[0].Importance)
	}
}

// ─── memoryFakeStore: in-memory Store for memory tests ────────────────

// memoryFakeStore is a minimal Store for memory.go tests. It only implements
// the memory/action_history methods; schedule-state methods are inherited from
// the agentstate fakeStore via embedding.
type memoryFakeStore struct {
	savedMemories []storage.Memory
	actions       []storage.ActionRecord
}

func (m *memoryFakeStore) LoadScheduleState(_ context.Context, _ string) (storage.ScheduleState, error) {
	return storage.ScheduleState{}, storage.ErrNotFound
}
func (m *memoryFakeStore) SaveScheduleState(_ context.Context, _ string, _ storage.ScheduleState) error {
	return nil
}
func (m *memoryFakeStore) SaveMemory(_ context.Context, _ string, mem storage.Memory) (int64, error) {
	m.savedMemories = append(m.savedMemories, mem)
	return int64(len(m.savedMemories)), nil
}
func (m *memoryFakeStore) LoadRecentMemories(_ context.Context, _ string, _ int) ([]storage.Memory, error) {
	return m.savedMemories, nil
}
func (m *memoryFakeStore) SaveActionRecord(_ context.Context, _ string, _ storage.ActionRecord) error {
	return nil
}
func (m *memoryFakeStore) LoadActionHistory(_ context.Context, _ string, _ int) ([]storage.ActionRecord, error) {
	return m.actions, nil
}
func (m *memoryFakeStore) SaveRelationship(_ context.Context, _ string, _ string, _ int, _ int) error {
	return nil
}
func (m *memoryFakeStore) LoadRelationships(_ context.Context, _ string, _ int) ([]storage.Relationship, error) {
	return nil, nil
}
func (m *memoryFakeStore) SeedRelationship(_ context.Context, _ string, _ string, _ int, _ int) error {
	return nil
}
func (m *memoryFakeStore) Close() error { return nil }
