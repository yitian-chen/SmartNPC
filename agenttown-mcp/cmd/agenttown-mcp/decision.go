package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

const (
	reasonFirstPerception            = "首次感知"
	reasonZoneChanged                = "区域变化"
	reasonLocationChanged            = "具体位置变化"
	reasonVisibleAgentsChanged       = "可见Agent集合变化"
	reasonVisibleAgentActionChanged  = "可见Agent当前动作变化"
	reasonNearbyObjectsChanged       = "附近物体集合变化"
	reasonNearbyObjectStateChanged   = "附近物体状态变化"
	reasonNearbyObjectActionsChanged = "附近物体可用动作变化"
	reasonAudibleEvent               = "听觉事件"
	reasonWeatherChanged             = "天气变化"
	reasonScanResponse               = "主动扫描响应"
)

// observedSnapshot contains only perception fields that can trigger a new
// decision. Its canonical string fields make snapshots directly comparable.
type observedSnapshot struct {
	zone                string
	location            string
	visibleAgents       string
	visibleAgentActions string
	nearbyObjects       string
	nearbyObjectStates  string
	nearbyObjectActions string
	audibleEvents       string
	weather             string
}

type keyedValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type localActionSummary struct {
	ActionID      string  `json:"action_id"`
	Cmd           string  `json:"cmd,omitempty"`
	Params        string  `json:"params,omitempty"`
	DecisionEpoch int64   `json:"decision_epoch,omitempty"`
	Result        string  `json:"result"`
	DurationMs    int64   `json:"duration_ms"`
	Progress      float64 `json:"progress"`
}

type localStateSummary struct {
	TimeOfDay         string                        `json:"time_of_day,omitempty"`
	Zone              string                        `json:"zone,omitempty"`
	Location          string                        `json:"location,omitempty"`
	Physical          *protocol.PhysicalState       `json:"physical_state,omitempty"`
	CurrentTask       *protocol.CurrentTaskProgress `json:"current_task,omitempty"`
	RecentActions     []localActionSummary          `json:"recent_actions,omitempty"`
	EnvironmentEvents []string                      `json:"environment_events,omitempty"`
}

// parsePerceptionSnapshot validates a perception payload and builds the
// comparable projection used by the decision gate.
func parsePerceptionSnapshot(payload json.RawMessage) (*protocol.PerceptionPayload, observedSnapshot, error) {
	var p protocol.PerceptionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, observedSnapshot{}, fmt.Errorf("parse perception: %w", err)
	}

	visibleIDs := make([]string, 0, len(p.VisibleAgents))
	visibleActions := make([]keyedValue, 0, len(p.VisibleAgents))
	for _, agent := range p.VisibleAgents {
		key := agent.ID
		if key == "" {
			key = agent.Name
		}
		visibleIDs = append(visibleIDs, key)
		visibleActions = append(visibleActions, keyedValue{Key: key, Value: agent.CurrentAction})
	}
	sort.Strings(visibleIDs)
	sort.Slice(visibleActions, func(i, j int) bool {
		if visibleActions[i].Key == visibleActions[j].Key {
			return visibleActions[i].Value < visibleActions[j].Value
		}
		return visibleActions[i].Key < visibleActions[j].Key
	})

	objectIDs := make([]string, 0, len(p.NearbyObjects))
	objectStates := make([]keyedValue, 0, len(p.NearbyObjects))
	objectActions := make([]keyedValue, 0, len(p.NearbyObjects))
	for _, object := range p.NearbyObjects {
		key := object.ID
		if key == "" {
			key = object.Name
		}
		actions := append([]string(nil), object.AvailableActions...)
		sort.Strings(actions)
		objectIDs = append(objectIDs, key)
		objectStates = append(objectStates, keyedValue{Key: key, Value: object.State})
		objectActions = append(objectActions, keyedValue{Key: key, Value: strings.Join(actions, "\x00")})
	}
	sort.Strings(objectIDs)
	sort.Slice(objectStates, func(i, j int) bool {
		if objectStates[i].Key == objectStates[j].Key {
			return objectStates[i].Value < objectStates[j].Value
		}
		return objectStates[i].Key < objectStates[j].Key
	})
	sort.Slice(objectActions, func(i, j int) bool {
		if objectActions[i].Key == objectActions[j].Key {
			return objectActions[i].Value < objectActions[j].Value
		}
		return objectActions[i].Key < objectActions[j].Key
	})

	audible := make([]string, 0, len(p.AudibleEvents))
	for _, event := range p.AudibleEvents {
		audible = append(audible, event.Type+"\x00"+event.Source+"\x00"+event.Content)
	}
	sort.Strings(audible)

	snapshot := observedSnapshot{
		zone:                optionalString(p.Location.CurrentZone),
		location:            optionalString(p.Location.CurrentLocation),
		visibleAgents:       canonicalJSON(visibleIDs),
		visibleAgentActions: canonicalJSON(visibleActions),
		nearbyObjects:       canonicalJSON(objectIDs),
		nearbyObjectStates:  canonicalJSON(objectStates),
		nearbyObjectActions: canonicalJSON(objectActions),
		audibleEvents:       canonicalJSON(audible),
		weather:             p.Environment.Weather,
	}
	return &p, snapshot, nil
}

func perceptionTriggerReasons(p *protocol.PerceptionPayload, current observedSnapshot, previous *observedSnapshot) []string {
	if previous == nil {
		reasons := []string{reasonFirstPerception}
		if len(p.AudibleEvents) > 0 {
			reasons = append(reasons, reasonAudibleEvent)
		}
		return reasons
	}

	reasons := make([]string, 0, 9)
	if current.zone != previous.zone {
		reasons = append(reasons, reasonZoneChanged)
	}
	if current.location != previous.location {
		reasons = append(reasons, reasonLocationChanged)
	}
	if current.visibleAgents != previous.visibleAgents {
		reasons = append(reasons, reasonVisibleAgentsChanged)
	} else if current.visibleAgentActions != previous.visibleAgentActions {
		reasons = append(reasons, reasonVisibleAgentActionChanged)
	}
	if current.nearbyObjects != previous.nearbyObjects {
		reasons = append(reasons, reasonNearbyObjectsChanged)
	} else {
		if current.nearbyObjectStates != previous.nearbyObjectStates {
			reasons = append(reasons, reasonNearbyObjectStateChanged)
		}
		if current.nearbyObjectActions != previous.nearbyObjectActions {
			reasons = append(reasons, reasonNearbyObjectActionsChanged)
		}
	}
	// A repeated scan of the same snapshot must not recursively trigger, even
	// when that snapshot still contains the same audible event.
	if len(p.AudibleEvents) > 0 && current.audibleEvents != previous.audibleEvents {
		reasons = append(reasons, reasonAudibleEvent)
	}
	if current.weather != previous.weather {
		reasons = append(reasons, reasonWeatherChanged)
	}
	return reasons
}

func audibleEventExtras(events []protocol.AudibleEvent) []string {
	extras := make([]string, 0, len(events))
	for _, event := range events {
		extras = append(extras, fmt.Sprintf("环境声音 type=%s source=%s content=%s", event.Type, event.Source, event.Content))
	}
	return extras
}

func optionalString(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return "<value>" + *value
}

func summaryOptional(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func canonicalJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func buildLocalSummary(
	perceptionPayload json.RawMessage,
	physical *protocol.PhysicalState,
	task *protocol.CurrentTaskProgress,
	actions []localActionSummary,
	events []string,
) string {
	var p protocol.PerceptionPayload
	_ = json.Unmarshal(perceptionPayload, &p)
	summary := localStateSummary{
		TimeOfDay:         p.Environment.TimeOfDay,
		Zone:              summaryOptional(p.Location.CurrentZone),
		Location:          summaryOptional(p.Location.CurrentLocation),
		Physical:          clonePhysical(physical),
		CurrentTask:       cloneTask(task),
		RecentActions:     append([]localActionSummary(nil), actions...),
		EnvironmentEvents: append([]string(nil), events...),
	}
	encoded, _ := json.Marshal(summary)
	return string(encoded)
}

func truncateText(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

func appendRolling[T any](items []T, value T, max int) []T {
	items = append(items, value)
	if len(items) > max {
		items = append([]T(nil), items[len(items)-max:]...)
	}
	return items
}

func mergeUnique(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, value)
		}
	}
	return dst
}
