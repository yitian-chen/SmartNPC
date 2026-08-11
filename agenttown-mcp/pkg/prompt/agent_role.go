// Package prompt — shared agent role helper.
//
// AgentRole is used by all three decision layers (strategic/tactical/reactive)
// to inject the 【你的角色】 segment, ensuring consistent NPC persona across
// prompt constructions.
package prompt

import (
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// AgentRole constructs the 【你的角色】 segment by merging three sources,
// per field, with priority: profile (non-empty) > KB (non-empty) > hardcoded
// fallback (non-empty). Shared by all three decision layers.
//
// Any of kb / profiles may be nil. When the agent is missing from all three
// sources the returned string is empty.
func AgentRole(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	p, kbAgent, fbName, fbProf, fbDesc, fbTraits, fbSpeech := roleSources(kb, profiles, agentID)
	// p may be nil when agentID is absent from profiles; substitute an empty
	// Profile so field reads below are nil-safe.
	if p == nil {
		p = &profile.Profile{}
	}

	var sb strings.Builder
	if name := firstNonEmpty(p.DisplayName, kbName(kbAgent), fbName); name != "" {
		sb.WriteString("名字：" + name + "\n")
	}
	if prof := firstNonEmpty(p.Profession, kbProfession(kbAgent), fbProf); prof != "" {
		sb.WriteString("职业：" + prof + "\n")
	}
	if desc := firstNonEmpty(p.Description, kbDescription(kbAgent), fbDesc); desc != "" {
		sb.WriteString("背景：" + desc + "\n")
	}
	if traits := firstNonEmptyTraits(p.Traits, kbTraits(kbAgent), fbTraits); len(traits) > 0 {
		sb.WriteString("性格特质：" + strings.Join(traits, "、") + "\n")
	}
	if speech := firstNonEmpty(p.SpeechStyle, kbSpeechStyle(kbAgent), fbSpeech); speech != "" {
		sb.WriteString("说话风格：" + speech + "\n")
	}
	return sb.String()
}

// roleSources resolves the three persona sources for agentID:
//   - p: profile from profiles map (nil if missing)
//   - kbAgent: agent from KB (nil if kb nil or agent missing)
//   - fb*: hardcoded fallback fields (zero values if agentID unknown)
func roleSources(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) (p *profile.Profile, kbAgent *worldkb.Agent, fbName, fbProf, fbDesc string, fbTraits []string, fbSpeech string) {
	if profiles != nil {
		p = profiles[agentID]
	}
	if kb != nil {
		kbAgent = kb.GetAgent(agentID)
	}
	fbName, fbProf, fbDesc, fbTraits, fbSpeech = fallbackFields(agentID)
	return
}

// firstNonEmpty returns the first non-empty string among inputs, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstNonEmptyTraits returns the first non-empty slice among inputs.
func firstNonEmptyTraits(slices ...[]string) []string {
	for _, s := range slices {
		if len(s) > 0 {
			return s
		}
	}
	return nil
}

// kbName extracts DisplayName from a KB agent (nil-safe).
func kbName(a *worldkb.Agent) string {
	if a == nil {
		return ""
	}
	return a.DisplayName
}

func kbProfession(a *worldkb.Agent) string {
	if a == nil {
		return ""
	}
	return a.Profession
}

func kbDescription(a *worldkb.Agent) string {
	if a == nil {
		return ""
	}
	return a.Description
}

func kbTraits(a *worldkb.Agent) []string {
	if a == nil {
		return nil
	}
	return a.Personality.Traits
}

func kbSpeechStyle(a *worldkb.Agent) string {
	if a == nil {
		return ""
	}
	return a.Personality.SpeechStyle
}

// fallbackFields returns the hardcoded fallback persona fields for agentID.
// Covers H-01/H-02/H-03 (phase-1 three NPCs); other agentIDs return zero
// values. Format matches AgentRole output (name/profession/background/
// personality/speech style), omitting fields that are empty in the authored
// KB so fallback and KB-loaded输出 stay consistent.
func fallbackFields(agentID string) (name, profession, description string, traits []string, speechStyle string) {
	switch agentID {
	case "H-01":
		return "老陈",
			"supervisor、worker、maintainer",
			"",
			[]string{"沉稳", "念旧", "重视工艺", "务实"},
			""
	case "H-02":
		return "小林",
			"maintainer、technician",
			"维修技术员，专注精密装配与设备维护",
			[]string{"细致", "严谨", "专注技术", "话少"},
			"精确，技术术语多"
	case "H-03":
		return "小赵",
			"logistics、patrol、worker",
			"物流巡检员，负责物资流转与区域巡检",
			[]string{"活泼", "勤快", "话多", "责任感强"},
			"热情，爱闲聊"
	}
	return "", "", "", nil, ""
}

// fallbackAgentRole is retained for backward compatibility with callers
// that have not yet migrated to the profiles-aware AgentRole signature.
// It returns the same string as AgentRole(nil, nil, agentID).
func fallbackAgentRole(agentID string) string {
	return AgentRole(nil, nil, agentID)
}
