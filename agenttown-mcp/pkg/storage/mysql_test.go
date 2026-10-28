package storage

import (
	"context"
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
// (DEFAULT ” in schema should not surprise Scan).
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

// ─── episodic_memories round-trip tests ───

// TestMySQLStore_SaveLoadEpisodeRoundTrip verifies a SaveEpisode followed by
// LoadEpisodes returns the same episode with fields intact (incl. participants).
func TestMySQLStore_SaveLoadEpisodeRoundTrip(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	ownerID := "test-ep-roundtrip"

	want := Episode{
		ID:           "ep_test_001",
		OwnerID:      ownerID,
		GameTime:     226800,
		GameDay:      3,
		Location:     "central_plaza",
		Category:     "conversation",
		Summary:      "在咖啡馆和老王聊下棋",
		Participants: []string{"npc_wang"},
		Importance:   7,
		Emotion:      0.6,
		Source:       "self",
		Status:       "active",
	}
	if err := store.SaveEpisode(ctx, want); err != nil {
		t.Fatalf("SaveEpisode: %v", err)
	}
	got, err := store.LoadEpisodes(ctx, ownerID, EpisodeQuery{Limit: 10})
	if err != nil {
		t.Fatalf("LoadEpisodes: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("LoadEpisodes: got 0 rows, want at least 1")
	}
	e := got[0]
	if e.Summary != want.Summary || e.Category != want.Category ||
		e.GameTime != want.GameTime || e.GameDay != want.GameDay ||
		len(e.Participants) != 1 || e.Participants[0] != "npc_wang" {
		t.Errorf("roundtrip: got %+v, want %+v", e, want)
	}
}

// TestMySQLStore_LoadLatestDailySummary verifies the daily_summary lookup
// returns ErrNotFound when absent and the latest row when present.
func TestMySQLStore_LoadLatestDailySummary(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	ownerID := "test-ep-daily"

	if _, err := store.LoadLatestDailySummary(ctx, ownerID); err != ErrNotFound {
		t.Fatalf("empty: got err=%v, want ErrNotFound", err)
	}
	if err := store.SaveEpisode(ctx, Episode{ID: "ep_d1", OwnerID: ownerID, GameTime: 100, GameDay: 1, Category: "daily_summary", Summary: "昨天总结"}); err != nil {
		t.Fatalf("SaveEpisode daily: %v", err)
	}
	got, err := store.LoadLatestDailySummary(ctx, ownerID)
	if err != nil {
		t.Fatalf("LoadLatestDailySummary: %v", err)
	}
	if got.Summary != "昨天总结" {
		t.Errorf("summary = %q, want 昨天总结", got.Summary)
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

// ─── semantic_people + episodic_commitments ───

// TestMySQLStore_UpsertPersonAffinityClamp verifies UpsertPerson inserts on
// first call (50 + delta) and clamps on subsequent calls.
func TestMySQLStore_UpsertPersonAffinityClamp(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	owner, target := "test-person-a", "test-person-b"

	// First upsert → new row affinity = 50 + 20 = 70.
	if err := store.UpsertPerson(ctx, Person{OwnerID: owner, TargetID: target, Impression: "初次"}, 20); err != nil {
		t.Fatalf("UpsertPerson #1: %v", err)
	}
	people, err := store.LoadPeople(ctx, owner)
	if err != nil {
		t.Fatalf("LoadPeople #1: %v", err)
	}
	if len(people) != 1 || people[0].Affinity != 70 {
		t.Fatalf("after first: got %+v, want affinity 70", people)
	}

	// Second upsert → affinity = 70 + 100 → clamp 100.
	if err := store.UpsertPerson(ctx, Person{OwnerID: owner, TargetID: target, Impression: "二次"}, 100); err != nil {
		t.Fatalf("UpsertPerson #2: %v", err)
	}
	people, err = store.LoadPeople(ctx, owner)
	if err != nil {
		t.Fatalf("LoadPeople #2: %v", err)
	}
	if people[0].Affinity != 100 {
		t.Errorf("after clamp-high: affinity=%d, want 100", people[0].Affinity)
	}
	if people[0].Impression != "二次" {
		t.Errorf("impression=%q, want 二次 (overwritten)", people[0].Impression)
	}
}

// TestMySQLStore_CommitmentRoundTrip verifies SaveCommitment / LoadCommitments
// round-trip + read-time expiry filtering + UpdateCommitment status change.
func TestMySQLStore_CommitmentRoundTrip(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	ownerID := "test-cm-roundtrip"

	cm := Commitment{
		ID: "cm_test_001", OwnerID: ownerID, Kind: "appointment",
		CounterpartID: "npc_wang", CounterpartName: "老王",
		Content: "在公园棋桌和老王下棋", DueStart: 309600, DueEnd: 313200,
		DueLocation: "zone_park", Status: "pending", CreatedTime: 226800,
	}
	if err := store.SaveCommitment(ctx, cm); err != nil {
		t.Fatalf("SaveCommitment: %v", err)
	}
	got, err := store.LoadCommitments(ctx, ownerID, CommitmentQuery{Status: "pending", DueEndMin: 300000})
	if err != nil {
		t.Fatalf("LoadCommitments: %v", err)
	}
	if len(got) != 1 || got[0].Content != cm.Content || got[0].DueLocation != "zone_park" {
		t.Errorf("roundtrip: got %+v, want %+v", got, cm)
	}

	// 读时过期过滤：DueEndMin 抬高后，due_end < min 的 pending 不返回。
	got, err = store.LoadCommitments(ctx, ownerID, CommitmentQuery{Status: "pending", DueEndMin: 320000})
	if err != nil {
		t.Fatalf("LoadCommitments expired: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expired filter: got %d rows, want 0", len(got))
	}

	// Update → done。
	if err := store.UpdateCommitment(ctx, cm.ID, "done", -1, -1); err != nil {
		t.Fatalf("UpdateCommitment: %v", err)
	}
	got, err = store.LoadCommitments(ctx, ownerID, CommitmentQuery{Status: "done"})
	if err != nil {
		t.Fatalf("LoadCommitments done: %v", err)
	}
	if len(got) != 1 || got[0].Status != "done" {
		t.Errorf("after update: got %+v, want status done", got)
	}
}

// TestMySQLStore_SeedPersonInsertIgnore verifies SeedPerson does not overwrite
// an existing row (INSERT IGNORE semantics).
func TestMySQLStore_SeedPersonInsertIgnore(t *testing.T) {
	store, _ := skipIfNoMySQL(t)
	defer store.Close()
	ctx := context.Background()
	owner, target := "test-seed-p-a", "test-seed-p-b"

	// First seed → affinity 80.
	if err := store.SeedPerson(ctx, Person{OwnerID: owner, TargetID: target, Affinity: 80}); err != nil {
		t.Fatalf("SeedPerson #1: %v", err)
	}
	// Live upsert bumps to 80+10=90.
	if err := store.UpsertPerson(ctx, Person{OwnerID: owner, TargetID: target}, 10); err != nil {
		t.Fatalf("UpsertPerson: %v", err)
	}
	// Second seed → INSERT IGNORE should NOT reset affinity back to 80.
	if err := store.SeedPerson(ctx, Person{OwnerID: owner, TargetID: target, Affinity: 80}); err != nil {
		t.Fatalf("SeedPerson #2: %v", err)
	}
	people, err := store.LoadPeople(ctx, owner)
	if err != nil {
		t.Fatalf("LoadPeople: %v", err)
	}
	if len(people) != 1 || people[0].Affinity != 90 {
		t.Errorf("after seed-ignore: affinity=%d, want 90", people[0].Affinity)
	}
}
