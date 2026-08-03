package tools

import (
	"reflect"
	"testing"
)

func TestAllToolInputsRequireAgentAndDecisionEpoch(t *testing.T) {
	inputs := []any{
		// Composite tools (6)
		WorkAtWorkbenchInput{}, WorkAtWorkshopInput{}, ChatWithInput{}, RepairTargetInput{},
		ChargeAtStationInput{}, PatrolZoneInput{},
		// Atomic tools (8) + control tools (stop/scan_area)
		MoveToLocationInput{}, MoveToAgentInput{}, TurnToInput{}, PlayMontageInput{},
		SpeakInput{}, EmoteInput{}, InteractInput{}, WaitInput{},
		ScanAreaInput{}, StopInput{},
	}
	if len(inputs) != 16 {
		t.Fatalf("input count=%d, want 16", len(inputs))
	}
	for _, input := range inputs {
		typeOf := reflect.TypeOf(input)
		for _, fieldName := range []string{"AgentID", "DecisionEpoch"} {
			field, ok := typeOf.FieldByName(fieldName)
			if !ok {
				t.Errorf("%s missing %s", typeOf.Name(), fieldName)
				continue
			}
			if field.Tag.Get("json") == "" || field.Tag.Get("jsonschema") == "" {
				t.Errorf("%s.%s missing schema tags", typeOf.Name(), fieldName)
			}
		}
	}
}

func TestBuildAckResultEchoesDecisionEpoch(t *testing.T) {
	result := buildAckResult(nil, 42)
	if !result.OK || result.DecisionEpoch != 42 {
		t.Fatalf("result=%+v", result)
	}
}
