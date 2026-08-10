package storage

import (
	"context"
	"os"
	"testing"
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
