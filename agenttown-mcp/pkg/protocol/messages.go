package protocol

// This file defines the payload struct for each message type (§2.3).
// The envelope carries these as json.RawMessage in its Payload field.

// ─── perception_update (UE → Agent) ─────────────────────────────

// PerceptionPayload is the payload of a perception_update message.
type PerceptionPayload struct {
	Location           Location            `json:"location"`
	PhysicalStateDelta map[string]float64  `json:"physical_state_delta,omitempty"` // only changed values over threshold
	VisibleAgents      []VisibleAgent      `json:"visible_agents"`
	NearbyObjects      []NearbyObject      `json:"nearby_objects"`
	AudibleEvents      []AudibleEvent      `json:"audible_events"`
	CurrentAnimation   string              `json:"current_animation"`
	CurrentEmote       *string             `json:"current_emote"`
	Environment        Environment         `json:"environment"`
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
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Distance         float64  `json:"distance"`
	State            string   `json:"state"`
	AvailableActions []string `json:"available_actions"`
}

// AudibleEvent is a heard sound/broadcast.
type AudibleEvent struct {
	Type    string `json:"type"`
	Source  string `json:"source"`
	Content string `json:"content"`
}

// Environment is ambient world info.
type Environment struct {
	TimeOfDay string `json:"time_of_day"`
	Weather   string `json:"weather"`
}

// ─── action_command (Agent → UE) ────────────────────────────────

// ActionCommandPayload is the payload of an action_command message.
type ActionCommandPayload struct {
	ActionID string         `json:"action_id"`
	Cmd      string         `json:"cmd"`
	Params   map[string]any `json:"params"`
}

// ─── action_started (UE → Agent, ACK) ───────────────────────────

// ActionStartedPayload is the ACK for an action_command.
type ActionStartedPayload struct {
	ActionID              string   `json:"action_id"`
	Accepted              bool     `json:"accepted"`
	EstimatedDurationSec  *float64 `json:"estimated_duration_sec"`
	RejectReason          string   `json:"reject_reason,omitempty"`
}

// ─── action_completed (UE → Agent) ──────────────────────────────

// ActionCompletedPayload is the completion callback for an action.
type ActionCompletedPayload struct {
	ActionID   string         `json:"action_id"`
	Result     string         `json:"result"` // success/failed/interrupted/error
	DurationMs int64          `json:"duration_ms"`
	Progress   float64        `json:"progress"`
	Details    map[string]any `json:"details"`
}

// ─── stop_action (Agent → UE) ───────────────────────────────────

// StopActionPayload requests stopping the current action.
type StopActionPayload struct {
	ActionID string `json:"action_id"`
	Reason   string `json:"reason"`
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
