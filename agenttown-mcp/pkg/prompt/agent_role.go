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
// is missing. Covers H-01/H-02/H-03 (phase-1 three NPCs); other agentIDs
// return empty string. Format matches AgentRole output (name/profession/
// background/personality/speech style), omitting fields that are empty in the
// authored KB so fallback and KB-loaded输出 stay consistent.
func fallbackAgentRole(agentID string) string {
	switch agentID {
	case "H-01":
		return "名字：老陈\n" +
			"职业：supervisor、worker、maintainer\n" +
			"性格特质：沉稳、念旧、重视工艺、务实\n"
	case "H-02":
		return "名字：小林\n" +
			"职业：maintainer、technician\n" +
			"背景：维修技术员，专注精密装配与设备维护\n" +
			"性格特质：细致、严谨、专注技术、话少\n" +
			"说话风格：精确，技术术语多\n"
	case "H-03":
		return "名字：小赵\n" +
			"职业：logistics、patrol、worker\n" +
			"背景：物流巡检员，负责物资流转与区域巡检\n" +
			"性格特质：活泼、勤快、话多、责任感强\n" +
			"说话风格：热情，爱闲聊\n"
	}
	return ""
}
