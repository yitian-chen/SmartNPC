package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// mysqlTestDSN is the env var read to obtain a MySQL connection for tests.
// When unset, all MySQLStore tests skip — so CI without MySQL passes.
const mysqlTestDSN = "MYSQL_TEST_DSN"

// skipIfNoMySQL skips the test when MYSQL_TEST_DSN is not set. Returns the
// DSN and a *MySQLStore opened against it (with migrations applied).
func skipIfNoMySQL(t *testing.T) (*MySQLStore, string) {
	t.Helper()
	dsn := os.Getenv(mysqlTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run MySQLStore tests", mysqlTestDSN)
	}
	store, err := NewMySQLStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewMySQLStore: %v", err)
	}
	return store, dsn
}

// TestMySQLStore_SaveLoadRoundTrip verifies a Save followed by a Load
// returns the same state.
func TestMySQLStore_SaveLoadRoundTrip(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()

	want := ScheduleState{
		DailyPlan:        "06:00-22:00 测试计划",
		CurrentDay:       3,
		CurrentPlanIndex: 2,
		CurrentSlot:      "14:00-18:00",
	}
	if err := store.SaveScheduleState(ctx, "test-roundtrip", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.LoadScheduleState(ctx, "test-roundtrip")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("roundtrip: got %+v, want %+v", got, want)
	}
}

// TestMySQLStore_LoadNotFound verifies a missing agent returns ErrNotFound.
func TestMySQLStore_LoadNotFound(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	_, err := store.LoadScheduleState(context.Background(), "nonexistent-agent-xyz")
	if err != ErrNotFound {
		t.Errorf("Load nonexistent: got err=%v, want ErrNotFound", err)
	}
}

// TestMySQLStore_UpsertIdempotent verifies Save is an upsert — saving twice
// with different values updates the row rather than failing.
func TestMySQLStore_UpsertIdempotent(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentID := "test-upsert"

	first := ScheduleState{DailyPlan: "v1", CurrentDay: 1, CurrentPlanIndex: 0, CurrentSlot: "06:00-10:00"}
	if err := store.SaveScheduleState(ctx, agentID, first); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	second := ScheduleState{DailyPlan: "v2-overwritten", CurrentDay: 2, CurrentPlanIndex: 1, CurrentSlot: "10:00-14:00"}
	if err := store.SaveScheduleState(ctx, agentID, second); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	got, err := store.LoadScheduleState(ctx, agentID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != second {
		t.Errorf("after upsert: got %+v, want %+v", got, second)
	}
}

// TestMySQLStore_EmptyStringFields verifies empty-string fields round-trip
// (DEFAULT '' in schema should not surprise Scan).
func TestMySQLStore_EmptyStringFields(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()

	want := ScheduleState{DailyPlan: "", CurrentDay: -1, CurrentPlanIndex: 0, CurrentSlot: ""}
	if err := store.SaveScheduleState(ctx, "test-empty", want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.LoadScheduleState(ctx, "test-empty")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("empty fields: got %+v, want %+v", got, want)
	}
}

// TestMySQLStore_MigrationsIdempotent verifies re-running NewMySQLStore
// (which runs migrations) doesn't fail on an existing schema.
func TestMySQLStore_MigrationsIdempotent(t *testing.T) {
	dsn := os.Getenv(mysqlTestDSN)
	if dsn == "" {
		t.Skipf("set %s to run MySQLStore tests", mysqlTestDSN)
	}
	ctx := context.Background()
	store1, err := NewMySQLStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewMySQLStore #1: %v", err)
	}
	_ = store1.Close()
	store2, err := NewMySQLStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewMySQLStore #2 (migrations should be idempotent): %v", err)
	}
	_ = store2.Close()
}

// ─── Stage 4: memory + action_history round-trip tests ───

// TestMySQLStore_SaveLoadMemoryRoundTrip verifies a SaveMemory followed by
// LoadRecentMemories returns the same memory with fields intact.
func TestMySQLStore_SaveLoadMemoryRoundTrip(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentID := "test-mem-roundtrip"
	now := time.Now().Truncate(time.Second)

	want := Memory{
		AgentID:         agentID,
		MemoryType:      "event",
		Content:         "完成车间装配任务",
		Importance:      60,
		RelatedObjectID: "workbench_01",
		RelatedZoneID:   "main_workshop",
		CreatedAt:       now,
		LastAccessedAt:  now,
		DecayScore:      1.0,
	}
	if _, err := store.SaveMemory(ctx, agentID, want); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	got, err := store.LoadRecentMemories(ctx, agentID, 10)
	if err != nil {
		t.Fatalf("LoadRecentMemories: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("LoadRecentMemories: got 0 rows, want at least 1")
	}
	// DESC order — most recent first. Our just-inserted row should be first.
	m := got[0]
	if m.Content != want.Content || m.MemoryType != want.MemoryType ||
		m.Importance != want.Importance || m.RelatedObjectID != want.RelatedObjectID ||
		m.RelatedZoneID != want.RelatedZoneID || m.RelatedAgentID != "" ||
		m.DecayScore != want.DecayScore {
		t.Errorf("roundtrip: got %+v, want %+v", m, want)
	}
	if m.ID == 0 {
		t.Error("roundtrip: ID should be non-zero after insert")
	}
}

// TestMySQLStore_LoadRecentMemoriesDESC verifies retrieval is created_at DESC.
func TestMySQLStore_LoadRecentMemoriesDESC(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentID := "test-mem-desc"
	base := time.Now().Truncate(time.Second)

	// Insert 3 memories with increasing created_at.
	for i := 0; i < 3; i++ {
		m := Memory{
			AgentID:    agentID,
			MemoryType: "event",
			Content:    fmt.Sprintf("event-%d", i),
			Importance: 50,
			CreatedAt:  base.Add(time.Duration(i) * time.Minute),
			LastAccessedAt: base,
			DecayScore: 1.0,
		}
		if _, err := store.SaveMemory(ctx, agentID, m); err != nil {
			t.Fatalf("SaveMemory[%d]: %v", i, err)
		}
	}
	got, err := store.LoadRecentMemories(ctx, agentID, 3)
	if err != nil {
		t.Fatalf("LoadRecentMemories: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// DESC — newest first.
	if got[0].Content != "event-2" || got[2].Content != "event-0" {
		t.Errorf("DESC order: got [%s, %s, %s], want [event-2, event-1, event-0]",
			got[0].Content, got[1].Content, got[2].Content)
	}
}

// TestMySQLStore_SaveLoadActionRecordRoundTrip verifies action_history round-trip
// including JSON params and nullable action_id/result.
func TestMySQLStore_SaveLoadActionRecordRoundTrip(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentID := "test-ah-roundtrip"
	start := time.Now().Truncate(time.Second)
	end := start.Add(5 * time.Second)

	want := ActionRecord{
		AgentID:     agentID,
		ActionID:    "act-123",
		Cmd:         "MoveTo",
		Params:      map[string]any{"target": "workbench_01"},
		Source:      "tactical",
		StartedAt:   start,
		CompletedAt: end,
		Result:      "success",
		DurationMs:  5000,
	}
	if err := store.SaveActionRecord(ctx, agentID, want); err != nil {
		t.Fatalf("SaveActionRecord: %v", err)
	}
	got, err := store.LoadActionHistory(ctx, agentID, 10)
	if err != nil {
		t.Fatalf("LoadActionHistory: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("LoadActionHistory: got 0 rows, want at least 1")
	}
	r := got[0] // DESC — most recent first
	if r.Cmd != want.Cmd || r.ActionID != want.ActionID || r.Source != want.Source ||
		r.Result != want.Result || r.DurationMs != want.DurationMs {
		t.Errorf("roundtrip: got %+v, want %+v", r, want)
	}
	if r.Params["target"] != "workbench_01" {
		t.Errorf("params: got %v, want target=workbench_01", r.Params)
	}
}

// TestMySQLStore_SaveActionRecordEmptyNullableFields verifies empty action_id
// and result are stored as NULL and read back as "".
func TestMySQLStore_SaveActionRecordEmptyNullableFields(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentID := "test-ah-empty"
	now := time.Now().Truncate(time.Second)

	want := ActionRecord{
		AgentID:     agentID,
		ActionID:    "", // NULL
		Cmd:         "Wait",
		Params:      nil,
		Source:      "",
		StartedAt:   now,
		CompletedAt: now,
		Result:      "", // NULL
		DurationMs:  0,
	}
	if err := store.SaveActionRecord(ctx, agentID, want); err != nil {
		t.Fatalf("SaveActionRecord: %v", err)
	}
	got, err := store.LoadActionHistory(ctx, agentID, 5)
	if err != nil {
		t.Fatalf("LoadActionHistory: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("LoadActionHistory: got 0 rows, want at least 1")
	}
	r := got[0]
	if r.ActionID != "" || r.Result != "" {
		t.Errorf("empty nullable fields: got action_id=%q result=%q, want both empty",
			r.ActionID, r.Result)
	}
	if r.Cmd != "Wait" {
		t.Errorf("cmd: got %q, want Wait", r.Cmd)
	}
}

// ─── Stage 5: agent_relationships ───

// TestMySQLStore_SaveRelationshipUpsert verifies SaveRelationship inserts on
// first call and increments on subsequent calls (ON DUPLICATE KEY UPDATE).
func TestMySQLStore_SaveRelationshipUpsert(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentA, agentB := "test-rel-a", "test-rel-b"

	// First save → INSERT (familiarity=1, interaction_count=1).
	if err := store.SaveRelationship(ctx, agentA, agentB, 1, 0); err != nil {
		t.Fatalf("SaveRelationship #1: %v", err)
	}
	rels, err := store.LoadRelationships(ctx, agentA, 10)
	if err != nil {
		t.Fatalf("LoadRelationships #1: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d rows, want 1", len(rels))
	}
	if rels[0].Familiarity != 1 || rels[0].InteractionCount != 1 {
		t.Errorf("after first save: got fam=%d count=%d, want fam=1 count=1",
			rels[0].Familiarity, rels[0].InteractionCount)
	}

	// Second save → ON DUPLICATE KEY UPDATE (familiarity=2, interaction_count=2).
	if err := store.SaveRelationship(ctx, agentA, agentB, 1, 0); err != nil {
		t.Fatalf("SaveRelationship #2: %v", err)
	}
	rels, err = store.LoadRelationships(ctx, agentA, 10)
	if err != nil {
		t.Fatalf("LoadRelationships #2: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d rows, want 1 (upsert)", len(rels))
	}
	if rels[0].Familiarity != 2 || rels[0].InteractionCount != 2 {
		t.Errorf("after second save: got fam=%d count=%d, want fam=2 count=2",
			rels[0].Familiarity, rels[0].InteractionCount)
	}
}

// TestMySQLStore_LoadRelationshipsBothSides verifies LoadRelationships returns
// rows where agentID is either agent_a or agent_b.
func TestMySQLStore_LoadRelationshipsBothSides(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	// A→B and A→C; querying B should find the A→B row (B is agent_b side).
	if err := store.SaveRelationship(ctx, "test-side-a", "test-side-b", 1, 0); err != nil {
		t.Fatalf("SaveRelationship A→B: %v", err)
	}
	if err := store.SaveRelationship(ctx, "test-side-a", "test-side-c", 1, 0); err != nil {
		t.Fatalf("SaveRelationship A→C: %v", err)
	}
	rels, err := store.LoadRelationships(ctx, "test-side-b", 10)
	if err != nil {
		t.Fatalf("LoadRelationships B: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("querying B: got %d rows, want 1", len(rels))
	}
	if rels[0].AgentA != "test-side-a" || rels[0].AgentB != "test-side-b" {
		t.Errorf("got %s→%s, want test-side-a→test-side-b", rels[0].AgentA, rels[0].AgentB)
	}
}

// TestMySQLStore_SeedRelationshipInsertIgnore verifies SeedRelationship does
// not overwrite an existing row (INSERT IGNORE semantics).
func TestMySQLStore_SeedRelationshipInsertIgnore(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	agentA, agentB := "test-seed-a", "test-seed-b"

	// First seed → inserts row with familiarity=5.
	if err := store.SeedRelationship(ctx, agentA, agentB, 5, 0); err != nil {
		t.Fatalf("SeedRelationship #1: %v", err)
	}
	// Live interaction bumps familiarity to 6 via SaveRelationship.
	if err := store.SaveRelationship(ctx, agentA, agentB, 1, 0); err != nil {
		t.Fatalf("SaveRelationship: %v", err)
	}
	// Second seed → INSERT IGNORE should NOT reset familiarity back to 5.
	if err := store.SeedRelationship(ctx, agentA, agentB, 5, 0); err != nil {
		t.Fatalf("SeedRelationship #2: %v", err)
	}
	rels, err := store.LoadRelationships(ctx, agentA, 10)
	if err != nil {
		t.Fatalf("LoadRelationships: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("got %d rows, want 1", len(rels))
	}
	if rels[0].Familiarity != 6 {
		t.Errorf("after seed-ignore: got fam=%d, want 6 (seed should not overwrite)", rels[0].Familiarity)
	}
}
