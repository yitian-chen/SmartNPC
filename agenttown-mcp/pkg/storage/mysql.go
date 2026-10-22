package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// SaveEpisode inserts one episodic_memories row. participants is marshaled
// to JSON (empty slice → "[]", never NULL).
func (s *MySQLStore) SaveEpisode(ctx context.Context, ep Episode) error {
	if ep.Participants == nil {
		ep.Participants = []string{}
	}
	participants, _ := json.Marshal(ep.Participants)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO episodic_memories
		   (id, owner_id, game_time, game_day, location, category, summary,
		    participants_json, importance, emotion, source, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ep.ID, ep.OwnerID, ep.GameTime, ep.GameDay, ep.Location, ep.Category,
		ep.Summary, string(participants), ep.Importance, ep.Emotion, ep.Source, ep.Status,
	)
	if err != nil {
		return fmt.Errorf("save episode %s for %s: %w", ep.ID, ep.OwnerID, err)
	}
	return nil
}

// LoadEpisodes returns episodes matching the query, ordered by game_time DESC.
// Participant filtering is NOT done in SQL (JSON matching is awkward in MySQL);
// callers filter in Go. Keywords use LIKE '%kw%' (no FTS — MySQL has no FTS5
// trigram; data volume is small enough for a scan).
func (s *MySQLStore) LoadEpisodes(ctx context.Context, ownerID string, q EpisodeQuery) ([]Episode, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, owner_id, game_time, game_day, location, category, summary,
	                participants_json, importance, emotion, source, status
	                FROM episodic_memories WHERE owner_id = ?`)
	args := []any{ownerID}
	if q.Category != "" {
		sb.WriteString(" AND category = ?")
		args = append(args, q.Category)
	}
	if q.GameTimeMin > 0 {
		sb.WriteString(" AND game_time >= ?")
		args = append(args, q.GameTimeMin)
	}
	if q.GameTimeMax > 0 {
		sb.WriteString(" AND game_time <= ?")
		args = append(args, q.GameTimeMax)
	}
	for _, kw := range q.Keywords {
		sb.WriteString(" AND summary LIKE ?")
		args = append(args, "%"+kw+"%")
	}
	sb.WriteString(" ORDER BY game_time DESC")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("load episodes for %s: %w", ownerID, err)
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var ep Episode
		var participants []byte
		if err := rows.Scan(&ep.ID, &ep.OwnerID, &ep.GameTime, &ep.GameDay, &ep.Location,
			&ep.Category, &ep.Summary, &participants, &ep.Importance, &ep.Emotion,
			&ep.Source, &ep.Status); err != nil {
			return nil, fmt.Errorf("scan episode for %s: %w", ownerID, err)
		}
		_ = json.Unmarshal(participants, &ep.Participants)
		out = append(out, ep)
	}
	return out, rows.Err()
}

// LoadLatestDailySummary returns the most recent daily_summary episode for
// ownerID. Returns ErrNotFound when none exists.
func (s *MySQLStore) LoadLatestDailySummary(ctx context.Context, ownerID string) (Episode, error) {
	var ep Episode
	var participants []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT id, owner_id, game_time, game_day, location, category, summary,
		        participants_json, importance, emotion, source, status
		 FROM episodic_memories WHERE owner_id = ? AND category = 'daily_summary'
		 ORDER BY game_time DESC LIMIT 1`, ownerID,
	).Scan(&ep.ID, &ep.OwnerID, &ep.GameTime, &ep.GameDay, &ep.Location, &ep.Category,
		&ep.Summary, &participants, &ep.Importance, &ep.Emotion, &ep.Source, &ep.Status)
	if err == sql.ErrNoRows {
		return Episode{}, ErrNotFound
	}
	if err != nil {
		return Episode{}, fmt.Errorf("load latest daily summary for %s: %w", ownerID, err)
	}
	_ = json.Unmarshal(participants, &ep.Participants)
	return ep, nil
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

// SaveCommitment inserts one episodic_commitments row.
func (s *MySQLStore) SaveCommitment(ctx context.Context, cm Commitment) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO episodic_commitments
		   (id, owner_id, origin_ep_id, kind, counterpart_id, counterpart_name,
		    content, due_start, due_end, due_location, status, created_time, updated_time)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cm.ID, cm.OwnerID, cm.OriginEpID, cm.Kind, cm.CounterpartID, cm.CounterpartName,
		cm.Content, cm.DueStart, cm.DueEnd, cm.DueLocation, cm.Status, cm.CreatedTime, cm.UpdatedTime,
	)
	if err != nil {
		return fmt.Errorf("save commitment %s for %s: %w", cm.ID, cm.OwnerID, err)
	}
	return nil
}

// UpdateCommitment updates a commitment's status and (optionally) its due
// window. Empty status = leave unchanged; dueStart/dueEnd < 0 = leave
// unchanged (sentinel). updated_time is set to the current wall clock (a
// bookkeeping stamp — semantic timestamps are the caller's game seconds).
func (s *MySQLStore) UpdateCommitment(ctx context.Context, id string, status string, dueStart, dueEnd int64) error {
	var sets []string
	var args []any
	if status != "" {
		sets = append(sets, "status = ?")
		args = append(args, status)
	}
	if dueStart >= 0 {
		sets = append(sets, "due_start = ?")
		args = append(args, dueStart)
	}
	if dueEnd >= 0 {
		sets = append(sets, "due_end = ?")
		args = append(args, dueEnd)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_time = ?")
	args = append(args, time.Now().Unix())
	args = append(args, id)
	_, err := s.db.ExecContext(ctx,
		`UPDATE episodic_commitments SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("update commitment %s: %w", id, err)
	}
	return nil
}

// LoadCommitments returns commitments matching the query, ordered by due_start ASC.
func (s *MySQLStore) LoadCommitments(ctx context.Context, ownerID string, q CommitmentQuery) ([]Commitment, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, owner_id, origin_ep_id, kind, counterpart_id, counterpart_name,
	                content, due_start, due_end, due_location, status, created_time, updated_time
	                FROM episodic_commitments WHERE owner_id = ?`)
	args := []any{ownerID}
	if q.Status != "" {
		sb.WriteString(" AND status = ?")
		args = append(args, q.Status)
	}
	if q.CounterpartID != "" {
		sb.WriteString(" AND counterpart_id = ?")
		args = append(args, q.CounterpartID)
	}
	if q.DueStartMax > 0 {
		sb.WriteString(" AND due_start <= ?")
		args = append(args, q.DueStartMax)
	}
	if q.DueEndMin > 0 {
		sb.WriteString(" AND due_end >= ?")
		args = append(args, q.DueEndMin)
	}
	sb.WriteString(" ORDER BY due_start ASC")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("load commitments for %s: %w", ownerID, err)
	}
	defer rows.Close()
	var out []Commitment
	for rows.Next() {
		var cm Commitment
		if err := rows.Scan(&cm.ID, &cm.OwnerID, &cm.OriginEpID, &cm.Kind, &cm.CounterpartID,
			&cm.CounterpartName, &cm.Content, &cm.DueStart, &cm.DueEnd, &cm.DueLocation,
			&cm.Status, &cm.CreatedTime, &cm.UpdatedTime); err != nil {
			return nil, fmt.Errorf("scan commitment for %s: %w", ownerID, err)
		}
		out = append(out, cm)
	}
	return out, rows.Err()
}

// UpsertPerson upserts one semantic_people row. affinity is incremented by the
// delta (clamped [0,100]); target_name/impression/last_interact_time are
// overwritten by the supplied values. New row affinity = 50 + delta (50 neutral).
func (s *MySQLStore) UpsertPerson(ctx context.Context, p Person, affinityDelta int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO semantic_people
		   (owner_id, target_id, target_name, affinity, impression, last_interact_time)
		 VALUES (?, ?, ?, GREATEST(0, LEAST(100, 50 + ?)), ?, ?)
		 ON DUPLICATE KEY UPDATE
		   affinity = GREATEST(0, LEAST(100, affinity + ?)),
		   target_name = VALUES(target_name),
		   impression = VALUES(impression),
		   last_interact_time = VALUES(last_interact_time)`,
		p.OwnerID, p.TargetID, p.TargetName, affinityDelta, p.Impression, p.LastInteractTime,
		affinityDelta,
	)
	if err != nil {
		return fmt.Errorf("upsert person %s→%s: %w", p.OwnerID, p.TargetID, err)
	}
	return nil
}

// SeedPerson inserts a person row only if none exists (INSERT IGNORE) with an
// explicit affinity. Used at cold start to import KB seed values without
// overwriting affinity accumulated in a previous run.
func (s *MySQLStore) SeedPerson(ctx context.Context, p Person) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT IGNORE INTO semantic_people
		   (owner_id, target_id, target_name, affinity, impression, last_interact_time)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.OwnerID, p.TargetID, p.TargetName, p.Affinity, p.Impression, p.LastInteractTime,
	)
	if err != nil {
		return fmt.Errorf("seed person %s→%s: %w", p.OwnerID, p.TargetID, err)
	}
	return nil
}

// LoadPeople returns all person rows for ownerID, ordered by affinity DESC.
func (s *MySQLStore) LoadPeople(ctx context.Context, ownerID string) ([]Person, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner_id, target_id, target_name, affinity, impression, last_interact_time
		 FROM semantic_people WHERE owner_id = ? ORDER BY affinity DESC`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("load people for %s: %w", ownerID, err)
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.OwnerID, &p.TargetID, &p.TargetName, &p.Affinity,
			&p.Impression, &p.LastInteractTime); err != nil {
			return nil, fmt.Errorf("scan person for %s: %w", ownerID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
