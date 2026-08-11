package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

func TestAllToolInputsRequireAgentAndDecisionEpoch(t *testing.T) {
	inputs := []any{
		// Composite tools (5)
		WorkShiftInput{}, ChargeAtStationInput{}, SelfMaintenanceInput{},
		RestAtResidenceInput{}, SurfInternetInput{},
		// Atomic tools (7) + control tools (stop/scan_area)
		GenericActInput{}, MoveToInput{}, TurnToInput{},
		SpeakInput{}, EmoteInput{}, InteractInput{}, WaitInput{},
		ScanAreaInput{}, StopInput{},
	}
	if len(inputs) != 14 {
		t.Fatalf("input count=%d, want 14", len(inputs))
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

func TestPascalToSnake(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MoveTo", "move_to"},
		{"TurnTo", "turn_to"},
		{"WorkShift", "work_shift"},
		{"ChargeAtStation", "charge_at_station"},
		{"Speak", "speak"},
		{"", ""},
	}
	for _, c := range cases {
		if got := pascalToSnake(c.in); got != c.want {
			t.Errorf("pascalToSnake(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestSnakeToPascal(t *testing.T) {
	cases := []struct{ in, want string }{
		{"move_to", "MoveTo"},
		{"turn_to", "TurnTo"},
		{"speak", "Speak"},
		{"", ""},
	}
	for _, c := range cases {
		if got := snakeToPascal(c.in); got != c.want {
			t.Errorf("snakeToPascal(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestCmdToToolName_Builtin(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{protocol.CmdMoveTo, "move_to"},
		{protocol.CmdInteractSmartObject, "interact"}, // shortened name
		{protocol.CmdChargeAtStation, "charge_at_station"},
		{protocol.CmdSpeak, "speak"},
	}
	for _, c := range cases {
		if got := CmdToToolName(c.cmd); got != c.want {
			t.Errorf("CmdToToolName(%q)=%q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestCmdToToolName_NewCmd(t *testing.T) {
	// Cmds not in BuiltinToolSpecs fall back to pascalToSnake.
	cases := []struct{ cmd, want string }{
		{"WaveHand", "wave_hand"},
		{"PickUpObject", "pick_up_object"},
		{"Fly", "fly"},
	}
	for _, c := range cases {
		if got := CmdToToolName(c.cmd); got != c.want {
			t.Errorf("CmdToToolName(%q)=%q, want %q (pascalToSnake fallback)", c.cmd, got, c.want)
		}
	}
}

func TestBuildInputSchemaFromParams(t *testing.T) {
	params := []protocol.CapabilityParam{
		{Name: "target_id", Type: "string", Description: "target", Required: true},
		{Name: "target_type", Type: "enum", Description: "type", Required: false, DefaultValue: "zone", EnumValues: []string{"agent", "smart_object", "zone", "position"}},
		{Name: "target_position", Type: "vector", Description: "coords", Required: false},
	}
	schema := buildInputSchemaFromParams(params)

	if schema["type"] != "object" {
		t.Errorf("schema type=%v, want object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties not a map: %T", schema["properties"])
	}
	// agent_id + decision_epoch + 3 params = 5 properties
	if len(props) != 5 {
		t.Errorf("properties count=%d, want 5", len(props))
	}
	if _, ok := props["agent_id"]; !ok {
		t.Error("missing agent_id property")
	}
	if _, ok := props["decision_epoch"]; !ok {
		t.Error("missing decision_epoch property")
	}
	typeProp := props["target_type"].(map[string]any)
	if typeProp["type"] != "string" {
		t.Errorf("target_type type=%v, want string (enum→string)", typeProp["type"])
	}
	posProp := props["target_position"].(map[string]any)
	if posProp["type"] != "array" {
		t.Errorf("target_position type=%v, want array (vector→array)", posProp["type"])
	}

	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required not []string: %T", schema["required"])
	}
	// agent_id + decision_epoch + target_id = 3 required
	wantRequired := map[string]bool{"agent_id": true, "decision_epoch": true, "target_id": true}
	if len(required) != 3 {
		t.Errorf("required count=%d, want 3: %v", len(required), required)
	}
	for _, r := range required {
		if !wantRequired[r] {
			t.Errorf("unexpected required field %q", r)
		}
	}
}

func TestJsonSchemaType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"string", "string"},
		{"enum", "string"},
		{"number", "number"},
		{"bool", "boolean"},
		{"vector", "array"},
		{"unknown", "string"}, // default
	}
	for _, c := range cases {
		if got := jsonSchemaType(c.in); got != c.want {
			t.Errorf("jsonSchemaType(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(42), 42},
		{int(42), 42},
		{float64(42.7), 42},
		{"42", 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := toInt64(c.in); got != c.want {
			t.Errorf("toInt64(%v)=%d, want %d", c.in, got, c.want)
		}
	}
}

// fakeExecutor records the last SendAction call for assertion.
type fakeExecutor struct {
	lastCmd    string
	lastParams map[string]any
	lastAgent  string
	lastEpoch  int64
}

func (f *fakeExecutor) SendAction(_ context.Context, agentID string, epoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error) {
	f.lastCmd = cmd
	f.lastParams = params
	f.lastAgent = agentID
	f.lastEpoch = epoch
	return &protocol.ActionStartedPayload{ActionID: "act_fake", EstimatedDurationSec: ptrFloat(10)}, nil
}
func (f *fakeExecutor) RequestScan(_ context.Context, _, _ string) error { return nil }
func (f *fakeExecutor) SendStopAction(_, _ string) error                 { return nil }

func ptrFloat(v float64) *float64 { return &v }

func TestReconcileTools_NewCmdRegistersGenericTool(t *testing.T) {
	// Use a real mcp.Server with InMemoryTransport to verify the generic
	// tool is callable.
	// Reset dynamic state.
	dynamicToolNames = make(map[string]struct{})

	server := mcp.NewServer(
		&mcp.Implementation{Name: "test", Version: "1.0"},
		nil,
	)
	ex := &fakeExecutor{}
	actions := []protocol.CapabilityAction{
		// All 12 built-in cmds (so none get dropped).
		{Cmd: protocol.CmdGenericAct, Kind: "atomic", Description: "generic"},
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "move"},
		{Cmd: protocol.CmdTurnTo, Kind: "atomic", Description: "turn"},
		{Cmd: protocol.CmdSpeak, Kind: "atomic", Description: "speak"},
		{Cmd: protocol.CmdEmote, Kind: "atomic", Description: "emote"},
		{Cmd: protocol.CmdInteractSmartObject, Kind: "atomic", Description: "interact"},
		{Cmd: protocol.CmdWait, Kind: "atomic", Description: "wait"},
		{Cmd: protocol.CmdWorkShift, Kind: "composite", Description: "work shift"},
		{Cmd: protocol.CmdChargeAtStation, Kind: "composite", Description: "charge"},
		{Cmd: protocol.CmdSelfMaintenance, Kind: "composite", Description: "maintenance"},
		{Cmd: protocol.CmdRestAtResidence, Kind: "composite", Description: "rest"},
		{Cmd: protocol.CmdSurfInternet, Kind: "composite", Description: "surf"},
		// New cmd not in BuiltinToolSpecs.
		{
			Cmd:         "WaveHand",
			Kind:        "atomic",
			Description: "挥手致意",
			Params: []protocol.CapabilityParam{
				{Name: "target_id", Type: "string", Required: true, Description: "目标"},
			},
		},
	}

	ReconcileTools(server, ex, nil, nil, actions)

	// Verify wave_hand was registered.
	if _, ok := dynamicToolNames["wave_hand"]; !ok {
		t.Errorf("dynamicToolNames=%v, want wave_hand", dynamicToolNames)
	}
}

