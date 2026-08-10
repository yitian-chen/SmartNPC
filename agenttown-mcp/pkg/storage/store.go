// Package storage defines the persistence layer for agent state.
//
// The Store interface is intentionally narrow: Stage 3 persists only the
// four schedule-state fields (dailyPlan / currentDay / currentPlanIndex /
// currentSlot). Stage 4 (memory) and Stage 5 (relationships) will extend
// the interface with their own CRUD methods, backed by tables already
// created in migrations/0001_init.sql.
//
// Two implementations:
//   - NoopStore: in-memory mode (empty DSN), all writes are no-op and
//     Load returns ErrNotFound. This is the default so tests and quick
//     smoke runs need no MySQL.
//   - MySQLStore: real persistence (pkg/storage/mysql.go), created when
//     --mysql-dsn is non-empty.
package storage

import (
	"context"
	"errors"
)

// ErrNotFound is returned by LoadScheduleState when no row exists for the
// given agent. Callers treat this as "cold start, use defaults".
var ErrNotFound = errors.New("schedule state not found")

// ScheduleState is the persisted slice of AgentState: the four fields
// marked persistent in pkg/agentstate. Saved as a single row per agent.
type ScheduleState struct {
	DailyPlan        string
	CurrentDay       int
	CurrentPlanIndex int
	CurrentSlot      string
}

// Store is the persistence interface consumed by pkg/agentstate.
// Methods must be safe for concurrent use by multiple goroutines
// (MySQLStore uses database/sql's connection pool, which is concurrency-safe).
type Store interface {
	// LoadScheduleState reads the persisted schedule state for an agent.
	// Returns ErrNotFound when the agent has no row (first run).
	LoadScheduleState(ctx context.Context, agentID string) (ScheduleState, error)

	// SaveScheduleState upserts the schedule state for an agent. Called
	// write-through from AgentState setters (no batching).
	SaveScheduleState(ctx context.Context, agentID string, s ScheduleState) error

	// Close releases the underlying resources (DB connection pool).
	// It must be idempotent and safe to call on a NoopStore.
	Close() error
}

// NoopStore is the default Store when --mysql-dsn is empty. All writes are
// no-op; Load returns ErrNotFound so the agent behaves as a cold start
// (generates a fresh daily plan, currentDay=-1). This preserves the
// pre-Stage-3 in-memory behavior exactly.
type NoopStore struct{}

// LoadScheduleState always returns ErrNotFound.
func (NoopStore) LoadScheduleState(context.Context, string) (ScheduleState, error) {
	return ScheduleState{}, ErrNotFound
}

// SaveScheduleState is a no-op.
func (NoopStore) SaveScheduleState(context.Context, string, ScheduleState) error {
	return nil
}

// Close is a no-op.
func (NoopStore) Close() error { return nil }
