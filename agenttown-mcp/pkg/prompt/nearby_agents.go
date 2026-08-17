package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
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
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米，当前：%s\n", a.Name, a.ID, a.Distance, a.CurrentAction)
		} else {
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米\n", a.Name, a.ID, a.Distance)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}
