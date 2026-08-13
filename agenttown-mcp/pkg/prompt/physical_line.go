// Package prompt — shared physical state line helper.
//
// PhysicalLine is used by the strategic and tactical layers to render the
// 物理状态 line in a consistent format. When UE5 has not yet reported any
// state_report (physical == nil or all-zero), a default "fresh" state is
// substituted so the LLM still sees a valid physical context instead of an
// empty line — this is the common case during phase-1 real-UE5 integration
// before state_report is implemented.
package prompt

import (
	"fmt"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// defaultPhysical is the fallback physical state used when UE5 has not
// reported perception_update yet. Represents a fully-rested NPC: energy full,
// no fatigue, no wear. Matches the mock_ue initial values
// (energy=100, fatigue=0, joint_wear=0).
var defaultPhysical = protocol.PhysicalState{
	Energy:    100,
	Fatigue:   0,
	JointWear: 0,
}

// PhysicalLine renders the 物理状态 line for prompt injection.
//
// When physical is nil or all-zero (UE5 perception_update not yet received),
// falls back to defaultPhysical so the prompt always carries a valid
// physical context. Returns the formatted line WITH a trailing newline
// so callers can concat it into the prompt skeleton without extra spacing.
//
// The trailing newline matches the existing tactical prompt convention
// where physicalLine sits on its own line in the skeleton.
func PhysicalLine(physical *protocol.PhysicalState) string {
	p := defaultPhysical
	if physical != nil && !physical.IsZero() {
		p = *physical
	}
	return fmt.Sprintf("物理状态：能量 %.0f、疲劳 %.0f、关节磨损 %.0f。", p.Energy, p.Fatigue, p.JointWear)
}
