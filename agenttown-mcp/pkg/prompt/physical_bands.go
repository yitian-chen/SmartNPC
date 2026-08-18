// Package prompt — physical state banding.
//
// Physical attribute values (energy/fatigue/joint_wear) are shown to the
// LLM as range labels ("低电量"/"非常疲劳"/…) instead of raw numbers. Each
// attribute's 0-100 range is split into 4 bands by 3 ascending thresholds;
// thresholds are per-NPC configurable via the profile.md ## 属性分段
// section (see pkg/profile), falling back to DefaultBandThresholds.
//
// The same thresholds also drive code-level alerts (reactive trigger,
// forced replan, tactical recovery constraints) via the alert accessors,
// so the LLM-visible label and the code behavior stay consistent: e.g.
// 老陈 with 疲劳: 40,70,90 only hits "非常疲劳" (and the fatigue alert) at
// ≥90, while the default NPC alerts at ≥80.
package prompt

import (
	"fmt"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// BandThresholds splits each physical attribute's 0-100 range into 4
// bands. The 3 thresholds per attribute are ascending boundaries.
type BandThresholds struct {
	Energy    [3]float64 // 低电量 <t1 | 偏低 | 中等 | 充足 ≥t3
	Fatigue   [3]float64 // 精神饱满 <t1 | 轻度疲劳 | 疲劳 | 非常疲劳 ≥t3
	JointWear [3]float64 // 良好 <t1 | 轻微磨损 | 明显磨损 | 严重磨损 ≥t3
}

// DefaultBandThresholds returns the global default band boundaries.
// Aligned with the code-level alert constants so default behavior is
// unchanged: energy alert at <40 (低电量), fatigue alert at ≥80 (非常疲劳),
// joint wear alert at ≥70 (严重磨损).
func DefaultBandThresholds() BandThresholds {
	return BandThresholds{
		Energy:    [3]float64{EnergyAlertThreshold, 60, 80},
		Fatigue:   [3]float64{40, 60, FatigueAlertThreshold},
		JointWear: [3]float64{30, 50, JointWearAlertThreshold},
	}
}

// BandThresholdsFor resolves the effective band thresholds for an agent:
// global defaults overlaid with the agent's profile ## 属性分段 overrides.
// profiles == nil or agent not found / section absent → defaults.
func BandThresholdsFor(profiles map[string]*profile.Profile, agentID string) BandThresholds {
	th := DefaultBandThresholds()
	p := profiles[agentID]
	if p == nil {
		return th
	}
	if v, ok := p.AttrBands["能量"]; ok {
		th.Energy = v
	}
	if v, ok := p.AttrBands["疲劳"]; ok {
		th.Fatigue = v
	}
	if v, ok := p.AttrBands["关节磨损"]; ok {
		th.JointWear = v
	}
	return th
}

// EnergyAlert returns the energy alert boundary: energy below this is
// "低电量" and triggers low-battery alerts.
func (b BandThresholds) EnergyAlert() float64 { return b.Energy[0] }

// OrDefault returns DefaultBandThresholds when b is the zero value (caller
// did not resolve per-NPC thresholds). Guards against zero thresholds
// making every state look like an alert (e.g. fatigue > 0 always true).
func (b BandThresholds) OrDefault() BandThresholds {
	if b == (BandThresholds{}) {
		return DefaultBandThresholds()
	}
	return b
}
// FatigueAlert returns the fatigue alert boundary: fatigue at or above
// this is "非常疲劳" and triggers fatigue alerts.
func (b BandThresholds) FatigueAlert() float64 { return b.Fatigue[2] }

// JointWearAlert returns the joint wear alert boundary: wear at or above
// this is "严重磨损" and triggers maintenance alerts.
func (b BandThresholds) JointWearAlert() float64 { return b.JointWear[2] }

// bandLabels are the fixed 4-band label ladders per attribute, indexed by
// bandOf result (0-3). Labels are global (not per-NPC configurable) — only
// the thresholds vary.
var (
	energyBandLabels    = [4]string{"低电量", "偏低", "中等", "充足"}
	fatigueBandLabels   = [4]string{"精神饱满", "轻度疲劳", "疲劳", "非常疲劳"}
	jointWearBandLabels = [4]string{"良好", "轻微磨损", "明显磨损", "严重磨损"}
)

// bandOf maps a value to its band index 0-3 given 3 ascending thresholds.
func bandOf(v float64, th [3]float64) int {
	switch {
	case v < th[0]:
		return 0
	case v < th[1]:
		return 1
	case v < th[2]:
		return 2
	default:
		return 3
	}
}

// EnergyBand returns the band label for an energy value.
func (b BandThresholds) EnergyBand(v float64) string {
	return energyBandLabels[bandOf(v, b.Energy)]
}

// FatigueBand returns the band label for a fatigue value.
func (b BandThresholds) FatigueBand(v float64) string {
	return fatigueBandLabels[bandOf(v, b.Fatigue)]
}

// JointWearBand returns the band label for a joint wear value.
func (b BandThresholds) JointWearBand(v float64) string {
	return jointWearBandLabels[bandOf(v, b.JointWear)]
}

// PhysicalLineActual renders the 物理状态 line for a concrete physical
// state (no default substitution), using band labels for energy/fatigue/
// joint_wear and a raw number for money (余额 stays numeric — it is an
// economic balance, not an alert-style state).
func PhysicalLineActual(p protocol.PhysicalState, th BandThresholds) string {
	return fmt.Sprintf("物理状态：能量 %s、疲劳 %s、关节磨损 %s、余额 %.0f。",
		th.EnergyBand(p.Energy), th.FatigueBand(p.Fatigue), th.JointWearBand(p.JointWear), p.Money)
}
