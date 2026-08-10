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
