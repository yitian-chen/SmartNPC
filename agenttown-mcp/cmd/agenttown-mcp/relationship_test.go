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
	seeded []storage.Relationship // captured seed calls (agentA, agentB, fam, aff)
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

// ensure time package is referenced (seedRelationshipsFromKB tests use time
// only indirectly via storage.Relationship, but keep the import active).
var _ = time.Time{}
