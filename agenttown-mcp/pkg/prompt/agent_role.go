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

// AgentRole constructs the 【你的角色】 segment by merging two sources,
// per field, with priority: profile (non-empty) > hardcoded fallback
// (non-empty). KB agent persona fields are intentionally ignored — persona
// is authored solely in assets/profiles/<agentID>.md. Shared by all three
// decision layers.
//
// The kb parameter is retained in the signature to avoid churn at call sites
// (which still pass kb for KBContext etc.) but is not read here.
//
// Any of kb / profiles may be nil. When the agent is missing from both
// profile and fallback the returned string is empty.
func AgentRole(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	_ = kb // KB persona fields intentionally ignored; persona comes from profile only.
	p, fbName, fbProf, fbDesc, fbTraits, fbSpeech := roleSources(profiles, agentID)
	// p may be nil when agentID is absent from profiles; substitute an empty
	// Profile so field reads below are nil-safe.
	if p == nil {
		p = &profile.Profile{}
	}

	var sb strings.Builder
	if name := firstNonEmpty(p.DisplayName, fbName); name != "" {
		sb.WriteString("名字：" + name + "\n")
	}
	if prof := firstNonEmpty(p.Profession, fbProf); prof != "" {
		sb.WriteString("职业：" + prof + "\n")
	}
	if desc := firstNonEmpty(p.Description, fbDesc); desc != "" {
		sb.WriteString("背景：" + desc + "\n")
	}
	if traits := firstNonEmptyTraits(p.Traits, fbTraits); len(traits) > 0 {
		sb.WriteString("性格特质：" + strings.Join(traits, "、") + "\n")
	}
	if speech := firstNonEmpty(p.SpeechStyle, fbSpeech); speech != "" {
		sb.WriteString("说话风格：" + speech + "\n")
	}
	return sb.String()
}

// roleSources resolves the persona sources for agentID:
//   - p: profile from profiles map (nil if missing)
//   - fb*: hardcoded fallback fields (zero values if agentID unknown)
func roleSources(profiles map[string]*profile.Profile, agentID string) (p *profile.Profile, fbName, fbProf, fbDesc string, fbTraits []string, fbSpeech string) {
	if profiles != nil {
		p = profiles[agentID]
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

// fallbackFields returns the hardcoded fallback persona fields for agentID.
// Covers H-01/H-02/H-03 (phase-1 three NPCs); other agentIDs return zero
// values. Format matches AgentRole output (name/profession/background/
// personality/speech style), omitting fields that are empty in the authored
// profile so fallback and profile-loaded 输出 stay consistent.
func fallbackFields(agentID string) (name, profession, description string, traits []string, speechStyle string) {
	switch agentID {
	case "H-01":
		return "老陈",
			"装配工人（专做工作台装配作业）",
			"资深装配工人，常驻主生产车间工作台，只做装配（assemble）",
			[]string{"沉稳", "念旧", "耐久省电", "磨损慢"},
			"简短有力，多用行业术语"
	case "H-02":
		return "老王",
			"物流分拣员（专做分拣传送带分拣作业）",
			"物流分拣员，常驻物流转运站分拣传送带，只做分拣（sort_cargo）",
			[]string{"沉稳", "懒散", "耗电慢", "疲劳上涨快"},
			"慵懒闲适，常常打哈欠"
	case "H-03":
		return "老李",
			"精密装配技术员（专做工作台装配作业）",
			"精密装配技术员，常驻主生产车间工作台，只做装配（assemble）",
			[]string{"细致", "严谨", "干劲足", "耗电快"},
			"技术术语多，话痨，干劲满满"
	case "H-04":
		return "老刘",
			"物流搬运工（专做分拣传送带分拣作业）",
			"物流搬运工，常驻物流转运站分拣传送带，只做分拣（sort_cargo）",
			[]string{"老实", "踏实", "力气大", "反应慢"},
			"话少，一两个字应答，慢半拍"
	case "H-05":
		return "老张",
			"质检员（专做质检台质检作业）",
			"质检员，常驻主生产车间质检台，只做质检（inspect）",
			[]string{"谨慎", "啰嗦", "责任心强", "神经质"},
			"唠叨，爱重复叮嘱，三句不离质量标准"
	}
	return "", "", "", nil, ""
}

// fallbackAgentRole is retained for backward compatibility with callers
// that have not yet migrated to the profiles-aware AgentRole signature.
// It returns the same string as AgentRole(nil, nil, agentID).
func fallbackAgentRole(agentID string) string {
	return AgentRole(nil, nil, agentID)
}
