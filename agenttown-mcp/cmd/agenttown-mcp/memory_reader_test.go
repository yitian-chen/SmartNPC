package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

// TestMemoryReader_RecentEpisodes pins the read-memory internal tool's
// 【过往经验】 rendering: bullet list with category annotation; nil store
// (in-memory mode) → empty. The reader is program-only — the LLM never calls
// it (it is not in the function-calling tools list).
func TestMemoryReader_RecentEpisodes(t *testing.T) {
	r := newMemoryReader(&memoryFakeStore{episodes: []storage.Episode{
		{Summary: "今天和老王下了盘棋", Category: "conversation"},
		{Summary: "昨天熬夜上网", Category: "daily_summary"},
	}})
	got := r.recentEpisodes(context.Background(), "H-01", 3)
	want := "- 今天和老王下了盘棋（conversation）\n- 昨天熬夜上网（daily_summary）"
	if got != want {
		t.Errorf("recentEpisodes =\n%q\nwant\n%q", got, want)
	}

	if got := newMemoryReader(nil).recentEpisodes(context.Background(), "H-01", 3); got != "" {
		t.Errorf("nil store = %q, want empty", got)
	}
}

// TestMemoryReader_PeopleNilStore verifies the people read degrades to the KB
// roster + default affinity when there is no store (in-memory mode) — the
// roster must not disappear from the prompt.
func TestMemoryReader_PeopleNilStore(t *testing.T) {
	r := newMemoryReader(nil)
	got := r.people(context.Background(), "H-01", relRosterKB(), nil)
	if got == "" {
		t.Fatal("nil store should still render the KB roster with default affinity")
	}
}

// TestMemoryReader_ShareableCommitments pins the 【可分享的消息】 read: only
// pending invitation commitments surface (broadcast events for word-of-mouth),
// not appointment/todo; nil store → empty.
func TestMemoryReader_ShareableCommitments(t *testing.T) {
	r := newMemoryReader(&memoryFakeStore{commitments: []storage.Commitment{
		{Kind: "invitation", Content: "老王的生日会", DueStart: 300000, DueEnd: 305000},
		{Kind: "appointment", Content: "和老王下棋", DueStart: 100000, DueEnd: 102000},
	}})
	got := r.shareableCommitments(context.Background(), "H-01", 100)
	if !strings.Contains(got, "老王的生日会") {
		t.Errorf("should include invitation, got %q", got)
	}
	if strings.Contains(got, "和老王下棋") {
		t.Errorf("should exclude appointment, got %q", got)
	}

	if got := newMemoryReader(nil).shareableCommitments(context.Background(), "H-01", 100); got != "" {
		t.Errorf("nil store = %q, want empty", got)
	}
}
