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
	"time"
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

// Memory represents one row in agent_memories. Stage 4: daily-batch
// generation at day rollover, retrieval by created_at DESC.
// decay_score is persisted but always 1.0 (decay logic deferred).
type Memory struct {
	ID              int64
	AgentID         string
	MemoryType      string // "daily_summary" | "event" | "skill" | "relationship"
	Content         string
	Importance      int // 0-100, default 50
	RelatedAgentID  string // "" = NULL
	RelatedObjectID string // "" = NULL
	RelatedZoneID   string // "" = NULL
	CreatedAt       time.Time
	LastAccessedAt  time.Time
	DecayScore      float64 // always 1.0
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

// Relationship represents one directional row in agent_relationships.
// A→B and B→A are independent rows (composite PK is ordered), allowing
// asymmetric familiarity/affection in future. Stage 5 updates both
// directions on each interaction (see maybeUpdateRelationship in main.go).
type Relationship struct {
	AgentA            string
	AgentB            string
	Familiarity       int
	Affection         int
	InteractionCount  int
	LastInteractionAt time.Time
}

// Dialogue represents one chat_turn row in agent_dialogues (Phase 2 Module C).
// One row per turn: speaker says content to listener within conv_id. The
// runner writes a row per turn exchanged; memory generation loads recent
// rows for an agent (either side) to summarize the conversation.
// is_end marks the terminating turn (LLM decided no more topics or forced).
type Dialogue struct {
	ID         int64
	ConvID     string
	SpeakerID  string
	ListenerID string
	Content    string
	TurnIndex  int
	IsEnd      bool
	CreatedAt  time.Time
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

	// SaveMemory inserts a new memory row, returns the auto-increment ID.
	SaveMemory(ctx context.Context, agentID string, m Memory) (int64, error)

	// LoadRecentMemories returns the most recent N memories by created_at DESC.
	// decay_score is currently always 1.0; future decay logic can filter here.
	LoadRecentMemories(ctx context.Context, agentID string, limit int) ([]Memory, error)

	// SaveActionRecord inserts one action_history row (single INSERT at completion).
	SaveActionRecord(ctx context.Context, agentID string, r ActionRecord) error

	// LoadActionHistory returns the most recent N action records by started_at DESC.
	// Used by memory generation at day rollover to summarize yesterday's activities.
	LoadActionHistory(ctx context.Context, agentID string, limit int) ([]ActionRecord, error)

	// SaveRelationship upserts one directional relationship row (agentA→agentB).
	// On duplicate key, familiarity/affection are incremented by the deltas and
	// interaction_count is bumped by 1. Caller updates both directions (A→B and
	// B→A) by invoking this twice with swapped arguments.
	SaveRelationship(ctx context.Context, agentA, agentB string, familiarityDelta, affectionDelta int) error

	// LoadRelationships returns rows where the agent is either side (agent_a OR
	// agent_b), ordered by most recently updated first. Used to inject the
	// 【人际关系】段 in the tactical prompt.
	LoadRelationships(ctx context.Context, agentID string, limit int) ([]Relationship, error)

	// SeedRelationship inserts a relationship row only if no row yet exists
	// (INSERT IGNORE). Used at cold start to import KB seed values without
	// overwriting interaction counts accumulated in a previous run.
	SeedRelationship(ctx context.Context, agentA, agentB string, familiarity, affection int) error

	// SaveDialogue inserts one dialogue turn row (Phase 2 Module C). Called
	// by the dialogue runner after each chat_turn is exchanged so memory
	// generation can summarize recent conversations per agent.
	SaveDialogue(ctx context.Context, d Dialogue) error

	// LoadRecentDialogues returns the most recent N dialogue turns involving
	// agentID (as speaker or listener), ordered by created_at DESC. Used by
	// memory generation to summarize yesterday's conversations.
	LoadRecentDialogues(ctx context.Context, agentID string, limit int) ([]Dialogue, error)

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

// SaveMemory is a no-op. Returns 0 ID (no row inserted).
func (NoopStore) SaveMemory(context.Context, string, Memory) (int64, error) {
	return 0, nil
}

// LoadRecentMemories returns nil (no memories in in-memory mode).
func (NoopStore) LoadRecentMemories(context.Context, string, int) ([]Memory, error) {
	return nil, nil
}

// SaveActionRecord is a no-op.
func (NoopStore) SaveActionRecord(context.Context, string, ActionRecord) error {
	return nil
}

// LoadActionHistory returns nil (no history in in-memory mode).
func (NoopStore) LoadActionHistory(context.Context, string, int) ([]ActionRecord, error) {
	return nil, nil
}

// SaveRelationship is a no-op.
func (NoopStore) SaveRelationship(context.Context, string, string, int, int) error {
	return nil
}

// LoadRelationships returns nil (no relationships in in-memory mode).
func (NoopStore) LoadRelationships(context.Context, string, int) ([]Relationship, error) {
	return nil, nil
}

// SeedRelationship is a no-op.
func (NoopStore) SeedRelationship(context.Context, string, string, int, int) error {
	return nil
}

// SaveDialogue is a no-op.
func (NoopStore) SaveDialogue(context.Context, Dialogue) error {
	return nil
}

// LoadRecentDialogues returns nil (no dialogues in in-memory mode).
func (NoopStore) LoadRecentDialogues(context.Context, string, int) ([]Dialogue, error) {
	return nil, nil
}

// Close is a no-op.
func (NoopStore) Close() error { return nil }
