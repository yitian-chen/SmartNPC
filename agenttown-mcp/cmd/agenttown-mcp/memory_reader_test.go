package main

import (
	"context"
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
		{Summary: "昨天熬夜上网", Category: "action_failed"},
	}})
	got := r.recentEpisodes(context.Background(), "H-01", 3)
	want := "- 今天和老王下了盘棋（conversation）\n- 昨天熬夜上网（action_failed）"
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
