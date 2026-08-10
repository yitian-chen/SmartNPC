package storage

import (
	"context"
	"database/sql"
	"encoding/json"
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

// nullableString returns nil for empty strings so the column receives NULL,
// otherwise the string value. Used for nullable VARCHAR columns
// (related_*_id, action_id, result).
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SaveMemory inserts a memory row. importance defaults to 50 in schema;
// related_* fields are NULL when empty string. Returns the auto-increment ID.
func (s *MySQLStore) SaveMemory(ctx context.Context, agentID string, m Memory) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_memories
		   (agent_id, memory_type, content, importance,
		    related_agent_id, related_object_id, related_zone_id,
		    created_at, last_accessed_at, decay_score)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, m.MemoryType, m.Content, m.Importance,
		nullableString(m.RelatedAgentID), nullableString(m.RelatedObjectID), nullableString(m.RelatedZoneID),
		m.CreatedAt, m.LastAccessedAt, m.DecayScore,
	)
	if err != nil {
		return 0, fmt.Errorf("save memory for %s: %w", agentID, err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// LoadRecentMemories returns the top-N memories for agentID by created_at DESC.
// Empty related_* columns (NULL) are coerced to "" via COALESCE.
func (s *MySQLStore) LoadRecentMemories(ctx context.Context, agentID string, limit int) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_id, memory_type, content, importance,
		        COALESCE(related_agent_id,''), COALESCE(related_object_id,''), COALESCE(related_zone_id,''),
		        created_at, last_accessed_at, decay_score
		 FROM agent_memories WHERE agent_id = ?
		 ORDER BY created_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("load recent memories for %s: %w", agentID, err)
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := rows.Scan(&m.ID, &m.AgentID, &m.MemoryType, &m.Content, &m.Importance,
			&m.RelatedAgentID, &m.RelatedObjectID, &m.RelatedZoneID,
			&m.CreatedAt, &m.LastAccessedAt, &m.DecayScore); err != nil {
			return nil, fmt.Errorf("scan memory for %s: %w", agentID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveActionRecord inserts one action_history row at action completion.
// params is marshaled to JSON; empty action_id/result become NULL.
func (s *MySQLStore) SaveActionRecord(ctx context.Context, agentID string, r ActionRecord) error {
	paramsJSON, _ := json.Marshal(r.Params)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO action_history
		   (agent_id, action_id, cmd, params, source, started_at, completed_at, result, duration_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		agentID, nullableString(r.ActionID), r.Cmd, string(paramsJSON),
		r.Source, r.StartedAt, r.CompletedAt, nullableString(r.Result), r.DurationMs,
	)
	if err != nil {
		return fmt.Errorf("save action record for %s: %w", agentID, err)
	}
	return nil
}

// LoadActionHistory returns the top-N action records for agentID by started_at DESC.
// Caller reverses the slice for chronological order before feeding to the LLM.
func (s *MySQLStore) LoadActionHistory(ctx context.Context, agentID string, limit int) ([]ActionRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, agent_id, action_id, cmd, params, source, started_at, completed_at, result, duration_ms
		 FROM action_history WHERE agent_id = ?
		 ORDER BY started_at DESC LIMIT ?`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("load action history for %s: %w", agentID, err)
	}
	defer rows.Close()
	var out []ActionRecord
	for rows.Next() {
		var r ActionRecord
		var actionID, result sql.NullString
		var paramsJSON []byte
		if err := rows.Scan(&r.ID, &r.AgentID, &actionID, &r.Cmd, &paramsJSON, &r.Source,
			&r.StartedAt, &r.CompletedAt, &result, &r.DurationMs); err != nil {
			return nil, fmt.Errorf("scan action record for %s: %w", agentID, err)
		}
		r.ActionID = actionID.String
		r.Result = result.String
		if len(paramsJSON) > 0 {
			_ = json.Unmarshal(paramsJSON, &r.Params)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRelationship upserts one directional row (agentA→agentB). On duplicate
// key (the row already exists from a prior interaction), familiarity and
// affection are incremented by the supplied deltas, interaction_count is
// bumped by 1, and last_interaction_at is refreshed. Callers update both
// directions by invoking this twice with swapped arguments.
func (s *MySQLStore) SaveRelationship(ctx context.Context, agentA, agentB string, familiarityDelta, affectionDelta int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_relationships
		   (agent_a, agent_b, familiarity, affection, interaction_count, last_interaction_at)
		 VALUES (?, ?, ?, ?, 1, NOW())
		 ON DUPLICATE KEY UPDATE
		   familiarity = familiarity + VALUES(familiarity),
		   affection   = affection   + VALUES(affection),
		   interaction_count = interaction_count + 1,
		   last_interaction_at = NOW()`,
		agentA, agentB, familiarityDelta, affectionDelta,
	)
	if err != nil {
		return fmt.Errorf("save relationship %s→%s: %w", agentA, agentB, err)
	}
	return nil
}

// LoadRelationships returns rows where agentID is either side (agent_a OR
// agent_b), ordered by most recently updated first. Used to inject the
// 【人际关系】段 in the tactical prompt. last_interaction_at may be NULL
// (seed rows that have never been interacted through SaveRelationship);
// COALESCE coerces to the zero time.
func (s *MySQLStore) LoadRelationships(ctx context.Context, agentID string, limit int) ([]Relationship, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_a, agent_b, familiarity, affection, interaction_count,
		        COALESCE(last_interaction_at, '0000-00-00 00:00:00')
		 FROM agent_relationships
		 WHERE agent_a = ? OR agent_b = ?
		 ORDER BY updated_at DESC LIMIT ?`, agentID, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("load relationships for %s: %w", agentID, err)
	}
	defer rows.Close()
	var out []Relationship
	for rows.Next() {
		var r Relationship
		if err := rows.Scan(&r.AgentA, &r.AgentB, &r.Familiarity, &r.Affection,
			&r.InteractionCount, &r.LastInteractionAt); err != nil {
			return nil, fmt.Errorf("scan relationship for %s: %w", agentID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedRelationship inserts a row only if no row yet exists for the pair
// (INSERT IGNORE). Used at cold start to import KB seed values without
// overwriting interaction counts accumulated in a previous run. Has no
// last_interaction_at (stays NULL) — the first live interaction fills it in.
func (s *MySQLStore) SeedRelationship(ctx context.Context, agentA, agentB string, familiarity, affection int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO agent_relationships
		   (agent_a, agent_b, familiarity, affection, interaction_count)
		 VALUES (?, ?, ?, ?, 0)`,
		agentA, agentB, familiarity, affection,
	)
	if err != nil {
		return fmt.Errorf("seed relationship %s→%s: %w", agentA, agentB, err)
	}
	return nil
}
