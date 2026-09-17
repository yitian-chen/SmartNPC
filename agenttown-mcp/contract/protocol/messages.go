package protocol

import "encoding/json"

// This file defines the payload struct for each message type (§2.3).
// The envelope carries these as json.RawMessage in its Payload field.

// ─── perception_update (UE → Agent) ─────────────────────────────

// PerceptionPayload is the payload of a perception_update message.
type PerceptionPayload struct {
	Location           Location           `json:"location"`
	PhysicalStateDelta map[string]float64 `json:"physical_state_delta,omitempty"` // full physical state uploaded by UE5 (energy/fatigue/joint_wear)
	VisibleAgents      []VisibleAgent     `json:"visible_agents"`
	NearbyObjects      []NearbyObject     `json:"nearby_objects"`
	AudibleEvents      []AudibleEvent     `json:"audible_events"`
	CurrentAnimation   string             `json:"current_animation"`
	CurrentEmote       *string            `json:"current_emote"`
	Environment        Environment        `json:"environment"`
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
	GameTimeSec  float64 `json:"game_time_sec"`     // 权威游戏时间（累计秒，DS 权威）
	TimeOfDaySec float64 `json:"time_of_day_sec"`   // 派生：当天秒数 0-86400
	DayCount     int     `json:"day_count"`         // 派生：第几天（从 0 开始）
	TimeScale    float64 `json:"time_scale"`        // 时间倍速（游戏秒/现实秒）
	Weather      string  `json:"weather,omitempty"` // 天气（可选）
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
	Status           string   `json:"status"`                       // queued/advanced/timeout
	Group            string   `json:"group,omitempty"`              // 目标设施的语义组名（如 workbench）
	Position         *int     `json:"position,omitempty"`           // 排队位置（0 = 队首）
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

// PhysicalState holds the UE-owned physical values. Money is the NPC's
// economic balance (working at workbench/sorting earns, charging/repair/
// sleep spends); UE uploads it alongside energy/fatigue/joint_wear in
// perception_update's physical_state_delta map.
type PhysicalState struct {
	Energy    float64 `json:"energy"`
	Fatigue   float64 `json:"fatigue"`
	JointWear float64 `json:"joint_wear"`
	Money     float64 `json:"money"`
}

// IsZero reports whether all three physical values are zero.
// 用于检测 UE 端尚未实现物理状态上报（perception_update 里 energy/fatigue/
// joint_wear 全为 0）的场景，三层决策据此跳过物理注入与物理告警触发，
// 避免 LLM 看到"体力=0/疲劳=0"误判为警戒带触发不合理 replan。
// UE 后续实现物理状态后自然返回非零值，此函数返回 false，三层决策自动恢复物理注入。
func (p PhysicalState) IsZero() bool {
	return p.Energy == 0 && p.Fatigue == 0 && p.JointWear == 0
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
	Cmd                  string            `json:"cmd"`         // one of Cmd* constants
	Kind                 string            `json:"kind"`        // "atomic" | "composite"
	Description          string            `json:"description"` // human/LLM-readable
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
	PushedAt  string          `json:"pushed_at"` // RFC3339, optional diagnostic
	Generated json.RawMessage `json:"generated"` // world.generated.json blob
	Authored  json.RawMessage `json:"authored"`  // world.authored.json blob
}

// ─── chat_invite (UE → target agent B, §3.1) ───────────────────
//
// UE delivers A's invitation to B after A's social_chat action_command
// opens a session. conv_id is UE-generated and threads through the
// subsequent rsp/turn messages. content is A's opening line (the same
// content A sent in the social_chat action_command params).
type ChatInvitePayload struct {
	ConvID      string `json:"conv_id"`
	FromAgentID string `json:"from"` // UE sends "from", not "from_agent_id"
	Content     string `json:"content"`
}

// ─── chat_invite_rsp (B → UE, forwarded to A) ──────────────────
//
// Carries ONLY the accept/reject decision. Any reply content goes as a
// separate chat_turn (§3.1 职责分离: rsp 只承载决策，所有话语都走 chat_turn).
type ChatInviteRspPayload struct {
	ConvID string `json:"conv_id"`
	Accept bool   `json:"accept"`
}

// ─── chat_turn (speaker → UE, forwarded to peer) ─────────────────
//
// One utterance in an active dialogue. end=true signals graceful close
// (speaker is done talking); interrupted=true signals the peer already
// left and this turn is a best-effort tail (§3.5 fallback). Both flags
// default to false (omitempty) for normal mid-conversation turns.
type ChatTurnPayload struct {
	ConvID      string `json:"conv_id"`
	Content     string `json:"content"`
	End         bool   `json:"end,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
}

// ─── world_event (UE → Agent, 事件驱动设计 §四) ─────────────────
//
// docs/AgentTown_WorldEvent_Protocol.md v1.0. UE pushes the moment
// something happens (edge-triggered): the envelope's agent_id is the
// RECEIVING NPC; force=true is the hard-guarantee channel (§四).

// WorldEventPayload is the payload of a world_event message. All fields
// except Subject/Location are required by the protocol; Data is a
// category-specific block (§三) kept as RawMessage so consumers unmarshal
// it into the typed *EventData structs below only when they need the
// details. Dedup key is EventID: a broadcast re-delivered by seq replay
// after reconnect must not be enqueued twice.
type WorldEventPayload struct {
	EventID    string          `json:"event_id"`           // evt_<YYYYMMDD>_<6位递增序号>；广播多 NPC 共用同一 id
	Category   string          `json:"category"`           // Category* constant
	EventType  string          `json:"event_type"`         // EventType* constant
	Force      bool            `json:"force"`              // true = 硬保证通道：不路由、不调 LLM、不可否决
	Severity   int             `json:"severity"`           // UE 视角的客观严重度 0~10；紧急与否由 Agent 结合关系/性格裁决
	Subject    string          `json:"subject,omitempty"`  // 事件主体（谁的能量、谁在搭话、谁故障）
	GameTime   string          `json:"game_time"`          // 事件发生时刻的游戏时间（"D12 10:47:03"，决策依据）
	Location   string          `json:"location,omitempty"` // zone id 或物体 id
	OccurredAt int64           `json:"occurred_at"`        // Unix epoch 毫秒（真实时间戳，仅测量/调试用）
	Data       json.RawMessage `json:"data"`               // 类别专属载荷（§三），空对象合法
}

// world_event category constants (§三). Agent 侧按 category 走对应处理
// 分支；event_type 是类别内枚举，后续新增先修订协议再实现。
const (
	CategoryPhysicalThreshold = "physical_threshold" // 物理跨阈值（边沿触发）
	CategorySpatial           = "spatial"            // 空间变化（zone 进出、NPC 接近/离开）
	CategorySocial            = "social"             // 社交（对话邀请、广播、被点名）
	CategoryActionAnomaly     = "action_anomaly"     // 动作异常（失败、目标被占用）
	CategoryWorld             = "world"              // 世界事件（Director 故障/环境/剧情）
	CategoryPlayerInteraction = "player_interaction" // 玩家互动（攻击/瞄准/脱战/交互）
)

// world_event event_type constants (§三). 本期 UE 必须实现的枚举全集。
const (
	// physical_threshold
	EventTypeEnergyBelow    = "energy_below"     // 能量向下跨过 20
	EventTypeEnergyAbove    = "energy_above"     // 能量向上跨过 80
	EventTypeFatigueAbove   = "fatigue_above"    // 疲劳向上跨过 70
	EventTypeJointWearAbove = "joint_wear_above" // 关节磨损向上跨过 60
	EventTypeMoneyBelow     = "money_below"      // 余额向下跨过 50
	// spatial
	EventTypeZoneEnter        = "zone_enter"         // 进入新 zone
	EventTypeZoneExit         = "zone_exit"          // 离开 zone
	EventTypeAgentNearby      = "agent_nearby"       // 另一 NPC 进入对话距离（500cm）
	EventTypeAgentLeaveNearby = "agent_leave_nearby" // 另一 NPC 离开对话距离
	// social
	EventTypeChatInviteIncoming = "chat_invite_incoming" // 有人发起对话（替代独立 chat_invite 消息，§3.3）
	EventTypeBroadcastHeard     = "broadcast_heard"      // 听到广播/大声说话
	EventTypeMentioned          = "mentioned"            // 被点名/被提及
	// action_anomaly
	EventTypeActionFailed        = "action_failed"        // 动作执行失败（不再走 error 通道，§3.4）
	EventTypeSmartObjectOccupied = "smartobject_occupied" // 目标 Smart Object 被占用
	// world
	EventTypeMalfunction       = "malfunction"        // 设备/NPC 故障
	EventTypeEnvironmentChange = "environment_change" // 环境变化（天气、停水停电）
	EventTypeDirectorDirective = "director_directive" // 剧情指令（固定 force=true）
	// player_interaction
	EventTypePlayerAttacked = "player_attacked" // 被玩家攻击（固定 force=true）
	EventTypePlayerTargeted = "player_targeted" // 被玩家瞄准/锁定（固定 force=true）
	EventTypeCombatExit     = "combat_exit"     // 脱离战斗
	EventTypePlayerInteract = "player_interact" // 玩家发起交互（对话/给物品等）
)

// physical_threshold data 的 attribute 枚举（§3.1）与 direction 枚举。
const (
	ThresholdAttrEnergy    = "energy"
	ThresholdAttrFatigue   = "fatigue"
	ThresholdAttrJointWear = "joint_wear"
	ThresholdAttrMoney     = "money"

	ThresholdDirectionBelow = "below" // 向下穿
	ThresholdDirectionAbove = "above" // 向上穿
)

// PhysicalThresholdData is the data block of category physical_threshold
// (§3.1): the edge-crossing snapshot of one attribute.
type PhysicalThresholdData struct {
	Attribute string  `json:"attribute"` // ThresholdAttr* constant
	Value     float64 `json:"value"`     // 跨越时刻的实际值
	Threshold float64 `json:"threshold"` // 被跨越的阈值
	Direction string  `json:"direction"` // ThresholdDirection* constant
}

// SpatialData is the data block of category spatial (§3.2). ZoneEnter fills
// Zone+From; ZoneExit fills Zone+To; the agent_nearby pair fills
// OtherAgent+DistanceCm.
type SpatialData struct {
	Zone       string  `json:"zone,omitempty"`        // zone_enter=进入的 zone；zone_exit=离开的 zone
	From       string  `json:"from,omitempty"`        // zone_enter：来源 zone
	To         string  `json:"to,omitempty"`          // zone_exit：去向 zone
	OtherAgent string  `json:"other_agent,omitempty"` // 对方 agent id
	DistanceCm float64 `json:"distance_cm,omitempty"` // 对话距离（阈值固定 500cm）
}

// SocialData is the data block of category social (§3.3). ChatInviteIncoming
// fills ConvID+From+Content (the three fields of the legacy chat_invite
// payload, verbatim); BroadcastHeard fills Source+Content; Mentioned fills
// Source+Context.
type SocialData struct {
	ConvID  string `json:"conv_id,omitempty"` // 对话会话 id（UE 生成）
	From    string `json:"from,omitempty"`    // 发起方 agent id（UE 发 "from"）
	Source  string `json:"source,omitempty"`  // 广播/提名的来源 agent id
	Content string `json:"content,omitempty"` // 消息内容
	Context string `json:"context,omitempty"` // 被提及时的上下文
}

// ActionAnomalyData is the data block of category action_anomaly (§3.4).
// ActionFailed fills Cmd+Reason; SmartObjectOccupied fills
// SemanticGroup+OccupiedBy.
type ActionAnomalyData struct {
	ActionID      string `json:"action_id"`                // 失败/被占用影响的动作 id
	Cmd           string `json:"cmd,omitempty"`            // action_failed：失败的 cmd
	Reason        string `json:"reason,omitempty"`         // action_failed：失败原因（unreachable 等）
	SemanticGroup string `json:"semantic_group,omitempty"` // smartobject_occupied：目标语义组名
	OccupiedBy    string `json:"occupied_by,omitempty"`    // smartobject_occupied：占用者 agent id
}

// WorldEventData is the data block of category world (§3.5): Director /
// debug-tool injected world happenings.
type WorldEventData struct {
	Target      string `json:"target,omitempty"`      // malfunction：故障对象（如 K-03）
	Description string `json:"description,omitempty"` // 事件描述
}

// PlayerInteractionData is the data block of category player_interaction
// (§3.6). Attacker serves player_attacked/player_targeted/combat_exit;
// Player serves player_interact.
type PlayerInteractionData struct {
	Attacker   string  `json:"attacker,omitempty"`    // 玩家标识（单机统一 player_1）
	Player     string  `json:"player,omitempty"`      // player_interact 的玩家标识
	Damage     float64 `json:"damage,omitempty"`      // player_attacked：伤害值
	DamageType string  `json:"damage_type,omitempty"` // physical / energy / ...
	Outcome    string  `json:"outcome,omitempty"`     // combat_exit：escaped / defeated / disengaged
	Action     string  `json:"action,omitempty"`      // player_interact：greet / give_item / push / ...
	Detail     string  `json:"detail,omitempty"`      // player_interact：附加信息
}
