package protocol

import "encoding/json"

// This file defines the payload struct for each message type (§2.3).
// The envelope carries these as json.RawMessage in its Payload field.

// ─── perception_update (UE → Agent) ─────────────────────────────

// PerceptionPayload is the payload of a perception_update message.
type PerceptionPayload struct {
	Location           Location                       `json:"location"`
	PhysicalStateDelta map[string]float64             `json:"physical_state_delta,omitempty"` // only changed values over threshold
	VisibleAgents      []VisibleAgent                 `json:"visible_agents"`
	NearbyObjects      []NearbyObject                 `json:"nearby_objects"`
	AudibleEvents      []AudibleEvent                 `json:"audible_events"`
	CurrentAnimation   string                         `json:"current_animation"`
	CurrentEmote       *string                        `json:"current_emote"`
	Environment        Environment                    `json:"environment"`
	// ObjectStatusSummary is UE5's per-category aggregate of smart object
	// availability across all zones (not just the NPC's current zone). MCP
	// uses the KB to map category → semantic_group for tactical prompt
	// injection, letting the LLM avoid planning actions targeting occupied
	// objects. Optional: real UE5 pushes this; mock UE may omit it.
	ObjectStatusSummary map[string]ObjectCategoryStatus `json:"object_status_summary,omitempty"`
	// ScanID correlates an immediate scan_area request with its one-shot
	// perception response. It is transport metadata, not world state.
	ScanID string `json:"scan_id,omitempty"`
}

// ObjectCategoryStatus is UE5's per-category aggregate of smart object
// availability. UE5 groups objects by its own category strings (e.g.
// "work", "charging", "Net", "rest", "maintainance"); MCP maps these to
// the KB's semantic_group values when injecting into the tactical prompt.
// A single category may contain multiple semantic_groups (e.g. "work"
// contains both "workbench" and "sorting_conveyor"), in which case the
// tactical prompt also shows per-instance state from nearby_objects to
// disambiguate which semantic_group is occupied.
type ObjectCategoryStatus struct {
	Total    int `json:"total"`
	Idle     int `json:"idle"`
	Occupied int `json:"occupied"`
	Broken   int `json:"broken"`
}

// Location is the spatial state block of a perception.
type Location struct {
	Position        []float64 `json:"position"` // [X,Y,Z] cm
	Rotation        []float64 `json:"rotation"` // [Pitch,Yaw,Roll] degrees
	CurrentZone     *string   `json:"current_zone"`
	CurrentLocation *string   `json:"current_location"`
}

// VisibleAgent is another agent in view.
type VisibleAgent struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Distance      float64 `json:"distance"`
	Angle         float64 `json:"angle"`
	CurrentAction string  `json:"current_action"`
}

// NearbyObject is a nearby interactable smart object.
type NearbyObject struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Category              string   `json:"category,omitempty"`
	Distance              float64  `json:"distance"`
	State                 string   `json:"state"`
	AvailableInteractions []string `json:"available_interactions"`
}

// AudibleEvent is a heard sound/broadcast.
type AudibleEvent struct {
	Type    string `json:"type"`
	Source  string `json:"source"`
	Content string `json:"content"`
}

// Environment is ambient world info. 按约定 19，游戏时间由 UE（DS）权威：
// GameTimeSec 是唯一权威源（累计秒），TimeOfDaySec / DayCount 为派生字段。
type Environment struct {
	GameTimeSec  float64 `json:"game_time_sec"`             // 权威游戏时间（累计秒，DS 权威）
	TimeOfDaySec float64 `json:"time_of_day_sec"`           // 派生：当天秒数 0-86400
	DayCount     int     `json:"day_count"`                 // 派生：第几天（从 0 开始）
	TimeScale    float64 `json:"time_scale"`                // 时间倍速（游戏秒/现实秒）
	Weather      string  `json:"weather,omitempty"`         // 天气（可选）
}

// ─── scan_area (Agent → UE control) ─────────────────────────────

// ScanAreaPayload correlates a scan request with the immediate perception
// response through ScanID. It is an MCP/UE bridge control payload.
type ScanAreaPayload struct {
	ScanID string `json:"scan_id"`
}

// ─── action_command (Agent → UE) ────────────────────────────────

// ActionCommandPayload is the payload of an action_command message.
type ActionCommandPayload struct {
	ActionID  string         `json:"action_id"`
	Cmd       string         `json:"cmd"`
	Params    map[string]any `json:"params"`
	AutoQueue bool           `json:"auto_queue,omitempty"` // 约定21: true=目标 Smart Object 被占用时让 UE 自动排队而非直接失败
}

// ─── action_started (UE → Agent, ACK) ───────────────────────────

// ActionStartedPayload is the ACK for an action_command.
type ActionStartedPayload struct {
	ActionID             string   `json:"action_id"`
	Accepted             bool     `json:"accepted"`
	EstimatedDurationSec *float64 `json:"estimated_duration_sec"`
	RejectReason         string   `json:"reject_reason,omitempty"`
}

// ─── action_completed (UE → Agent) ──────────────────────────────

// ActionCompletedPayload is the completion callback for an action.
type ActionCompletedPayload struct {
	ActionID   string         `json:"action_id"`
	Result     string         `json:"result"` // success/failed/interrupted/error
	DurationMs int64          `json:"duration_ms"`
	Reason     string         `json:"reason,omitempty"` // 失败/打断/异常原因（success 时常为空）
	Progress   float64        `json:"progress"`
	Details    map[string]any `json:"details"`
}

// ─── stop_action (Agent → UE) ───────────────────────────────────

// StopActionPayload requests stopping the current action.
type StopActionPayload struct {
	ActionID string `json:"action_id"`
	Reason   string `json:"reason"`
}

// ─── action_queued (UE → Agent, 排队状态通知) ────────────────────

// ActionQueuedPayload notifies the agent of queue status changes when
// an auto_queue=true action targets an occupied Smart Object that
// supports queueing (约定21). status=queued/advanced/timeout.
type ActionQueuedPayload struct {
	ActionID         string   `json:"action_id"`
	Status           string   `json:"status"`                      // queued/advanced/timeout
	Group            string   `json:"group,omitempty"`             // 目标设施的语义组名（如 workbench）
	Position         *int     `json:"position,omitempty"`          // 排队位置（0 = 队首）
	EstimatedWaitSec *float64 `json:"estimated_wait_sec,omitempty"` // 预计等待时长（秒）
}

// ─── event_notification (Agent internal) ────────────────────────

// EventNotificationPayload is a director-injected event.
type EventNotificationPayload struct {
	EventID         string         `json:"event_id"`
	Event           map[string]any `json:"event"`
	PerceptionLevel string         `json:"perception_level"` // direct/broadcast/rumor
}

// ─── state_report (UE → Agent, authoritative physical state) ─────

// StateReportPayload is the authoritative physical-state channel.
type StateReportPayload struct {
	PhysicalState       PhysicalState        `json:"physical_state"`
	CurrentTaskProgress *CurrentTaskProgress `json:"current_task_progress,omitempty"`
}

// PhysicalState holds the four UE-owned physical values.
type PhysicalState struct {
	Energy    float64 `json:"energy"`
	Fatigue   float64 `json:"fatigue"`
	JointWear float64 `json:"joint_wear"`
	Health    float64 `json:"health"`
}

// IsZero reports whether all four physical values are zero.
// 用于检测 UE 端尚未实现物理状态上报（state_report 里 energy/fatigue/
// joint_wear/health 全为 0）的场景，三层决策据此跳过物理注入与物理告警触发，
// 避免 LLM 看到"体力=0/疲劳=0"误判为警戒带触发不合理 replan。
// UE 后续实现物理状态后自然返回非零值，此函数返回 false，三层决策自动恢复物理注入。
func (p PhysicalState) IsZero() bool {
	return p.Energy == 0 && p.Fatigue == 0 && p.JointWear == 0 && p.Health == 0
}

// CurrentTaskProgress reports the running action's progress.
type CurrentTaskProgress struct {
	ActionID string  `json:"action_id"`
	Progress float64 `json:"progress"`
}

// ─── agent_registered (UE → Agent) ──────────────────────────────

// AgentRegisteredPayload announces a robot coming online.
type AgentRegisteredPayload struct {
	AgentType       string    `json:"agent_type"`
	UE5Ref          string    `json:"ue5_ref"`
	InitialPosition []float64 `json:"initial_position"` // [X,Y,Z] cm
	InitialZone     string    `json:"initial_zone"`
}

// ─── agent_unregistered (UE → Agent) ────────────────────────────

// AgentUnregisteredPayload announces a robot going offline.
type AgentUnregisteredPayload struct {
	Reason string `json:"reason"`
}

// ─── heartbeat (bidirectional) ──────────────────────────────────

// HeartbeatPayload keeps the connection alive.
type HeartbeatPayload struct {
	UptimeSec int64 `json:"uptime_sec"`
}

// ─── error (bidirectional) ──────────────────────────────────────

// ErrorPayload reports an error condition.
type ErrorPayload struct {
	ErrorCode string         `json:"error_code"`
	Message   string         `json:"message"`
	ActionID  string         `json:"action_id,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
}

// ─── resync (control, reconnect) ────────────────────────────────

// ResyncPayload conveys the sender's last successfully received seq so the
// peer can replay discrete messages beyond it (约定11, §4.2).
type ResyncPayload struct {
	LastReceivedSeq int64 `json:"last_received_seq"`
}

// ─── event_lost (warning) ───────────────────────────────────────

// EventLostPayload signals that some discrete messages could not be
// replayed because the send buffer had already rolled past the resume
// point. The receiver should fall back to the latest snapshot (约定11).
type EventLostPayload struct {
	FromSeq int64  `json:"from_seq"` // first seq the peer wanted (last_received_seq+1)
	ToSeq   int64  `json:"to_seq"`   // oldest seq still available in the buffer
	Count   int64  `json:"count"`    // number of lost discrete messages
	Reason  string `json:"reason"`
}

// ─── capability_registry (UE → MCP, capability declaration) ────

// CapabilityRegistryPayload is sent by UE on connection (and any time
// the NPC's capability set changes) to declare which cmds it can
// execute. agent_id="system" sets the global default; a specific
// agent_id overrides the default for that agent only.
//
// MCP uses this payload to drive:
//   - tactical-layer prompt generation (which actions are available)
//   - dynamic MCP tool registration (AddTool/RemoveTools)
type CapabilityRegistryPayload struct {
	Actions []CapabilityAction `json:"actions"`
}

// CapabilityAction describes one cmd the UE can execute.
type CapabilityAction struct {
	Cmd                  string            `json:"cmd"`                       // one of Cmd* constants
	Kind                 string            `json:"kind"`                      // "atomic" | "composite"
	Description          string            `json:"description"`               // human/LLM-readable
	UsageHint            string            `json:"usage_hint,omitempty"`
	EstimatedDurationSec float64           `json:"estimated_duration_sec,omitempty"`
	Params               []CapabilityParam `json:"params,omitempty"`
}

// CapabilityParam describes one parameter of a cmd.
// Type is one of "string" | "number" | "bool" | "vector" | "enum"
// (per docs/AgentTown_CommProtocol_Values.md §2.4).
type CapabilityParam struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Description  string   `json:"description,omitempty"`
	Required     bool     `json:"required"`
	DefaultValue string   `json:"default_value,omitempty"`
	EnumValues   []string `json:"enum_values,omitempty"`
}

// ─── world_kb (UE → MCP, world knowledge base push) ─────────────

// WorldKBPayload is the payload of a world_kb message. UE pushes the full
// world KB as two JSON blobs on connection: the generated half (spatial
// facts exported by the editor) and the authored half (human narrative
// overlay). MCP merges them via the worldkb pipeline.
//
// Generated and Authored are json.RawMessage to keep the protocol package
// independent of the worldkb package — the handler unmarshals them into
// worldkb.GeneratedDoc / worldkb.AuthoredDoc after dispatch.
type WorldKBPayload struct {
	PushedAt  string          `json:"pushed_at"`           // RFC3339, optional diagnostic
	Generated json.RawMessage `json:"generated"`           // world.generated.json blob
	Authored  json.RawMessage `json:"authored"`            // world.authored.json blob
}
