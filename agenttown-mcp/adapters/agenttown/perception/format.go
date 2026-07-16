// Package perception converts structured perception JSON from Mock UE into
// the natural-language text that MCP pushes to Hermes Gateway.
//
// The format mirrors what the Python Mock UE previously built inline
// (mock_ue.py:_build_perception_text). Keeping the NL shape consistent
// ensures the SOUL.md personality reacts identically regardless of which
// side of the WS pipe generated the text.
package perception

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Snapshot is the perception payload Mock UE pushes over WebSocket.
type Snapshot struct {
	AgentID       string   `json:"agent_id"`
	Phase         string   `json:"phase"`           // "day_start" | "perception" | "day_end"
	Time          TimeSpec `json:"time"`
	Position      []float64 `json:"position"`
	Zone          string   `json:"zone"`
	Energy        float64  `json:"energy"`
	Fatigue       float64  `json:"fatigue"`
	Holding       *string  `json:"holding,omitempty"`
	CurrentAction *string  `json:"current_action,omitempty"`
	NearbyObjects []string `json:"nearby_objects,omitempty"`
	Event         string   `json:"event,omitempty"` // scenario injection line, if matched
}

// TimeSpec is the game-time portion of a Snapshot.
type TimeSpec struct {
	Day     int    `json:"day"`
	Hour    int    `json:"hour"`
	Minute  int    `json:"minute"`
	Display string `json:"display"`
}

// Format converts a perception JSON blob (an Event.data field from Mock UE)
// into the natural-language text Hermes receives via /v1/responses.
//
// Returns "" if data does not parse as a Snapshot.
func Format(data json.RawMessage) string {
	var p Snapshot
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	return FormatSnapshot(&p)
}

// FormatSnapshot is the typed entry point. Exposed for tests.
func FormatSnapshot(p *Snapshot) string {
	if p == nil {
		return ""
	}

	period := periodOf(p.Time.Hour)
	lines := make([]string, 0, 8)

	switch p.Phase {
	case "day_start":
		lines = append(lines, "[SYSTEM] 新的一天开始了。")
	case "day_end":
		lines = append(lines, "[SYSTEM] 一天工作结束。")
	}

	lines = append(lines, fmt.Sprintf("[感知] %s，时间%s。", period, p.Time.Display))
	lines = append(lines, fmt.Sprintf("你在%s，电池%.0f%%。", p.Zone, p.Energy))

	if len(p.NearbyObjects) > 0 {
		lines = append(lines, "附近有: "+strings.Join(p.NearbyObjects, ", ")+"。")
	}

	// Time-based context hints, matching mock_ue.py.
	if p.Time.Hour == 12 {
		lines = append(lines, "午间维护时间到了，可以去充电。")
	} else if p.Time.Hour == 17 {
		lines = append(lines, "工作快结束了，准备收尾。")
	}

	if p.Event != "" {
		lines = append(lines, "\n[EVENT] "+p.Event)
	}

	if p.CurrentAction != nil && *p.CurrentAction != "" {
		lines = append(lines, fmt.Sprintf("当前动作: %s。", *p.CurrentAction))
	}

	if p.Phase == "day_end" {
		lines = append(lines, "请准备充电休息，说说今天的感想。")
	} else {
		lines = append(lines, "你现在想做什么？")
	}

	return strings.Join(lines, "\n")
}

// periodOf maps a 0-23 hour to a Chinese time-of-day label.
func periodOf(hour int) string {
	switch {
	case hour < 6:
		return "深夜"
	case hour == 6:
		return "清晨"
	case hour < 12:
		return "上午"
	case hour < 13:
		return "中午"
	case hour < 18:
		return "下午"
	default:
		return "傍晚"
	}
}
