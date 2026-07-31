package protocol

import (
	"encoding/json"
	"testing"
)

// TestEnvelopeRoundTrip verifies the 7-field envelope marshals and
// unmarshals with all fields intact.
func TestEnvelopeRoundTrip(t *testing.T) {
	pp := PerceptionPayload{
		Location: Location{
			Position: []float64{20000, 10000, 0},
			Rotation: []float64{0, 90, 0},
		},
		VisibleAgents: []VisibleAgent{},
		NearbyObjects: []NearbyObject{
			{ID: "workbench_01", Name: "工作台一号", Distance: 8.0, State: "idle",
				AvailableActions: []string{"assemble", "inspect"}},
		},
		AudibleEvents:    []AudibleEvent{},
		CurrentAnimation: "idle",
		Environment:      Environment{TimeOfDay: "14:23", Weather: "clear"},
	}
	raw, err := json.Marshal(pp)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	env := Envelope{
		Version:   Version,
		MsgID:     "uuid-001",
		Seq:       1001,
		Timestamp: 1719456000000,
		Type:      TypePerceptionUpdate,
		AgentID:   "H-01",
		Payload:   raw,
	}
	frame, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	var got Envelope
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if got.Version != Version || got.Seq != 1001 || got.Type != TypePerceptionUpdate || got.AgentID != "H-01" {
		t.Fatalf("envelope fields lost: %+v", got)
	}

	var gotPP PerceptionPayload
	if err := json.Unmarshal(got.Payload, &gotPP); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if gotPP.Environment.TimeOfDay != "14:23" || len(gotPP.NearbyObjects) != 1 {
		t.Fatalf("payload fields lost: %+v", gotPP)
	}
	if gotPP.NearbyObjects[0].Name != "工作台一号" {
		t.Fatalf("CJK object name lost: %q", gotPP.NearbyObjects[0].Name)
	}
}

// TestActionLifecyclePayloads verifies command/started/completed payloads.
func TestScanAreaPayloadAndPerceptionScanID(t *testing.T) {
	request := ScanAreaPayload{ScanID: "scan_123"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var gotRequest ScanAreaPayload
	if err := json.Unmarshal(raw, &gotRequest); err != nil || gotRequest.ScanID != "scan_123" {
		t.Fatalf("scan request round-trip: %+v err=%v", gotRequest, err)
	}

	payload := PerceptionPayload{ScanID: "scan_123"}
	raw, _ = json.Marshal(payload)
	var gotPerception PerceptionPayload
	if err := json.Unmarshal(raw, &gotPerception); err != nil || gotPerception.ScanID != "scan_123" {
		t.Fatalf("scan perception round-trip: %+v err=%v", gotPerception, err)
	}
	var legacy PerceptionPayload
	if err := json.Unmarshal([]byte(`{"location":{},"environment":{}}`), &legacy); err != nil || legacy.ScanID != "" {
		t.Fatalf("legacy perception compatibility: %+v err=%v", legacy, err)
	}
}

func TestActionLifecyclePayloads(t *testing.T) {
	cmd := ActionCommandPayload{
		ActionID: "act_001",
		Cmd:      CmdMoveTo,
		Params:   map[string]any{"target": "central_plaza", "speed": "walk"},
	}
	raw, _ := json.Marshal(cmd)
	var gotCmd ActionCommandPayload
	if json.Unmarshal(raw, &gotCmd) != nil || gotCmd.ActionID != "act_001" || gotCmd.Cmd != CmdMoveTo {
		t.Fatalf("action_command round-trip failed: %+v", gotCmd)
	}

	est := 30.0
	ack := ActionStartedPayload{ActionID: "act_001", Accepted: true, EstimatedDurationSec: &est}
	raw, _ = json.Marshal(ack)
	var gotAck ActionStartedPayload
	if json.Unmarshal(raw, &gotAck) != nil || !gotAck.Accepted || *gotAck.EstimatedDurationSec != 30.0 {
		t.Fatalf("action_started round-trip failed: %+v", gotAck)
	}

	done := ActionCompletedPayload{ActionID: "act_001", Result: ResultSuccess, DurationMs: 30200, Progress: 1.0}
	raw, _ = json.Marshal(done)
	var gotDone ActionCompletedPayload
	if json.Unmarshal(raw, &gotDone) != nil || gotDone.Result != ResultSuccess || gotDone.DurationMs != 30200 {
		t.Fatalf("action_completed round-trip failed: %+v", gotDone)
	}
}

// TestStateReportPayload verifies the four physical values.
func TestStateReportPayload(t *testing.T) {
	sr := StateReportPayload{
		PhysicalState:       PhysicalState{Energy: 20, Fatigue: 70, JointWear: 85, Health: 90},
		CurrentTaskProgress: &CurrentTaskProgress{ActionID: "act_010", Progress: 0.6},
	}
	raw, _ := json.Marshal(sr)
	var got StateReportPayload
	if json.Unmarshal(raw, &got) != nil {
		t.Fatal("state_report unmarshal failed")
	}
	if got.PhysicalState.Energy != 20 || got.PhysicalState.JointWear != 85 || got.PhysicalState.Health != 90 {
		t.Fatalf("physical state lost: %+v", got.PhysicalState)
	}
	if got.CurrentTaskProgress == nil || got.CurrentTaskProgress.Progress != 0.6 {
		t.Fatalf("task progress lost: %+v", got.CurrentTaskProgress)
	}
}

func TestWorldKBPayloadRoundTrip(t *testing.T) {
	genBlob := json.RawMessage(`{"$schema":"agenttown-world-generated/v1","schema_version":"1.0","zones":[{"id":"z1","bounds":{"center":[0,0,0],"extent":[1,1,1]}}]}`)
	authBlob := json.RawMessage(`{"version":"1.0","narrative":{"setting":"小镇","theme":"测试"},"zones":{"z1":{"display_name":"Z1"}}}`)
	p := WorldKBPayload{
		PushedAt:  "2026-07-31T03:00:00Z",
		Generated: genBlob,
		Authored:  authBlob,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got WorldKBPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PushedAt != p.PushedAt {
		t.Errorf("PushedAt lost: %q", got.PushedAt)
	}
	if string(got.Generated) != string(genBlob) {
		t.Errorf("Generated blob corrupted: %s", got.Generated)
	}
	if string(got.Authored) != string(authBlob) {
		t.Errorf("Authored blob corrupted: %s", got.Authored)
	}

	// Verify the blob is still valid JSON that can be unmarshaled later
	// by the worldkb package (deferred deserialization pattern).
	var probe struct {
		Version string `json:"schema_version"`
	}
	if json.Unmarshal(got.Generated, &probe) != nil || probe.Version != "1.0" {
		t.Errorf("generated blob not independently parseable: %+v", probe)
	}
}
