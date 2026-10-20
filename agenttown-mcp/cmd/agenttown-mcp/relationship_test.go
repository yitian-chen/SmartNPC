package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
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

// relRosterKB is the multi-agent KB fixture for formatRelationshipsForPrompt:
// self H-01 老陈 + peers H-02 老王 / H-03 老李（无职业，职业来自 profile 时另建）。
func relRosterKB() *worldkb.KB {
	return &worldkb.KB{Agents: []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-02", DisplayName: "老王"},
		{ID: "H-03", DisplayName: "老李"},
	}}
}

func TestFormatRelationshipsForPrompt_Empty(t *testing.T) {
	if got := formatRelationshipsForPrompt(nil, nil, nil, "H-01"); got != "" {
		t.Errorf("nil kb = %q, want empty", got)
	}
	single := &worldkb.KB{Agents: []worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}}}
	if got := formatRelationshipsForPrompt(nil, single, nil, "H-01"); got != "" {
		t.Errorf("single-agent kb = %q, want empty", got)
	}
}

// TestFormatRelationshipsForPrompt_DefaultRoster pins the merged rendering:
// EVERY KB peer appears even with no stored rows (cold start / memory mode)
// — default affection 5, no 熟悉度/互动 paren, self excluded. Before the
// merged renderer an empty table silently hid the roster and affection from
// every layer.
func TestFormatRelationshipsForPrompt_DefaultRoster(t *testing.T) {
	got := formatRelationshipsForPrompt(nil, relRosterKB(), nil, "H-01")
	wantContains := []string{
		"- 老王（H-02）：好感 5/10",
		"- 老李（H-03）：好感 5/10",
	}
	for _, w := range wantContains {
		if !contains(got, w) {
			t.Errorf("output missing %q\ngot:\n%s", w, got)
		}
	}
	if strings.Contains(got, "熟悉度") || strings.Contains(got, "互动") {
		t.Errorf("default rows must not carry interaction history:\n%s", got)
	}
	if strings.Contains(got, "老陈") || strings.Contains(got, "（H-01") {
		t.Errorf("self must not appear in the roster:\n%s", got)
	}
}

// TestFormatRelationshipsForPrompt_ProfessionFromProfile pins the
// profile > KB fallback order for display name / profession.
func TestFormatRelationshipsForPrompt_ProfessionFromProfile(t *testing.T) {
	kb := &worldkb.KB{Agents: []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-02", DisplayName: "老王", Profession: "KB职业"},
		{ID: "H-03", DisplayName: "老李"},
	}}
	profiles := map[string]*profile.Profile{
		"H-02": {DisplayName: "老王", Profession: "物流分拣兼加工员"},
	}
	got := formatRelationshipsForPrompt(nil, kb, profiles, "H-01")
	if !contains(got, "- 老王（H-02，物流分拣兼加工员）：好感 5/10") {
		t.Errorf("H-02 should carry the profile profession (profile > KB):\n%s", got)
	}
	if !contains(got, "- 老李（H-03）：好感 5/10") {
		t.Errorf("H-03 (no profession anywhere) should render name+id only:\n%s", got)
	}
}

func TestFormatRelationshipsForPrompt_WithRels(t *testing.T) {
	rels := []storage.Relationship{
		{AgentA: "H-01", AgentB: "H-02", Familiarity: 5, Affection: 2, InteractionCount: 3},
		{AgentA: "H-03", AgentB: "H-01", Familiarity: 1, Affection: 0, InteractionCount: 1},
	}
	got := formatRelationshipsForPrompt(rels, relRosterKB(), nil, "H-01")
	// First row: H-01 is agent_a, other side is H-02.
	// Second row: H-01 is agent_b, other side is H-03.
	wantContains := []string{
		"- 老王（H-02）：好感 2/10（熟悉度 5、互动 3 次）",
		"- 老李（H-03）：好感 0/10（熟悉度 1、互动 1 次）",
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
			{From: "H-01", To: "H-02", Familiarity: 60, Affection: 8, Type: "colleague"},
			{From: "H-02", To: "H-01", Familiarity: 60, Affection: 8, Type: "colleague"},
			// H-01 ↔ H-03 (colleague)
			{From: "H-01", To: "H-03", Familiarity: 55, Affection: 7, Type: "colleague"},
			{From: "H-03", To: "H-01", Familiarity: 55, Affection: 7, Type: "colleague"},
			// H-02 ↔ H-03 (acquaintance)
			{From: "H-02", To: "H-03", Familiarity: 30, Affection: 6, Type: "acquaintance"},
			{From: "H-03", To: "H-02", Familiarity: 30, Affection: 6, Type: "acquaintance"},
		},
	}

	cases := []struct {
		registering string
		wantSeeded  []storage.Relationship
	}{
		{
			registering: "H-01",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-01", AgentB: "H-02", Familiarity: 60, Affection: 8},
				{AgentA: "H-01", AgentB: "H-03", Familiarity: 55, Affection: 7},
			},
		},
		{
			registering: "H-02",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-02", AgentB: "H-01", Familiarity: 60, Affection: 8},
				{AgentA: "H-02", AgentB: "H-03", Familiarity: 30, Affection: 6},
			},
		},
		{
			registering: "H-03",
			wantSeeded: []storage.Relationship{
				{AgentA: "H-03", AgentB: "H-01", Familiarity: 55, Affection: 7},
				{AgentA: "H-03", AgentB: "H-02", Familiarity: 30, Affection: 6},
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

// TestFormatRelationshipsForPrompt_DedupByOtherAgent pins fix #1: when
// LoadRelationships returns both A→B and B→A rows for the same pair
// (it queries WHERE agent_a=? OR agent_b=?), the prompt must show only
// ONE line per other agent — not two lines for the same peer
// (2026-09-18 仿真实测 duplication).
func TestFormatRelationshipsForPrompt_DedupByOtherAgent(t *testing.T) {
	kb := &worldkb.KB{Agents: []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-03", DisplayName: "老李"},
	}}
	rels := []storage.Relationship{
		{AgentA: "H-01", AgentB: "H-03", Familiarity: 1, Affection: 0, InteractionCount: 1},
		{AgentA: "H-03", AgentB: "H-01", Familiarity: 1, Affection: 0, InteractionCount: 1},
	}
	got := formatRelationshipsForPrompt(rels, kb, nil, "H-01")
	if strings.Count(got, "H-03") != 1 {
		t.Fatalf("should show exactly one line per other agent, got:\n%s", got)
	}
	// 双向时取更高值。
	rels = []storage.Relationship{
		{AgentA: "H-01", AgentB: "H-03", Familiarity: 1, Affection: 0, InteractionCount: 1},
		{AgentA: "H-03", AgentB: "H-01", Familiarity: 3, Affection: 2, InteractionCount: 5},
	}
	got = formatRelationshipsForPrompt(rels, kb, nil, "H-01")
	if !strings.Contains(got, "好感 2/10（熟悉度 3、互动 5 次）") {
		t.Fatalf("should show the higher values, got:\n%s", got)
	}
	// 多个不同 agent 不误并。
	rels = []storage.Relationship{
		{AgentA: "H-01", AgentB: "H-02", Familiarity: 2, Affection: 1, InteractionCount: 3},
		{AgentA: "H-01", AgentB: "H-03", Familiarity: 1, Affection: 0, InteractionCount: 1},
	}
	got = formatRelationshipsForPrompt(rels, relRosterKB(), nil, "H-01")
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("two distinct agents → two lines, got:\n%s", got)
	}
}

// ─── 好感度：会话累积 / 结束检测 / delta 解析 ──────────────

func TestConversationSession_AccumulateAndClose(t *testing.T) {
	ac := &agentContext{}
	ac.recordConversationTurn("H-01", "H-02", "老王，今天干啥？", 100)
	ac.recordConversationTurn("H-02", "H-02", "老陈，分拣呢。", 120)

	// 未到空闲超时：不关闭。
	if peer, turns := ac.closeConversationIfIdle(100 + conversationIdleTimeoutGameSec - 1); peer != "" || turns != nil {
		t.Fatalf("should not close before idle timeout, got peer=%q turns=%v", peer, turns)
	}
	// 到点关闭：返回 peer + 完整 transcript。
	peer, turns := ac.closeConversationIfIdle(120 + conversationIdleTimeoutGameSec)
	if peer != "H-02" || len(turns) != 2 {
		t.Fatalf("close: peer=%q turns=%v, want H-02 + 2 turns", peer, turns)
	}
	if turns[0].speakerID != "H-01" || turns[0].content != "老王，今天干啥？" {
		t.Fatalf("turn[0] = %+v, want H-01 开场白", turns[0])
	}
	// 关闭后清空。
	if peer2, _ := ac.closeConversationIfIdle(9999); peer2 != "" {
		t.Fatalf("after close should be empty, got peer=%q", peer2)
	}
}

func TestConversationSession_PeerChangeClears(t *testing.T) {
	ac := &agentContext{}
	ac.recordConversationTurn("H-01", "H-02", "老王", 100)
	ac.recordConversationTurn("H-01", "H-03", "老李", 200) // 换人：旧 H-02 会话丢弃
	ac.recordConversationTurn("H-03", "H-03", "老陈？", 220)

	peer, turns := ac.closeConversationIfIdle(220 + conversationIdleTimeoutGameSec)
	if peer != "H-03" || len(turns) != 2 {
		t.Fatalf("peer change: peer=%q turns=%v, want H-03 + 2 turns (H-02 session discarded)", peer, turns)
	}
}

func TestConversationSession_Clear(t *testing.T) {
	ac := &agentContext{}
	ac.recordConversationTurn("H-01", "H-02", "老王", 100)
	ac.clearConversation()
	if peer, _ := ac.closeConversationIfIdle(9999); peer != "" {
		t.Fatalf("cleared session should be empty, got peer=%q", peer)
	}
}

func TestParseAffectionDelta(t *testing.T) {
	if d, err := parseAffectionDelta(`{"delta": 2}`); err != nil || d != 2 {
		t.Fatalf("parse delta=2: got %d err=%v", d, err)
	}
	if d, err := parseAffectionDelta(`{"delta": -3}`); err != nil || d != -3 {
		t.Fatalf("parse delta=-3: got %d err=%v", d, err)
	}
	if _, err := parseAffectionDelta(`not-json`); err == nil {
		t.Fatal("parse invalid should error")
	}
}

// ─── conversationContextForPrompt ─────────────────────────────

// convKB 构建带索引的测试 KB（NewKB 会 buildIndex——struct 字面量直接
// 构造不建索引，GetAgent 查不到）。
func convKB() *worldkb.KB {
	return worldkb.NewKB(nil, nil, []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-02", DisplayName: "老王"},
		{ID: "H-03", DisplayName: "老李"},
	})
}

// TestConversationContextForPrompt_NoSession verifies no active conversation
// yields an empty block and no cap.
func TestConversationContextForPrompt_NoSession(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	if ctx, capped := ac.conversationContextForPrompt("H-01", convKB()); ctx != "" || capped {
		t.Errorf("no session = (%q, %v), want empty/uncapped", ctx, capped)
	}
}

// TestConversationContextForPrompt_Transcript verifies the block renders the
// peer header with round count, own turns as "你", peer turns by KB display
// name, and reports uncapped below the turn cap.
func TestConversationContextForPrompt_Transcript(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	// H-02（老王）视角的会话：对方的话由 speech_heard 记录（speaker=peer），
	// 自己的话由 speak_to 下发记录（speaker=self, peer=target）。
	ac.recordConversationTurn("H-01", "H-01", "老王，一起去晨跑？", 100)
	ac.recordConversationTurn("H-02", "H-01", "行啊，正好活动活动。", 200)
	ac.recordConversationTurn("H-01", "H-01", "那走起？", 300)

	ctx, capped := ac.conversationContextForPrompt("H-02", convKB())
	if capped {
		t.Errorf("3 turns must not be capped")
	}
	for _, want := range []string{
		"【对话记录】（与 老陈（H-01）的当前对话，已 3 轮）",
		"老陈：老王，一起去晨跑？",
		"你：行啊，正好活动活动。",
		"老陈：那走起？",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("missing %q in:\n%s", want, ctx)
		}
	}
}

// TestConversationContextForPrompt_CappedAtLimit verifies the cap fires at
// maxConversationTurns (6 utterances = 3 full round-trips).
func TestConversationContextForPrompt_CappedAtLimit(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	for i := 0; i < maxConversationTurns; i++ {
		speaker := "H-01"
		if i%2 == 1 {
			speaker = "H-02"
		}
		ac.recordConversationTurn(speaker, "H-01", fmt.Sprintf("第 %d 句", i+1), float64(100+i))
	}
	if _, capped := ac.conversationContextForPrompt("H-02", convKB()); !capped {
		t.Errorf("%d turns must be capped", maxConversationTurns)
	}
	// 上限前一轮不封顶。
	ac2, _ := newAgentContext(context.Background())
	for i := 0; i < maxConversationTurns-1; i++ {
		speaker := "H-01"
		if i%2 == 1 {
			speaker = "H-02"
		}
		ac2.recordConversationTurn(speaker, "H-01", fmt.Sprintf("第 %d 句", i+1), float64(100+i))
	}
	if _, capped := ac2.conversationContextForPrompt("H-02", convKB()); capped {
		t.Errorf("%d turns must not be capped", maxConversationTurns-1)
	}
}

// TestConversationContextForPrompt_PeerChangeResets mirrors
// recordConversationTurn's session semantics: switching peers drops the old
// transcript (abandoned conversation, not evaluated).
func TestConversationContextForPrompt_PeerChangeResets(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.recordConversationTurn("H-02", "H-02", "老王，一起去晨跑？", 100)
	ac.recordConversationTurn("H-03", "H-03", "老李，装配台那边见？", 200)
	ctx, _ := ac.conversationContextForPrompt("H-01", convKB())
	if strings.Contains(ctx, "一起去晨跑") {
		t.Errorf("old peer transcript must be dropped on peer change:\n%s", ctx)
	}
	if !strings.Contains(ctx, "老李，装配台那边见？") {
		t.Errorf("new peer transcript missing:\n%s", ctx)
	}
}

// TestConversationEndedHint verifies the post-conversation hint: full
// transcript (own turns as "你") + the follow-through directive that carries
// the agreed activity past the conversation (single-action speak_to design:
// the agreement rides this hint, not the conversation replan).
func TestConversationEndedHint(t *testing.T) {
	turns := []conversationTurn{
		{speakerID: "H-01", content: "老王，一起去晨跑？"},
		{speakerID: "H-02", content: "行啊，正好活动活动。"},
	}
	got := conversationEndedHint("H-02", "H-01", turns, convKB())
	for _, want := range []string{
		"【对话结束】你与 老陈（H-01）的对话刚刚结束",
		"老陈：老王，一起去晨跑？",
		"你：行啊，正好活动活动。",
		"若对话中你们约定了一起做某事",
		"守约优先于当前时段目标",
		"不要因为时段切换而弃约",
		// 时间免疫声明（2026-10-20 17:17 实测：对话往返耗 33 游戏分钟，
		// 约定盖着 07:47 的戳、执行时已 08:14——LLM 以"时间已过"为由把
		// 约定叙述为"晨跑结束了"跑去分拣）。
		"约定刚刚达成、此刻仍然有效",
		"禁止以\"时间已过\"为理由把约定叙述为已发生",
		"不算执行",
		"长动作不可省略",
		"若你的邀请被婉拒或双方只是寒暄/告别",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// 无轮次 → 空串（不注入）。
	if got := conversationEndedHint("H-02", "H-01", nil, convKB()); got != "" {
		t.Errorf("no turns = %q, want empty", got)
	}
	if got := conversationEndedHint("H-02", "", turns, convKB()); got != "" {
		t.Errorf("no peer = %q, want empty", got)
	}
}

// seedConversationSession 为 agentID 播种一个与 peer 的进行中会话
// （invite heard + own acceptance 两轮），返回所用的游戏时间基准。
func seedConversationSession(t *testing.T, ac *agentContext, agentID, peer string) {
	t.Helper()
	ac.recordConversationTurn(peer, peer, "走，一起去跑步机晨跑？", 1000)
	ac.recordConversationTurn(agentID, peer, "行，跟你一起跑几步。", 1010)
}

// TestRecordActionCompletion_NoReplyClosesConversationAndSetsHint 验证对话终局
// 即时判定：speak_to 以 no_reply（result=success + reason 含 no_reply）完成时
// 立即关会话 + 注入守约 hint + 清社交反应窗口——不等 20 游戏分钟的 idle 超时
// （2026-10-20 实测：应邀方应下后，立即 refill 被 armed hint 的"对方无回音
// 则回日程"推回车间，守约 hint 晚 6s 才注入）。
func TestRecordActionCompletion_NoReplyClosesConversationAndSetsHint(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	seedConversationSession(t, ac, "H-01", "H-05")
	// 武装社交反应窗口（社交回应 replan 开启的 armed 窗口）。
	ac.beginReaction(2, 1010, "社交信号：老张邀请", true)
	if ac.reactionDescSnapshot() == "" {
		t.Fatal("reaction window should be armed")
	}
	// 在途 speak_to（对会话 peer）。
	ac.as.RecordActionStarted("act-acc", "SpeakTo",
		map[string]any{"target_agent_id": "H-05", "content": "行，跟你一起跑几步。", "interrupt": true},
		agentstate.SourceTactical, "tc-1")

	_, _ = ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-acc", Result: "success", Reason: "speech:no_reply", DurationMs: 10000,
	}, nil, nil, nil)

	// 会话已关、守约 hint 已落、社交窗口已清。
	if peer, turns := ac.conversationPeerState(); peer != "" || turns != 0 {
		t.Errorf("session should be closed, got peer=%q turns=%d", peer, turns)
	}
	hint := ac.as.ReplanHint()
	if !strings.HasPrefix(hint, "【对话结束】") {
		t.Fatalf("replan hint = %q, want 【对话结束】 prefix", hint[:min(len(hint), 40)])
	}
	if !strings.Contains(hint, "跑步机") || !strings.Contains(hint, "守约") {
		t.Errorf("hint should carry the transcript + follow-through directive: %q", hint[:min(len(hint), 120)])
	}
	if d := ac.reactionDescSnapshot(); d != "" {
		t.Errorf("reaction window should be cleared, got desc=%q", d)
	}
}

// TestRecordActionCompletion_NoReplyPeerMismatchKeepsSession 验证 peer 不匹配
// （speak_to 目标非当前会话对方，如陈旧会话残留）时不立即关闭——走原
// idle-close 兜底路径。
func TestRecordActionCompletion_NoReplyPeerMismatchKeepsSession(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	seedConversationSession(t, ac, "H-01", "H-05") // 会话对方 H-05
	ac.as.RecordActionStarted("act-x", "SpeakTo",
		map[string]any{"target_agent_id": "H-03", "content": "老李，聊两句？"},
		agentstate.SourceTactical, "tc-1")

	_, _ = ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-x", Result: "success", Reason: "speech:no_reply", DurationMs: 10000,
	}, nil, nil, nil)

	if peer, turns := ac.conversationPeerState(); peer != "H-05" || turns != 2 {
		t.Errorf("H-05 session must stay intact, got peer=%q turns=%d", peer, turns)
	}
	if hint := ac.as.ReplanHint(); strings.HasPrefix(hint, "【对话结束】") {
		t.Errorf("no hint should be injected on peer mismatch")
	}
}

// TestRecordActionCompletion_NoReplyVariantsKeepSession 验证非 no_reply 终局
// （普通 success 无 reason、interrupted）不触发立即关闭——speak_to 被回话打断
// 或被 stop 的路径交给 idle-close / 社交 replan 处理。
func TestRecordActionCompletion_NoReplyVariantsKeepSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		reason string
	}{
		{"普通 success 无 reason", "success", ""},
		{"interrupted（被回话/stop 打断）", "interrupted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ac, _ := newAgentContext(context.Background())
			ac.as.SetIdentity("H-01", nil)
			seedConversationSession(t, ac, "H-01", "H-05")
			ac.as.RecordActionStarted("act-a", "SpeakTo",
				map[string]any{"target_agent_id": "H-05", "content": "行。"},
				agentstate.SourceTactical, "tc-1")

			_, _ = ac.recordActionCompletion(protocol.ActionCompletedPayload{
				ActionID: "act-a", Result: tc.result, Reason: tc.reason, DurationMs: 5000,
			}, nil, nil, nil)

			if peer, turns := ac.conversationPeerState(); peer != "H-05" || turns != 2 {
				t.Errorf("session must stay intact (%s), got peer=%q turns=%d", tc.name, peer, turns)
			}
		})
	}
}

// TestConversationCarryover_ReplyAfterCloseContinuesTurns 验证会话续接：no_reply
// 终局关闭后同一 peer 的应答在宽限窗内到达，旧轮次续接进新会话——transcript
// 保持完整（邀请 + 应答），社交 replan 不再看到一句没有前文的孤立发言
// （2026-10-20 16:27 实测：邀请 no_reply 与应答送达同秒，会话重开只剩应答
// 1 轮，约定上下文被劈碎、邀请方回了车间）。
func TestConversationCarryover_ReplyAfterCloseContinuesTurns(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	ac.recordConversationTurn("H-01", "H-05", "老张，走，一起去跑步机晨跑？", 1000) // 己方邀请
	if peer, turns := ac.closeConversationByPeer("H-05"); peer != "H-05" || len(turns) != 1 {
		t.Fatalf("closeConversationByPeer = %q,%d turns", peer, len(turns))
	}
	// H-05 的应答到达（heard 记录）：同 peer、宽限窗内 → 续接。
	ac.recordConversationTurn("H-05", "H-05", "好嘞，一起跑两圈。", 1010)
	if peer, turns := ac.conversationPeerState(); peer != "H-05" || turns != 2 {
		t.Fatalf("carried session should hold 2 turns, got peer=%q turns=%d", peer, turns)
	}
	ctx, _ := ac.conversationContextForPrompt("H-01", nil)
	if !strings.Contains(ctx, "一起去跑步机晨跑") || !strings.Contains(ctx, "好嘞，一起跑两圈") {
		t.Errorf("transcript must contain both invite and reply:\n%s", ctx)
	}
}

// TestConversationCarryover_IdleClosePathAlsoCarries 验证 idle 关闭路径同样
// 续接（与 no_reply 关闭共用 closeConversationLocked 快照）。
func TestConversationCarryover_IdleClosePathAlsoCarries(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	ac.recordConversationTurn("H-01", "H-05", "邀请", 1000)
	if peer, _ := ac.closeConversationIfIdle(1000 + conversationIdleTimeoutGameSec + 1); peer != "H-05" {
		t.Fatalf("idle close failed: peer=%q", peer)
	}
	ac.recordConversationTurn("H-05", "H-05", "应答", 2000)
	if _, turns := ac.conversationPeerState(); turns != 2 {
		t.Errorf("idle-closed session should carry over on reply, turns=%d", turns)
	}
}

// TestConversationCarryover_WindowExpiryNoCarry 验证宽限窗外的新发言不续接
// （开新会话）。窗口临时置为负值模拟过期。
func TestConversationCarryover_WindowExpiryNoCarry(t *testing.T) {
	orig := conversationCarryoverWindow
	conversationCarryoverWindow = -time.Second
	defer func() { conversationCarryoverWindow = orig }()

	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	ac.recordConversationTurn("H-01", "H-05", "邀请", 1000)
	ac.closeConversationByPeer("H-05")
	ac.recordConversationTurn("H-05", "H-05", "迟到的应答", 2000)
	if _, turns := ac.conversationPeerState(); turns != 1 {
		t.Errorf("expired window must start a fresh session, turns=%d", turns)
	}
}

// TestConversationCarryover_DifferentPeerFreshSession 验证不同 peer 的新发言
// 不续接旧会话（新对话从零开始）。
func TestConversationCarryover_DifferentPeerFreshSession(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	ac.recordConversationTurn("H-01", "H-05", "邀请", 1000)
	ac.closeConversationByPeer("H-05")
	ac.recordConversationTurn("H-03", "H-03", "老李搭话", 1010)
	if peer, turns := ac.conversationPeerState(); peer != "H-03" || turns != 1 {
		t.Errorf("different peer starts fresh, got peer=%q turns=%d", peer, turns)
	}
}

// TestConversationCarryover_ActiveSessionNotDisturbed 验证会话进行中（同
// peer、已有轮次）时记录不触发续接——快照只在会话重开（无轮次起始）时消费。
func TestConversationCarryover_ActiveSessionNotDisturbed(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.SetIdentity("H-01", nil)
	ac.recordConversationTurn("H-01", "H-02", "旧对话", 1000)
	ac.closeConversationByPeer("H-02") // 快照 = [旧对话]
	// 新会话与 H-05（非快照 peer）进行中。
	ac.recordConversationTurn("H-05", "H-05", "新对话第一句", 2000)
	ac.recordConversationTurn("H-01", "H-05", "新对话第二句", 2010)
	if peer, turns := ac.conversationPeerState(); peer != "H-05" || turns != 2 {
		t.Errorf("active H-05 session must stay at 2 turns, got peer=%q turns=%d", peer, turns)
	}
}
