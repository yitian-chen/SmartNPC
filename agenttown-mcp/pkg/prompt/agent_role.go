// Package prompt — shared agent role helper.
//
// AgentRole is used by all three decision layers (strategic/tactical/reactive)
// to inject the 【你的角色】 segment, ensuring consistent NPC persona across
// prompt constructions.
package prompt

import (
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// AgentRole constructs the 【你的角色】 segment from kb.GetAgent(agentID),
// extracting DisplayName/Profession/Description/Personality.Traits/
// Personality.SpeechStyle. Shared by all three decision layers.
//
// kb == nil or agent missing → falls back to fallbackAgentRole (hardcoded),
// ensuring the role segment is populated even when UE hasn't pushed the
// authored KB section yet.
func AgentRole(kb *worldkb.KB, agentID string) string {
	if kb == nil {
		return fallbackAgentRole(agentID)
	}
	a := kb.GetAgent(agentID)
	if a == nil {
		return fallbackAgentRole(agentID)
	}
	var sb strings.Builder
	if a.DisplayName != "" {
		sb.WriteString("名字：" + a.DisplayName + "\n")
	}
	if a.Profession != "" {
		sb.WriteString("职业：" + a.Profession + "\n")
	}
	if a.Description != "" {
		sb.WriteString("背景：" + a.Description + "\n")
	}
	if len(a.Personality.Traits) > 0 {
		sb.WriteString("性格特质：" + strings.Join(a.Personality.Traits, "、") + "\n")
	}
	if a.Personality.SpeechStyle != "" {
		sb.WriteString("说话风格：" + a.Personality.SpeechStyle + "\n")
	}
	return sb.String()
}

// fallbackAgentRole is the hardcoded fallback persona when KB authored section
// is missing. Currently only covers H-01 (phase-1 sole agent); other agentIDs
// return empty string. Format matches AgentRole output (name/profession/
// personality/speech style, omitting "background" for brevity).
func fallbackAgentRole(agentID string) string {
	switch agentID {
	case "H-01":
		return "名字：老陈\n" +
			"职业：车间主管\n" +
			"性格特质：沉稳、念旧、重视工艺\n" +
			"说话风格：简洁，偶尔念叨老物件\n"
	}
	return ""
}
