package main

import (
	"context"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

// TestMemoryReader_RecentMemories pins the read-memory internal tool's
// 【过往经验】 rendering: bullet list with memory-type annotation; nil store
// (in-memory mode) → empty. The reader is program-only — the LLM never calls
// it (it is not in the function-calling tools list).
func TestMemoryReader_RecentMemories(t *testing.T) {
	r := newMemoryReader(&memoryFakeStore{savedMemories: []storage.Memory{
		{Content: "今天和老王下了盘棋", MemoryType: "event"},
		{Content: "昨天熬夜上网", MemoryType: "daily_summary"},
	}})
	got := r.recentMemories(context.Background(), "H-01")
	want := "- 今天和老王下了盘棋（event）\n- 昨天熬夜上网（daily_summary）"
	if got != want {
		t.Errorf("recentMemories =\n%q\nwant\n%q", got, want)
	}

	if got := newMemoryReader(nil).recentMemories(context.Background(), "H-01"); got != "" {
		t.Errorf("nil store = %q, want empty", got)
	}
}

// TestMemoryReader_RelationshipsNilStore verifies the relationships read
// degrades to the KB roster + default affection when there is no store
// (in-memory mode) — the roster must not disappear from the prompt.
func TestMemoryReader_RelationshipsNilStore(t *testing.T) {
	r := newMemoryReader(nil)
	got := r.relationships(context.Background(), "H-01", relRosterKB(), nil)
	if got == "" {
		t.Fatal("nil store should still render the KB roster with default affection")
	}
}
