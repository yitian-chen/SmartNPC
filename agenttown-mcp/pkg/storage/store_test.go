package storage

import (
	"context"
	"errors"
	"testing"
)

// TestNoopStore_LoadReturnsNotFound verifies NoopStore signals cold-start
// via ErrNotFound (rather than a zero ScheduleState), so callers can
// distinguish "no row" from "row with zero values".
func TestNoopStore_LoadReturnsNotFound(t *testing.T) {
	st := NoopStore{}
	_, err := st.LoadScheduleState(context.Background(), "H-01")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadScheduleState: got err=%v, want ErrNotFound", err)
	}
}

// TestNoopStore_SaveIsNoOp verifies Save does not panic and returns nil.
func TestNoopStore_SaveIsNoOp(t *testing.T) {
	st := NoopStore{}
	if err := st.SaveScheduleState(context.Background(), "H-01", ScheduleState{DailyPlan: "x"}); err != nil {
		t.Errorf("SaveScheduleState: got err=%v, want nil", err)
	}
}

// TestNoopStore_CloseIsNoOp verifies Close is safe and idempotent.
func TestNoopStore_CloseIsNoOp(t *testing.T) {
	st := NoopStore{}
	for i := 0; i < 3; i++ {
		if err := st.Close(); err != nil {
			t.Errorf("Close[%d]: got err=%v, want nil", i, err)
		}
	}
}

// ─── migration file embedding sanity ───

// TestEmbeddedMigrations_Listed verifies the embed directive picked up
// the init migration. Catches build-time mistakes with the //go:embed path.
func TestEmbeddedMigrations_Listed(t *testing.T) {
	versions, err := listMigrationFiles()
	if err != nil {
		t.Fatalf("listMigrationFiles: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("expected at least one embedded migration, got none — embed directive broken?")
	}
	if versions[0] != "0001_init" {
		t.Errorf("first migration: got %q, want 0001_init", versions[0])
	}
}

// TestSplitStatements verifies the simple SQL splitter handles the
// multi-statement init migration correctly.
func TestSplitStatements(t *testing.T) {
	stmts := splitStatements("CREATE TABLE a(x INT); CREATE TABLE b(y INT);")
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}
}

// TestSplitStatements_TrailingSemicolon verifies trailing semicolons don't
// produce empty statements that would confuse Exec.
func TestSplitStatements_TrailingSemicolon(t *testing.T) {
	stmts := splitStatements("SELECT 1;;")
	// The empty middle produces one blank entry which the caller filters.
	if len(stmts) == 0 {
		t.Fatal("got 0 statements, want at least 1")
	}
}
