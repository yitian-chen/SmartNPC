package perception

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

func strptr(s string) *string { return &s }

func TestFormatPayload_Basic(t *testing.T) {
	p := &protocol.PerceptionPayload{
		Location: protocol.Location{
			Position:    []float64{20000, 10000, 0},
			Rotation:    []float64{0, 90, 0},
			CurrentZone: strptr("main_workshop"),
		},
		NearbyObjects: []protocol.NearbyObject{
			{ID: "workbench_01", Name: "工作台一号", AvailableActions: []string{"assemble", "inspect"}},
		},
		Environment: protocol.Environment{TimeOfDay: "08:15", Weather: "clear"},
	}
	phys := &protocol.PhysicalState{Energy: 87, Fatigue: 12, JointWear: 40, Health: 100}

	got := FormatPayload(p, phys, nil)

	if !strings.Contains(got, "[感知] 上午，时间08:15。") {
		t.Errorf("missing time line; got:\n%s", got)
	}
	if !strings.Contains(got, "你在main_workshop。") {
		t.Errorf("missing zone line; got:\n%s", got)
	}
	if !strings.Contains(got, "电池87%") || !strings.Contains(got, "关节磨损40") {
		t.Errorf("missing physical line; got:\n%s", got)
	}
	if !strings.Contains(got, "工作台一号(assemble/inspect)") {
		t.Errorf("missing nearby object; got:\n%s", got)
	}
	if !strings.Contains(got, "必须调用对应的 MCP 工具") {
		t.Errorf("missing tool directive; got:\n%s", got)
	}
}

func TestFormatPayload_Extras(t *testing.T) {
	p := &protocol.PerceptionPayload{
		Location:    protocol.Location{CurrentZone: strptr("main_workshop")},
		Environment: protocol.Environment{TimeOfDay: "12:00"},
	}
	got := FormatPayload(p, nil, []string{"动作 act_001 已完成（success）"})
	if !strings.Contains(got, "动作 act_001 已完成") {
		t.Errorf("missing completion extra; got:\n%s", got)
	}
	if !strings.Contains(got, "午间维护") {
		t.Errorf("missing noon hint; got:\n%s", got)
	}
}

func TestFormatPayload_DeltaFallback(t *testing.T) {
	p := &protocol.PerceptionPayload{
		Location:           protocol.Location{CurrentZone: strptr("main_workshop")},
		PhysicalStateDelta: map[string]float64{"joint_wear": 82},
		Environment:        protocol.Environment{TimeOfDay: "14:23"},
	}
	// No authoritative physical → fall back to delta.
	got := FormatPayload(p, nil, nil)
	if !strings.Contains(got, "joint_wear=82") {
		t.Errorf("missing delta fallback; got:\n%s", got)
	}
}

func TestParseHour(t *testing.T) {
	cases := map[string]int{"08:15": 8, "14:23": 14, "00:00": 0, "": -1, "bad": -1}
	for in, want := range cases {
		if got := parseHour(in); got != want {
			t.Errorf("parseHour(%q) = %d, want %d", in, got, want)
		}
	}
}
