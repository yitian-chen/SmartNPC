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
	out := generateDailyMemories(context.Background(), sc, nil, "H-01", nil, nil, slog.Default(), 1000, 1)
	if out != "" {
		t.Errorf("nil store = %q, want empty", out)
	}
}

// TestGenerateDailyMemories_EmptyHistory verifies store with no history returns "".
func TestGenerateDailyMemories_EmptyHistory(t *testing.T) {
	sc := &fakeStrategicCaller{}
	store := &memoryFakeStore{}
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default(), 1000, 1)
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
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default(), 1000, 1)
	if out != "" {
		t.Errorf("llm fail = %q, want empty", out)
	}
}

// TestGenerateDailyMemories_Success verifies the narrative is returned AND
// persisted as a daily_summary episode (game_time anchored).
func TestGenerateDailyMemories_Success(t *testing.T) {
	raw := `{"narrative":"今天装配顺利","memories":[
		{"type":"event","content":"完成装配","importance":70,"related_object_id":"workbench_01"}
	]}`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	store := &memoryFakeStore{
		actions: []storage.ActionRecord{
			{Cmd: "WorkAtWorkbench", Source: "tactical", Result: "success", StartedAt: time.Now()},
		},
	}
	out := generateDailyMemories(context.Background(), sc, store, "H-01", nil, nil, slog.Default(), 226800, 3)
	if out != "今天装配顺利" {
		t.Errorf("narrative = %q, want '今天装配顺利'", out)
	}
	if len(store.episodes) != 1 {
		t.Fatalf("episodes = %d, want 1 (daily_summary)", len(store.episodes))
	}
	ep := store.episodes[0]
	if ep.Category != "daily_summary" || ep.Summary != "今天装配顺利" || ep.GameTime != 226800 || ep.GameDay != 3 {
		t.Errorf("episode = %+v, want daily_summary + summary + game_time 226800 + day 3", ep)
	}
}

// TestPersistExtractedMemory verifies the compact structured-extraction write:
// episodes + people_updates land in the two tables (commitments are now
// extracted at conversation close via persistCommitments, not compact).
func TestPersistExtractedMemory(t *testing.T) {
	store := &memoryFakeStore{}
	ex := compactExtract{
		Summary: "在咖啡馆和老王下棋",
		Episodes: []compactEpisode{
			{Summary: "在咖啡馆和老王聊下棋", Category: "conversation", Importance: 7, Emotion: 0.6, Participants: []string{"npc_wang"}},
		},
		PeopleUpdates: []compactPersonUpd{
			{TargetID: "npc_wang", AffinityDelta: 2, Impression: "爱下棋的老邻居"},
		},
	}
	persistExtractedMemory(store, "H-01", ex, 226800, 3, slog.Default())
	if len(store.episodes) != 1 || store.episodes[0].Category != "conversation" {
		t.Errorf("episodes = %+v, want 1 conversation", store.episodes)
	}
	if len(store.people) != 1 || store.people[0].TargetID != "npc_wang" {
		t.Errorf("people = %+v, want npc_wang", store.people)
	}
	// nil store → no-op。
	persistExtractedMemory(nil, "H-01", ex, 226800, 3, slog.Default())
}

// TestPersistCommitments verifies the conversation-close commitment write:
// new commitments land in episodic_commitments, and the created list is returned.
func TestPersistCommitments(t *testing.T) {
	store := &memoryFakeStore{}
	created := persistCommitments(store, "H-01",
		[]compactCommitment{
			{Kind: "appointment", CounterpartID: "npc_wang", Content: "在公园棋桌和老王下棋", DueStartDay: 4, DueStartTime: "14:00", DueEndDay: 4, DueEndTime: "15:00", DueLocation: "zone_park"},
		},
		[]compactCmChange{{ID: "cm_old", Action: "done"}},
		226800, slog.Default())
	if len(created) != 1 || created[0].Kind != "appointment" {
		t.Errorf("created = %+v, want 1 appointment", created)
	}
	// D4 14:00 = (4-1)*86400 + 14*3600 = 259200 + 50400 = 309600。
	if created[0].DueStart != 309600 || created[0].DueEnd != 313200 {
		t.Errorf("due = %d~%d, want 309600~313200 (D4 14:00~15:00)", created[0].DueStart, created[0].DueEnd)
	}
	if len(store.commitments) != 1 || store.commitments[0].Content != "在公园棋桌和老王下棋" {
		t.Errorf("commitments = %+v, want the appointment", store.commitments)
	}
	// nil store → nil。
	if got := persistCommitments(nil, "H-01", nil, nil, 0, slog.Default()); got != nil {
		t.Errorf("nil store = %v, want nil", got)
	}
}

// TestDayTimeToGameSec pins the day+time → game-seconds conversion used by the
// commitment extraction (LLM outputs day+HH:MM, code computes seconds).
func TestDayTimeToGameSec(t *testing.T) {
	if got := dayTimeToGameSec(2, "16:00"); got != 144000 {
		t.Errorf("D2 16:00 = %d, want 144000", got)
	}
	if got := dayTimeToGameSec(1, "08:45"); got != 31500 {
		t.Errorf("D1 08:45 = %d, want 31500", got)
	}
	if got := dayTimeToGameSec(0, "16:00"); got != 0 {
		t.Errorf("day 0 = %d, want 0", got)
	}
	if got := dayTimeToGameSec(2, ""); got != 0 {
		t.Errorf("empty time = %d, want 0", got)
	}
}

// ─── memoryFakeStore: in-memory Store for memory tests ────────────────

// memoryFakeStore is a minimal Store for memory.go tests. It captures the
// three-table writes (episodes / commitments / people) + action_history.
type memoryFakeStore struct {
	episodes    []storage.Episode
	commitments []storage.Commitment
	people      []storage.Person
	actions     []storage.ActionRecord
}

func (m *memoryFakeStore) LoadScheduleState(_ context.Context, _ string) (storage.ScheduleState, error) {
	return storage.ScheduleState{}, storage.ErrNotFound
}
func (m *memoryFakeStore) SaveScheduleState(_ context.Context, _ string, _ storage.ScheduleState) error {
	return nil
}
func (m *memoryFakeStore) SaveEpisode(_ context.Context, ep storage.Episode) error {
	m.episodes = append(m.episodes, ep)
	return nil
}
func (m *memoryFakeStore) LoadEpisodes(_ context.Context, _ string, _ storage.EpisodeQuery) ([]storage.Episode, error) {
	return m.episodes, nil
}
func (m *memoryFakeStore) LoadLatestDailySummary(_ context.Context, _ string) (storage.Episode, error) {
	for i := len(m.episodes) - 1; i >= 0; i-- {
		if m.episodes[i].Category == "daily_summary" {
			return m.episodes[i], nil
		}
	}
	return storage.Episode{}, storage.ErrNotFound
}
func (m *memoryFakeStore) SaveCommitment(_ context.Context, cm storage.Commitment) error {
	m.commitments = append(m.commitments, cm)
	return nil
}
func (m *memoryFakeStore) UpdateCommitment(_ context.Context, _ string, _ string, _ int64, _ int64) error {
	return nil
}
func (m *memoryFakeStore) LoadCommitments(_ context.Context, _ string, _ storage.CommitmentQuery) ([]storage.Commitment, error) {
	return m.commitments, nil
}
func (m *memoryFakeStore) UpsertPerson(_ context.Context, p storage.Person, _ int) error {
	m.people = append(m.people, p)
	return nil
}
func (m *memoryFakeStore) SeedPerson(_ context.Context, p storage.Person) error {
	m.people = append(m.people, p)
	return nil
}
func (m *memoryFakeStore) LoadPeople(_ context.Context, _ string) ([]storage.Person, error) {
	return m.people, nil
}
func (m *memoryFakeStore) SaveActionRecord(_ context.Context, _ string, _ storage.ActionRecord) error {
	return nil
}
func (m *memoryFakeStore) LoadActionHistory(_ context.Context, _ string, _ int) ([]storage.ActionRecord, error) {
	return m.actions, nil
}
func (m *memoryFakeStore) Close() error { return nil }

func TestGenerateDailySocialNeed(t *testing.T) {
	// 正常解析 + schema 名。
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`{"social_need": 8}`)}
	if got := generateDailySocialNeed(context.Background(), sc, "H-01", nil, nil, slog.Default(), "今天的日程", 5); got != 8 {
		t.Fatalf("social need = %d, want 8", got)
	}
	if sc.capturedSchemaName != "social_need" {
		t.Errorf("schema name = %q, want social_need", sc.capturedSchemaName)
	}
	// LLM 失败 → 保持当前值。
	sc2 := &fakeStrategicCaller{err: errors.New("llm down")}
	if got := generateDailySocialNeed(context.Background(), sc2, "H-01", nil, nil, slog.Default(), "今天的日程", 5); got != 5 {
		t.Fatalf("social need on failure = %d, want 5 (unchanged)", got)
	}
	// 越界 → clamp 到 [0,10]。
	sc3 := &fakeStrategicCaller{resp: makeStrategicResponse(`{"social_need": 99}`)}
	if got := generateDailySocialNeed(context.Background(), sc3, "H-01", nil, nil, slog.Default(), "今天的日程", 5); got != 10 {
		t.Fatalf("social need after clamp-high = %d, want 10", got)
	}
	// 解析失败 → 保持当前值。
	sc4 := &fakeStrategicCaller{resp: makeStrategicResponse(`not-json`)}
	if got := generateDailySocialNeed(context.Background(), sc4, "H-01", nil, nil, slog.Default(), "今天的日程", 5); got != 5 {
		t.Fatalf("social need on parse failure = %d, want 5 (unchanged)", got)
	}
}
