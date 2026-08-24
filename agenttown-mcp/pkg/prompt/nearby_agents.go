package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// NearbyAgentsLine formats the visible-NPC list as 【附近NPC】 for the
// tactical prompt. Returns "" when empty (caller skips the segment). Each
// line shows display name, id, distance (meters), and current action —
// enough for the LLM to pick a social_chat target.
//
// Phase 2 Module C: this segment pairs with the social_chat composite
// tool so the LLM knows which NPC ids are valid target_agent_id values.
// Distance is rounded to whole meters for readability; current_action
// is omitted when empty (UE reports "" when the NPC is idle).
func NearbyAgentsLine(agents []protocol.VisibleAgent) string {
	if len(agents) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【附近NPC】\n")
	for _, a := range agents {
		if a.CurrentAction != "" {
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米，当前：%s\n", a.Name, a.ID, a.Distance/100, a.CurrentAction)
		} else {
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米\n", a.Name, a.ID, a.Distance/100)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// OtherAgentsLine formats the KB's static NPC roster (excluding self) as the
// 【其他NPC】 segment. Currently used only by the tactical layer (strategic
// layer temporarily has no social planning, so its roster segment is removed).
// Each line shows display name, id, and profession.
//
// Differs from NearbyAgentsLine: the tactical fallback uses the static KB
// roster (all NPCs are valid targets regardless of where they currently
// stand), while the primary tactical view uses runtime-visible agents (UE
// perception, includes distance/current action).
//
// Returns "" when kb is nil, has fewer than 2 agents, or self is the only
// agent — caller skips the segment in those cases.
func OtherAgentsLine(kb *worldkb.KB, selfID string) string {
	if kb == nil || len(kb.Agents) < 2 {
		return ""
	}
	var sb strings.Builder
	for _, a := range kb.Agents {
		if a.ID == selfID {
			continue
		}
		name := a.DisplayName
		if name == "" {
			name = a.ID
		}
		if a.Profession != "" {
			fmt.Fprintf(&sb, "- %s（id=%s）职业：%s\n", name, a.ID, a.Profession)
		} else {
			fmt.Fprintf(&sb, "- %s（id=%s）\n", name, a.ID)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
