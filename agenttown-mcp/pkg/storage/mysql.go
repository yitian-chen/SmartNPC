package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // MySQL driver; registered via blank import.
)

// MySQLStore implements Store backed by MySQL. One row per agent in
// agent_schedule_state, upserted on every write-through call.
//
// The connection pool is sized for a single-process MCP serving a handful
// of agents; Stage 3's write frequency is low (a few slot switches per day).
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore opens a MySQL connection, runs migrations, and returns a
// ready Store. The DSN must include parseTime=true so DATETIME columns
// scan into time.Time (documented in .env.example).
//
// dsn example:
//
//	user:pass@tcp(127.0.0.1:3306)/agenttown?parseTime=true&charset=utf8mb4
func NewMySQLStore(ctx context.Context, dsn string) (*MySQLStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("mysql dsn is empty")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	// Pool sizing: modest — agent count is small, writes are infrequent.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := pingWithRetry(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

// LoadScheduleState reads the persisted row for agentID. Returns ErrNotFound
// when no row exists (first run after schema creation).
func (s *MySQLStore) LoadScheduleState(ctx context.Context, agentID string) (ScheduleState, error) {
	var st ScheduleState
	err := s.db.QueryRowContext(ctx,
		`SELECT daily_plan, current_day, current_plan_index, current_slot
		 FROM agent_schedule_state WHERE agent_id = ?`, agentID,
	).Scan(&st.DailyPlan, &st.CurrentDay, &st.CurrentPlanIndex, &st.CurrentSlot)
	if err == sql.ErrNoRows {
		return ScheduleState{}, ErrNotFound
	}
	if err != nil {
		return ScheduleState{}, fmt.Errorf("load schedule state for %s: %w", agentID, err)
	}
	return st, nil
}

// SaveScheduleState upserts the row for agentID. INSERT ... ON DUPLICATE KEY
// UPDATE avoids a read-then-write race when two goroutines save concurrently
// (each agent's writes are serialized by AgentState's mutex, but upsert is
// still the correct semantic).
func (s *MySQLStore) SaveScheduleState(ctx context.Context, agentID string, st ScheduleState) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_schedule_state
		   (agent_id, daily_plan, current_day, current_plan_index, current_slot)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   daily_plan = VALUES(daily_plan),
		   current_day = VALUES(current_day),
		   current_plan_index = VALUES(current_plan_index),
		   current_slot = VALUES(current_slot)`,
		agentID, st.DailyPlan, st.CurrentDay, st.CurrentPlanIndex, st.CurrentSlot,
	)
	if err != nil {
		return fmt.Errorf("save schedule state for %s: %w", agentID, err)
	}
	return nil
}

// Close releases the connection pool. Safe to call multiple times.
func (s *MySQLStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
