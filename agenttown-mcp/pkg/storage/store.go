// Package storage defines the persistence layer for agent state.
//
// The Store interface persists schedule state (Stage 3), action history, and
// the long-term-memory three-table model (Stage 4/5, 2026-10-22 rebuild per
// docs/AgentTown_LongTermMemory_SQLite_Design.md):
//
//	episodic_memories   — 过去经历（只追加，game_time 锚定）
//	episodic_commitments — 未来待办（pending → done/expired/cancelled）
//	semantic_people     — 对人认知（单一 affinity 0-100 + impression）
//
// Two implementations:
//   - NoopStore: in-memory mode (empty DSN), all writes are no-op and
//     Load returns ErrNotFound / empty. This is the default so tests and
//     quick smoke runs need no MySQL.
//   - MySQLStore: real persistence (pkg/storage/mysql.go), created when
//     --mysql-dsn is non-empty.
package storage

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by LoadScheduleState / LoadLatestDailySummary when
// no row exists for the given agent. Callers treat this as "cold start / no
// data yet".
var ErrNotFound = errors.New("not found")

// ScheduleState is the persisted slice of AgentState: the four fields
// marked persistent in pkg/agentstate. Saved as a single row per agent.
type ScheduleState struct {
	DailyPlan        string
	CurrentDay       int
	CurrentPlanIndex int
	CurrentSlot      string
}

// Episode represents one row in episodic_memories: a past experience.
// First-person one-line summary, appended only, anchored to game time.
type Episode struct {
	ID           string
	OwnerID      string
	GameTime     int64    // 游戏秒（UE GameTimeSec 截断）
	GameDay      int      // 第几个游戏日（1-indexed）
	Location     string   // zone_id / object_id
	Category     string   // conversation / combat / action_failed / daily_summary
	Summary      string   // 第一人称一句话摘要，直接进 prompt
	Participants []string // 涉及 agent_id，JSON 序列化
	Importance   int      // 1-10
	Emotion      float64  // -1.0 ~ +1.0
	Source       string   // self / heard / overheard
	Status       string   // active / archived
}

// EpisodeQuery filters LoadEpisodes. Zero values mean "no filter".
type EpisodeQuery struct {
	Category    string   // exact match; "" = all
	GameTimeMin int64    // game_time >= x; 0 = no lower bound
	GameTimeMax int64    // game_time <= x; 0 = no upper bound
	Keywords    []string // summary LIKE any keyword; empty = no filter
	Limit       int      // cap; 0 = no cap
}

// Commitment represents one row in episodic_commitments: a future to-do /
// appointment the NPC must remember. Status machine: pending → done /
// expired / cancelled.
type Commitment struct {
	ID              string
	OwnerID         string
	OriginEpID      string
	Kind            string // appointment / relay / invitation / todo
	CounterpartID   string // 关联对象（约老王填 npc_wang）
	CounterpartName string
	Content         string // 具体待办内容，直接进 prompt
	DueStart        int64  // 开始游戏秒
	DueEnd          int64  // 截止游戏秒
	DueLocation     string
	Status          string // pending / done / expired / cancelled
	CreatedTime     int64
	UpdatedTime     int64
}

// CommitmentQuery filters LoadCommitments. Zero values mean "no filter".
type CommitmentQuery struct {
	Status        string // exact match; "" = all
	CounterpartID string // exact match; "" = all
	DueStartMax   int64  // due_start <= x; 0 = no upper bound
	DueEndMin     int64  // due_end >= x; 0 = no lower bound
	Limit         int
}

// Person represents one row in semantic_people: cognition of another agent.
// Single numeric dimension affinity (0-100, 50 neutral) + a one-line
// impression (free text).
type Person struct {
	OwnerID          string
	TargetID         string
	TargetName       string
	Affinity         int    // 0-100，50 中性
	Impression       string // 一句话印象
	LastInteractTime int64  // 最近一次互动的游戏秒
}

// ActionRecord represents one row in action_history. Written as a single
// INSERT at action completion (not two-phase started/completed split).
type ActionRecord struct {
	ID          int64
	AgentID     string
	ActionID    string
	Cmd         string
	Params      map[string]any // marshaled to JSON column
	Source      string         // "mcp_tool" | "tactical" | ""
	StartedAt   time.Time
	CompletedAt time.Time
	Result      string // "success" | "failed" | "interrupted" | "error"
	DurationMs  int
}

// Store is the persistence interface consumed by pkg/agentstate and
// cmd/agenttown-mcp. Methods must be safe for concurrent use by multiple
// goroutines (MySQLStore uses database/sql's connection pool).
type Store interface {
	// LoadScheduleState reads the persisted schedule state for an agent.
	// Returns ErrNotFound when the agent has no row (first run).
	LoadScheduleState(ctx context.Context, agentID string) (ScheduleState, error)

	// SaveScheduleState upserts the schedule state for an agent. Called
	// write-through from AgentState setters (no batching).
	SaveScheduleState(ctx context.Context, agentID string, s ScheduleState) error

	// SaveEpisode inserts a new episode row.
	SaveEpisode(ctx context.Context, ep Episode) error

	// LoadEpisodes returns episodes matching the query, ordered by game_time DESC.
	// Participant filtering is NOT done here — callers filter in Go.
	LoadEpisodes(ctx context.Context, ownerID string, q EpisodeQuery) ([]Episode, error)

	// LoadLatestDailySummary returns the most recent daily_summary episode.
	// Returns ErrNotFound when none exists.
	LoadLatestDailySummary(ctx context.Context, ownerID string) (Episode, error)

	// SaveCommitment inserts a new commitment row.
	SaveCommitment(ctx context.Context, cm Commitment) error

	// UpdateCommitment updates a commitment's status and (optionally) its due
	// window. A non-empty status overwrites; dueStart/dueEnd < 0 leave the
	// existing value untouched (sentinel).
	UpdateCommitment(ctx context.Context, id string, status string, dueStart, dueEnd int64) error

	// LoadCommitments returns commitments matching the query, ordered by due_start ASC.
	LoadCommitments(ctx context.Context, ownerID string, q CommitmentQuery) ([]Commitment, error)

	// UpsertPerson upserts a person row: affinity is incremented by the delta
	// (clamped [0,100]) and target_name/impression/last_interact_time are
	// overwritten by the supplied values. New row affinity = 50 + delta.
	UpsertPerson(ctx context.Context, p Person, affinityDelta int) error

	// SeedPerson inserts a person row only if none exists (INSERT IGNORE) with
	// an explicit affinity. Used at cold start to import KB seed values without
	// overwriting affinity accumulated in a previous run.
	SeedPerson(ctx context.Context, p Person) error

	// LoadPeople returns all person rows for ownerID.
	LoadPeople(ctx context.Context, ownerID string) ([]Person, error)

	// SaveActionRecord inserts one action_history row (single INSERT at completion).
	SaveActionRecord(ctx context.Context, agentID string, r ActionRecord) error

	// LoadActionHistory returns the most recent N action records by started_at DESC.
	LoadActionHistory(ctx context.Context, agentID string, limit int) ([]ActionRecord, error)

	// Close releases the underlying resources (DB connection pool).
	// It must be idempotent and safe to call on a NoopStore.
	Close() error
}

// NoopStore is the default Store when --mysql-dsn is empty. All writes are
// no-op; Load returns ErrNotFound / empty so the agent behaves as a cold
// start (generates a fresh daily plan, currentDay=-1). This preserves the
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

// SaveEpisode is a no-op.
func (NoopStore) SaveEpisode(context.Context, Episode) error { return nil }

// LoadEpisodes returns nil.
func (NoopStore) LoadEpisodes(context.Context, string, EpisodeQuery) ([]Episode, error) {
	return nil, nil
}

// LoadLatestDailySummary returns ErrNotFound.
func (NoopStore) LoadLatestDailySummary(context.Context, string) (Episode, error) {
	return Episode{}, ErrNotFound
}

// SaveCommitment is a no-op.
func (NoopStore) SaveCommitment(context.Context, Commitment) error { return nil }

// UpdateCommitment is a no-op.
func (NoopStore) UpdateCommitment(context.Context, string, string, int64, int64) error {
	return nil
}

// LoadCommitments returns nil.
func (NoopStore) LoadCommitments(context.Context, string, CommitmentQuery) ([]Commitment, error) {
	return nil, nil
}

// UpsertPerson is a no-op.
func (NoopStore) UpsertPerson(context.Context, Person, int) error { return nil }

// SeedPerson is a no-op.
func (NoopStore) SeedPerson(context.Context, Person) error { return nil }

// LoadPeople returns nil.
func (NoopStore) LoadPeople(context.Context, string) ([]Person, error) {
	return nil, nil
}

// SaveActionRecord is a no-op.
func (NoopStore) SaveActionRecord(context.Context, string, ActionRecord) error {
	return nil
}

// LoadActionHistory returns nil.
func (NoopStore) LoadActionHistory(context.Context, string, int) ([]ActionRecord, error) {
	return nil, nil
}

// Close is a no-op.
func (NoopStore) Close() error { return nil }
