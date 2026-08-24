package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

func TestBandOf_Boundaries(t *testing.T) {
	th := [3]float64{40, 60, 80}
	cases := []struct {
		v    float64
		want int
	}{
		{0, 0}, {39.9, 0},
		{40, 1}, {59.9, 1},
		{60, 2}, {79.9, 2},
		{80, 3}, {100, 3},
	}
	for _, c := range cases {
		if got := bandOf(c.v, th); got != c.want {
			t.Errorf("bandOf(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestDefaultBandThresholds_AlignedWithAlertConstants(t *testing.T) {
	th := DefaultBandThresholds()
	if th.EnergyAlert() != EnergyAlertThreshold {
		t.Errorf("EnergyAlert = %v, want %v", th.EnergyAlert(), EnergyAlertThreshold)
	}
	if th.FatigueAlert() != FatigueAlertThreshold {
		t.Errorf("FatigueAlert = %v, want %v", th.FatigueAlert(), FatigueAlertThreshold)
	}
	if th.JointWearAlert() != JointWearAlertThreshold {
		t.Errorf("JointWearAlert = %v, want %v", th.JointWearAlert(), JointWearAlertThreshold)
	}
}

func TestBandThresholdsFor_NilProfiles(t *testing.T) {
	if got := BandThresholdsFor(nil, "H-01"); got != DefaultBandThresholds() {
		t.Errorf("nil profiles should return defaults, got %+v", got)
	}
}

func TestBandThresholdsFor_ProfileOverride(t *testing.T) {
	profiles := map[string]*profile.Profile{
		"H-01": {AgentID: "H-01", AttrBands: map[string][3]float64{"疲劳": {40, 70, 90}}},
	}
	got := BandThresholdsFor(profiles, "H-01")
	if got.Fatigue != [3]float64{40, 70, 90} {
		t.Errorf("Fatigue = %v, want [40 70 90]", got.Fatigue)
	}
	// 未覆盖的属性保持默认
	if got.Energy != DefaultBandThresholds().Energy {
		t.Errorf("Energy = %v, want default %v", got.Energy, DefaultBandThresholds().Energy)
	}
	// 其他 agent 不受影响
	if got := BandThresholdsFor(profiles, "H-02"); got != DefaultBandThresholds() {
		t.Errorf("H-02 should get defaults, got %+v", got)
	}
}

func TestBandThresholdsFor_PerNPCDifference(t *testing.T) {
	// 老陈（耐久）：疲劳 90 才"非常疲劳"；默认 NPC 80 即"非常疲劳"。
	profiles := map[string]*profile.Profile{
		"H-01": {AgentID: "H-01", AttrBands: map[string][3]float64{"疲劳": {40, 70, 90}}},
	}
	laochen := BandThresholdsFor(profiles, "H-01")
	other := BandThresholdsFor(profiles, "H-02")
	if got := laochen.FatigueBand(85); got != "疲劳" {
		t.Errorf("老陈 fatigue 85 = %q, want 疲劳", got)
	}
	if got := other.FatigueBand(85); got != "非常疲劳" {
		t.Errorf("默认 NPC fatigue 85 = %q, want 非常疲劳", got)
	}
	if laochen.FatigueAlert() != 90 || other.FatigueAlert() != 80 {
		t.Errorf("FatigueAlert: 老陈=%v want 90, 默认=%v want 80", laochen.FatigueAlert(), other.FatigueAlert())
	}
}

func TestPhysicalLineActual_BandedOutput(t *testing.T) {
	p := protocol.PhysicalState{Energy: 10, Fatigue: 85, JointWear: 75, Money: 150}
	got := PhysicalLineActual(p, DefaultBandThresholds())
	for _, want := range []string{"电量 低电量", "疲劳 非常疲劳", "关节磨损 严重磨损", "余额 150"} {
		if !strings.Contains(got, want) {
			t.Errorf("PhysicalLineActual missing %q in: %s", want, got)
		}
	}
	// 不应出现原始数值（10/85/75）
	for _, num := range []string{"10", "85", "75"} {
		if strings.Contains(got, num) {
			t.Errorf("PhysicalLineActual should not contain raw value %q in: %s", num, got)
		}
	}
}

func TestPhysicalLine_DefaultFallbackBanded(t *testing.T) {
	got := PhysicalLine(nil, DefaultBandThresholds())
	if !strings.Contains(got, "电量 很高") || !strings.Contains(got, "疲劳 精神饱满") {
		t.Errorf("nil physical should render default fresh bands, got: %s", got)
	}
}
