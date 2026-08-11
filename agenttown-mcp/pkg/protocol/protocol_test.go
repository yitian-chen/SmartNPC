package protocol

import (
	"encoding/json"
	"strings"
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
				AvailableInteractions: []string{"assemble", "inspect"}},
		},
		AudibleEvents:    []AudibleEvent{},
		CurrentAnimation: "idle",
		Environment:      Environment{GameTimeSec: 51780, TimeOfDaySec: 51780, DayCount: 0, TimeScale: 60, Weather: "clear"},
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
	if gotPP.Environment.TimeOfDaySec != 51780 || len(gotPP.NearbyObjects) != 1 {
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
		Params:   map[string]any{"target_type": "zone", "target_id": "workshop"},
	}
	raw, _ := json.Marshal(cmd)
	var gotCmd ActionCommandPayload
	if json.Unmarshal(raw, &gotCmd) != nil || gotCmd.ActionID != "act_001" || gotCmd.Cmd != CmdMoveTo {
		t.Fatalf("action_command round-trip failed: %+v", gotCmd)
	}

	// AutoQueue=false (default) should omit the field from JSON for
	// backward compatibility with UE servers that don't know about
	// auto_queue yet.
	rawDefault, _ := json.Marshal(cmd)
	if strings.Contains(string(rawDefault), "auto_queue") {
		t.Fatalf("auto_queue should be omitted when false: %s", rawDefault)
	}

	// AutoQueue=true should marshal the field.
	cmdQueue := ActionCommandPayload{
		ActionID:  "act_002",
		Cmd:       CmdWorkShift,
		Params:    map[string]any{"smart_object": "workbench_01"},
		AutoQueue: true,
	}
	rawQueue, _ := json.Marshal(cmdQueue)
	var gotQueue ActionCommandPayload
	if err := json.Unmarshal(rawQueue, &gotQueue); err != nil || !gotQueue.AutoQueue {
		t.Fatalf("auto_queue=true round-trip failed: %+v err=%v", gotQueue, err)
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

	// 验证 failed + reason 字段能正确 round-trip（UE 同事新增的 reason 字段）
	failDone := ActionCompletedPayload{
		ActionID: "act_002", Result: ResultFailed, Reason: "寻路不可达", Progress: 0.3,
	}
	rawFail, _ := json.Marshal(failDone)
	var gotFail ActionCompletedPayload
	if err := json.Unmarshal(rawFail, &gotFail); err != nil {
		t.Fatalf("action_completed (failed) unmarshal failed: %v", err)
	}
	if gotFail.Reason != "寻路不可达" {
		t.Errorf("action_completed reason round-trip failed: got %q, want %q", gotFail.Reason, "寻路不可达")
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

// TestActionQueuedPayload verifies the action_queued message payload
// round-trips with all fields including optional pointer fields.
func TestActionQueuedPayload(t *testing.T) {
	pos := 2
	wait := 30.0
	aq := ActionQueuedPayload{
		ActionID:         "act_001",
		Status:           QueueStatusQueued,
		Group:            "workbench",
		Position:         &pos,
		EstimatedWaitSec: &wait,
	}
	raw, err := json.Marshal(aq)
	if err != nil {
		t.Fatalf("marshal action_queued: %v", err)
	}
	var got ActionQueuedPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal action_queued: %v", err)
	}
	if got.ActionID != "act_001" || got.Status != QueueStatusQueued || got.Group != "workbench" {
		t.Fatalf("action_queued core fields lost: %+v", got)
	}
	if got.Position == nil || *got.Position != 2 {
		t.Fatalf("position lost: %+v", got)
	}
	if got.EstimatedWaitSec == nil || *got.EstimatedWaitSec != 30.0 {
		t.Fatalf("estimated_wait_sec lost: %+v", got)
	}

	// Optional fields omitted → nil pointers.
	minimal := ActionQueuedPayload{
		ActionID: "act_002",
		Status:   QueueStatusAdvanced,
	}
	rawMin, _ := json.Marshal(minimal)
	var gotMin ActionQueuedPayload
	if err := json.Unmarshal(rawMin, &gotMin); err != nil {
		t.Fatalf("unmarshal minimal action_queued: %v", err)
	}
	if gotMin.Position != nil || gotMin.EstimatedWaitSec != nil {
		t.Fatalf("optional fields should be nil: %+v", gotMin)
	}
	// omitempty should drop the optional fields from JSON.
	if strings.Contains(string(rawMin), "position") || strings.Contains(string(rawMin), "estimated_wait_sec") {
		t.Fatalf("optional fields should be omitted from JSON: %s", rawMin)
	}

	// All three status constants should round-trip.
	for _, status := range []string{QueueStatusQueued, QueueStatusAdvanced, QueueStatusTimeout} {
		p := ActionQueuedPayload{ActionID: "act_x", Status: status}
		raw, _ := json.Marshal(p)
		var got ActionQueuedPayload
		if err := json.Unmarshal(raw, &got); err != nil || got.Status != status {
			t.Fatalf("status %q round-trip failed: %+v", status, got)
		}
	}
}
