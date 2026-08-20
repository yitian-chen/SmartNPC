package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"log/slog"
)

// ─── parseRelationshipJudgeResponse ──────────────────────────────

func TestParseRelationshipJudgeResponse_Yes(t *testing.T) {
	cases := []string{"yes", "Yes", "YES", "yes.", "yes\n", "  yes  "}
	for _, in := range cases {
		if got := parseRelationshipJudgeResponse(in); !got {
			t.Errorf("parseRelationshipJudgeResponse(%q) = false, want true", in)
		}
	}
}

func TestParseRelationshipJudgeResponse_No(t *testing.T) {
	cases := []string{"no", "No", "n", "negative", "否"}
	for _, in := range cases {
		if got := parseRelationshipJudgeResponse(in); got {
			t.Errorf("parseRelationshipJudgeResponse(%q) = true, want false", in)
		}
	}
}

func TestParseRelationshipJudgeResponse_Garbage(t *testing.T) {
	cases := []string{"", "   ", "the action is social", "123", "{}"}
	for _, in := range cases {
		if got := parseRelationshipJudgeResponse(in); got {
			t.Errorf("parseRelationshipJudgeResponse(%q) = true, want false (garbage)", in)
		}
	}
}

// ─── formatRelationshipsForPrompt ────────────────────────────────

func TestFormatRelationshipsForPrompt_Empty(t *testing.T) {
	if got := formatRelationshipsForPrompt(nil, "H-01"); got != "" {
		t.Errorf("empty slice = %q, want empty", got)
	}
}

func TestFormatRelationshipsForPrompt_WithRels(t *testing.T) {
	rels := []storage.Relationship{
		{AgentA: "H-01", AgentB: "H-02", Familiarity: 5, Affection: 2, InteractionCount: 3},
		{AgentA: "H-03", AgentB: "H-01", Familiarity: 1, Affection: 0, InteractionCount: 1},
	}
	got := formatRelationshipsForPrompt(rels, "H-01")
	// First row: H-01 is agent_a, other side is H-02.
	// Second row: H-01 is agent_b, other side is H-03.
	wantContains := []string{
		"与 H-02：熟悉度 5、好感 2（互动 3 次）",
		"与 H-03：熟悉度 1、好感 0（互动 1 次）",
	}
	for _, w := range wantContains {
		if !contains(got, w) {
			t.Errorf("output missing %q\ngot:\n%s", w, got)
		}
	}
}

// contains is a minimal substring helper (avoid pulling strings.Contains into
// the production code just for tests).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// ─── seedRelationshipsFromKB ─────────────────────────────────────

// relationshipFakeStore is a minimal Store for relationship.go tests. It
// captures SeedRelationship calls so tests can assert which rows were seeded.
type relationshipFakeStore struct {
	seeded  []storage.Relationship // captured seed calls (agentA, agentB, fam, aff)
	seedErr error
}

func (s *relationshipFakeStore) LoadScheduleState(_ context.Context, _ string) (storage.ScheduleState, error) {
	return storage.ScheduleState{}, storage.ErrNotFound
}
func (s *relationshipFakeStore) SaveScheduleState(_ context.Context, _ string, _ storage.ScheduleState) error {
	return nil
}
func (s *relationshipFakeStore) SaveMemory(_ context.Context, _ string, _ storage.Memory) (int64, error) {
	return 0, nil
}
func (s *relationshipFakeStore) LoadRecentMemories(_ context.Context, _ string, _ int) ([]storage.Memory, error) {
	return nil, nil
}
func (s *relationshipFakeStore) SaveActionRecord(_ context.Context, _ string, _ storage.ActionRecord) error {
	return nil
}
func (s *relationshipFakeStore) LoadActionHistory(_ context.Context, _ string, _ int) ([]storage.ActionRecord, error) {
	return nil, nil
}
func (s *relationshipFakeStore) SaveRelationship(_ context.Context, _ string, _ string, _ int, _ int) error {
	return nil
}
func (s *relationshipFakeStore) LoadRelationships(_ context.Context, _ string, _ int) ([]storage.Relationship, error) {
	return nil, nil
}
func (s *relationshipFakeStore) SeedRelationship(_ context.Context, agentA, agentB string, fam, aff int) error {
	if s.seedErr != nil {
		return s.seedErr
	}
	s.seeded = append(s.seeded, storage.Relationship{
		AgentA: agentA, AgentB: agentB, Familiarity: fam, Affection: aff,
	})
	return nil
}
func (s *relationshipFakeStore) SaveDialogue(_ context.Context, _ storage.Dialogue) error {
	return nil
}
func (s *relationshipFakeStore) LoadRecentDialogues(_ context.Context, _ string, _ int) ([]storage.Dialogue, error) {
	return nil, nil
}
func (s *relationshipFakeStore) Close() error { return nil }

func TestSeedRelationshipsFromKB_NilStore(t *testing.T) {
	// nil store → no-op, no panic.
	if err := seedRelationshipsFromKB(context.Background(), &worldkb.KB{}, nil, "H-01", slog.Default()); err != nil {
		t.Errorf("nil store: got err=%v, want nil", err)
	}
}

func TestSeedRelationshipsFromKB_NilKB(t *testing.T) {
	store := &relationshipFakeStore{}
	if err := seedRelationshipsFromKB(context.Background(), nil, store, "H-01", slog.Default()); err != nil {
		t.Errorf("nil kb: got err=%v, want nil", err)
	}
	if len(store.seeded) != 0 {
		t.Errorf("nil kb: got %d seeded, want 0", len(store.seeded))
	}
}

func TestSeedRelationshipsFromKB_EmptyRelationships(t *testing.T) {
	store := &relationshipFakeStore{}
	kb := &worldkb.KB{Agents: []worldkb.Agent{{ID: "H-01"}}}
	if err := seedRelationshipsFromKB(context.Background(), kb, store, "H-01", slog.Default()); err != nil {
		t.Errorf("empty rels: got err=%v, want nil", err)
	}
	if len(store.seeded) != 0 {
		t.Errorf("empty rels: got %d seeded, want 0", len(store.seeded))
	}
}

func TestSeedRelationshipsFromKB_Success(t *testing.T) {
	store := &relationshipFakeStore{}
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{{ID: "H-01"}, {ID: "H-02"}},
		Relationships: []worldkb.Relationship{
			{From: "H-01", To: "H-02", Familiarity: 5, Affection: 1, Type: "colleague"},
			{From: "H-02", To: "H-03", Familiarity: 3, Affection: 0, Type: "colleague"}, // doesn't involve H-01
		},
	}
	if err := seedRelationshipsFromKB(context.Background(), kb, store, "H-01", slog.Default()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Only the H-01→H-02 relationship involves H-01 (as From).
	// The H-02→H-03 relationship doesn't involve H-01, so it's skipped.
	if len(store.seeded) != 1 {
		t.Fatalf("got %d seeded, want 1", len(store.seeded))
	}
	got := store.seeded[0]
	if got.AgentA != "H-01" || got.AgentB != "H-02" {
		t.Errorf("seeded %s→%s, want H-01→H-02", got.AgentA, got.AgentB)
	}
	if got.Familiarity != 5 || got.Affection != 1 {
		t.Errorf("seeded fam=%d aff=%d, want fam=5 aff=1", got.Familiarity, got.Affection)
	}
}

func TestSeedRelationshipsFromKB_ReverseDirection(t *testing.T) {
	// KB declares H-02→H-01; when H-01 registers, it should seed H-01→H-02
	// (agentID as agent_a, other as agent_b) with the declared familiarity.
	store := &relationshipFakeStore{}
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{{ID: "H-01"}, {ID: "H-02"}},
		Relationships: []worldkb.Relationship{
			{From: "H-02", To: "H-01", Familiarity: 4, Affection: 2},
		},
	}
	if err := seedRelationshipsFromKB(context.Background(), kb, store, "H-01", slog.Default()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(store.seeded) != 1 {
		t.Fatalf("got %d seeded, want 1", len(store.seeded))
	}
	got := store.seeded[0]
	if got.AgentA != "H-01" || got.AgentB != "H-02" {
		t.Errorf("reverse: seeded %s→%s, want H-01→H-02 (agentID as agent_a)", got.AgentA, got.AgentB)
	}
	if got.Familiarity != 4 || got.Affection != 2 {
		t.Errorf("reverse: seeded fam=%d aff=%d, want fam=4 aff=2", got.Familiarity, got.Affection)
	}
}

func TestSeedRelationshipsFromKB_PartialError(t *testing.T) {
	// Per-row error should log warn and continue with the next row.
	store := &relationshipFakeStore{seedErr: errors.New("db down")}
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{{ID: "H-01"}, {ID: "H-02"}, {ID: "H-03"}},
		Relationships: []worldkb.Relationship{
			{From: "H-01", To: "H-02", Familiarity: 1},
			{From: "H-01", To: "H-03", Familiarity: 2},
		},
	}
	// All seed calls fail, but the function should not return an error
	// (best-effort: logs warn per row, continues).
	if err := seedRelationshipsFromKB(context.Background(), kb, store, "H-01", slog.Default()); err != nil {
		t.Errorf("partial error: got err=%v, want nil (best-effort)", err)
	}
}

// ─── shouldUpdateRelationship nil-client guard ───────────────────

func TestShouldUpdateRelationship_NilClient(t *testing.T) {
	// nil Ollama client → false, no panic.
	got := shouldUpdateRelationship(context.Background(), nil, "ChatWith",
		map[string]any{"target_agent_id": "H-02"}, "H-02")
	if got {
		t.Errorf("nil client = true, want false")
	}
}

// TestSeedRelationshipsFromKB_ThreeNPCColdStart 验证 3 NPC 冷启动场景下，
// 每个 agent 注册时都正确种子涉及自己的所有 KB 关系行。
// KB 形态对齐 assets/world_kb.yaml：6 条关系 = 3 对双向（H-01↔H-02、H-01↔H-03、H-02↔H-03）。
// 每个 agent 注册时应种子 2 条出向行（agentID 作为 agent_a）。
func TestSeedRelationshipsFromKB_ThreeNPCColdStart(t *testing.T) {
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{{ID: "H-01"}, {ID: "H-02"}, {ID: "H-03"}},
		Relationships: []worldkb.Relationship{
			// H-01 ↔ H-02 (colleague)
			{From: "H-01", To: "H-02", Familiarity: 60, Affection: 50, Type: "colleague"},
			{From: "H-02", To: "H-01", Familiarity: 60, Affection: 50, Type: "colleague"},
			// H-01 ↔ H-03 (colleague)
			{From: "H-01", To: "H-03", Familiarity: 55, Affection: 45, Type: "colleague"},
			{From: "H-03", To: "H-01", Familiarity: 55, Affection: 45, Type: "colleague"},
			// H-02 ↔ H-03 (acquaintance)
			{From: "H-02", To: "H-03", Familiarity: 30, Affection: 35, Type: "acquaintance"},
			{From: "H-03", To: "H-02", Familiarity: 30, Affection: 35, Type: "acquaintance"},
		},
	}

	cases := []struct {
		registering string
		wantSeeded  []storage.Relationship
	}{
		{
			registering: "H-01",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-01", AgentB: "H-02", Familiarity: 60, Affection: 50},
				{AgentA: "H-01", AgentB: "H-03", Familiarity: 55, Affection: 45},
			},
		},
		{
			registering: "H-02",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-02", AgentB: "H-01", Familiarity: 60, Affection: 50},
				{AgentA: "H-02", AgentB: "H-03", Familiarity: 30, Affection: 35},
			},
		},
		{
			registering: "H-03",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-03", AgentB: "H-01", Familiarity: 55, Affection: 45},
				{AgentA: "H-03", AgentB: "H-02", Familiarity: 30, Affection: 35},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.registering, func(t *testing.T) {
			store := &relationshipFakeStore{}
			if err := seedRelationshipsFromKB(context.Background(), kb, store, tc.registering, slog.Default()); err != nil {
				t.Fatalf("seed %s: %v", tc.registering, err)
			}
			if len(store.seeded) != len(tc.wantSeeded) {
				t.Fatalf("%s: got %d seeded, want %d; seeded=%+v",
					tc.registering, len(store.seeded), len(tc.wantSeeded), store.seeded)
			}
			// 顺序匹配：seedRelationshipsFromKB 按 KB Relationships 数组顺序遍历，
			// 所以 wantSeeded 的顺序应与 KB 中涉及该 agent 的关系出现顺序一致。
			for i, want := range tc.wantSeeded {
				got := store.seeded[i]
				if got.AgentA != want.AgentA || got.AgentB != want.AgentB {
					t.Errorf("%s seeded[%d] = %s→%s, want %s→%s",
						tc.registering, i, got.AgentA, got.AgentB, want.AgentA, want.AgentB)
				}
				if got.Familiarity != want.Familiarity || got.Affection != want.Affection {
					t.Errorf("%s seeded[%d] fam/aff = %d/%d, want %d/%d",
						tc.registering, i, got.Familiarity, got.Affection, want.Familiarity, want.Affection)
				}
			}
		})
	}
}

// ensure time package is referenced (seedRelationshipsFromKB tests use time
// only indirectly via storage.Relationship, but keep the import active).
var _ = time.Time{}
