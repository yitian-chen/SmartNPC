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

// TestPerceptionObjectStatusSummary_Unmarshal verifies that UE5's real
// perception_update payload (which includes object_status_summary and
// nearby_objects[].category) is correctly parsed into the new struct
// fields. Regression guard for the Fix B bug where these fields were
// dropped during JSON unmarshal because the struct lacked matching tags.
func TestPerceptionObjectStatusSummary_Unmarshal(t *testing.T) {
	// Payload shape observed in real UE5 logs (logs/2026-08-12/debug-mcp.log).
	raw := []byte(`{
		"location":{"position":[6421.8,6831.0,-21109.6],"rotation":[0,-156.3,0],"current_zone":"main_workshop"},
		"nearby_objects":[
			{"id":"WorkBench","category":"work","state":"occupied","distance":152.8,"available_interactions":["assemble"]}
		],
		"object_status_summary":{
			"charging":{"total":6,"idle":6,"occupied":0,"broken":0},
			"maintainance":{"total":1,"idle":1,"occupied":0,"broken":0},
			"Net":{"total":1,"idle":1,"occupied":0,"broken":0},
			"rest":{"total":1,"idle":1,"occupied":0,"broken":0},
			"work":{"total":2,"idle":1,"occupied":1,"broken":0}
		},
		"environment":{"game_time_sec":36693.7,"time_of_day_sec":36693.7,"day_count":0,"time_scale":120}
	}`)
	var got PerceptionPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(got.ObjectStatusSummary) != 5 {
		t.Errorf("ObjectStatusSummary: want 5 categories, got %d (%+v)", len(got.ObjectStatusSummary), got.ObjectStatusSummary)
	}
	work, ok := got.ObjectStatusSummary["work"]
	if !ok {
		t.Fatalf("ObjectStatusSummary missing 'work' category: %+v", got.ObjectStatusSummary)
	}
	if work.Total != 2 || work.Idle != 1 || work.Occupied != 1 || work.Broken != 0 {
		t.Errorf("work category status mismatch: got %+v, want {Total:2 Idle:1 Occupied:1 Broken:0}", work)
	}
	if len(got.NearbyObjects) != 1 {
		t.Fatalf("NearbyObjects: want 1, got %d", len(got.NearbyObjects))
	}
	nb := got.NearbyObjects[0]
	if nb.ID != "WorkBench" || nb.Category != "work" || nb.State != "occupied" {
		t.Errorf("NearbyObject mismatch: got %+v, want {ID:WorkBench Category:work State:occupied}", nb)
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

// TestStateReportPayload verifies the three physical values.
func TestStateReportPayload(t *testing.T) {
	sr := StateReportPayload{
		PhysicalState:       PhysicalState{Energy: 20, Fatigue: 70, JointWear: 85},
		CurrentTaskProgress: &CurrentTaskProgress{ActionID: "act_010", Progress: 0.6},
	}
	raw, _ := json.Marshal(sr)
	var got StateReportPayload
	if json.Unmarshal(raw, &got) != nil {
		t.Fatal("state_report unmarshal failed")
	}
	if got.PhysicalState.Energy != 20 || got.PhysicalState.JointWear != 85 {
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

// TestSocialChatCmdConstants verifies the Phase 2 Module C dialogue cmd
// and message-type constants exist and IsCompositeCmd recognizes SocialChat.
func TestSocialChatCmdConstants(t *testing.T) {
	if CmdSocialChat != "SocialChat" {
		t.Fatalf("CmdSocialChat = %q, want %q", CmdSocialChat, "SocialChat")
	}
	if !IsCompositeCmd(CmdSocialChat) {
		t.Fatalf("IsCompositeCmd(%q) = false, want true", CmdSocialChat)
	}
	// Existing composites must still be recognized.
	for _, c := range []string{CmdWorkShift, CmdChargeAtStation, CmdSelfMaintenance, CmdRestAtResidence, CmdSurfInternet} {
		if !IsCompositeCmd(c) {
			t.Fatalf("IsCompositeCmd(%q) = false, want true (regression)", c)
		}
	}
	// Atomic cmds must NOT be composite.
	for _, c := range []string{CmdMoveTo, CmdSpeak, CmdWait, CmdGenericAct, CmdTurnTo, CmdInteractSmartObject, CmdEmote} {
		if IsCompositeCmd(c) {
			t.Fatalf("IsCompositeCmd(%q) = true, want false (regression)", c)
		}
	}
	// Message type constants for the dialogue protocol.
	for _, tc := range []struct{ name, val string }{
		{"TypeChatInvite", TypeChatInvite},
		{"TypeChatInviteRsp", TypeChatInviteRsp},
		{"TypeChatTurn", TypeChatTurn},
	} {
		if tc.val == "" {
			t.Fatalf("%s is empty", tc.name)
		}
	}
}

// TestDialoguePayloadRoundTrip verifies the three dialogue payload structs
// (chat_invite / chat_invite_rsp / chat_turn) round-trip with all fields,
// and that optional flags are omitted when false.
func TestDialoguePayloadRoundTrip(t *testing.T) {
	// chat_invite
	invite := ChatInvitePayload{ConvID: "conv_001", FromAgentID: "H-01", Content: "最近怎么样？"}
	raw, _ := json.Marshal(invite)
	var gotInvite ChatInvitePayload
	if err := json.Unmarshal(raw, &gotInvite); err != nil {
		t.Fatalf("chat_invite unmarshal: %v", err)
	}
	if gotInvite.ConvID != "conv_001" || gotInvite.FromAgentID != "H-01" || gotInvite.Content != "最近怎么样？" {
		t.Fatalf("chat_invite fields lost: %+v", gotInvite)
	}

	// chat_invite_rsp — accept=true
	rsp := ChatInviteRspPayload{ConvID: "conv_001", Accept: true}
	raw, _ = json.Marshal(rsp)
	var gotRsp ChatInviteRspPayload
	if err := json.Unmarshal(raw, &gotRsp); err != nil || gotRsp.ConvID != "conv_001" || !gotRsp.Accept {
		t.Fatalf("chat_invite_rsp accept=true round-trip failed: %+v err=%v", gotRsp, err)
	}
	// chat_invite_rsp — accept=false must marshal the field (not omitempty).
	rspFalse := ChatInviteRspPayload{ConvID: "conv_002", Accept: false}
	rawFalse, _ := json.Marshal(rspFalse)
	if !strings.Contains(string(rawFalse), `"accept":false`) {
		t.Fatalf("chat_invite_rsp accept=false must serialize the field: %s", rawFalse)
	}

	// chat_turn — normal mid-conversation (end/interrupted omitted).
	turn := ChatTurnPayload{ConvID: "conv_001", Content: "还行，忙着装配呢"}
	raw, _ = json.Marshal(turn)
	if strings.Contains(string(raw), "end") || strings.Contains(string(raw), "interrupted") {
		t.Fatalf("chat_turn should omit end/interrupted when false: %s", raw)
	}
	var gotTurn ChatTurnPayload
	if err := json.Unmarshal(raw, &gotTurn); err != nil || gotTurn.Content != "还行，忙着装配呢" || gotTurn.End || gotTurn.Interrupted {
		t.Fatalf("chat_turn normal round-trip failed: %+v err=%v", gotTurn, err)
	}

	// chat_turn — graceful end.
	endTurn := ChatTurnPayload{ConvID: "conv_001", Content: "回头聊，我去忙了", End: true}
	raw, _ = json.Marshal(endTurn)
	var gotEnd ChatTurnPayload
	if err := json.Unmarshal(raw, &gotEnd); err != nil || !gotEnd.End || gotEnd.Interrupted {
		t.Fatalf("chat_turn end=true round-trip failed: %+v err=%v", gotEnd, err)
	}

	// chat_turn — interrupted fallback.
	intTurn := ChatTurnPayload{ConvID: "conv_001", Content: "等等？", End: true, Interrupted: true}
	raw, _ = json.Marshal(intTurn)
	var gotInt ChatTurnPayload
	if err := json.Unmarshal(raw, &gotInt); err != nil || !gotInt.End || !gotInt.Interrupted {
		t.Fatalf("chat_turn interrupted round-trip failed: %+v err=%v", gotInt, err)
	}
}
