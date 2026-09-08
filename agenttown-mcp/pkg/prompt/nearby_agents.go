package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
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
//
// kb is used to fall back a display name when UE reports an empty
// visible_agents[].name (observed in practice); a.Name empty → KB
// DisplayName → a.ID.
func NearbyAgentsLine(agents []protocol.VisibleAgent, kb *worldkb.KB) string {
	if len(agents) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("【附近NPC】\n")
	for _, a := range agents {
		name := a.Name
		if name == "" && kb != nil {
			// 遍历 KB 花名册（不依赖未导出的 agentByID 索引，struct 字面量构造的
			// KB 也可用）。agent 数量极少（≤5），线性查找开销可忽略。
			for _, ag := range kb.Agents {
				if ag.ID == a.ID {
					name = ag.DisplayName
					break
				}
			}
		}
		if name == "" {
			name = a.ID
		}
		if a.CurrentAction != "" {
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米，当前：%s\n", name, a.ID, a.Distance/100, a.CurrentAction)
		} else {
			fmt.Fprintf(&sb, "- %s（id=%s）距离 %.0f 米\n", name, a.ID, a.Distance/100)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// OtherAgentsLine formats the KB's static NPC roster (excluding self) as the
// 【其他NPC】 segment, used by both the strategic layer (social_chat target
// roster at 07:00 planning) and the tactical layer (fallback when no runtime
// visible agents). Each line shows display name, id, and profession.
//
// Differs from NearbyAgentsLine: the tactical fallback and strategic layer use
// the static KB roster (all NPCs are valid targets regardless of where they
// currently stand), while the primary tactical view uses runtime-visible
// agents (UE perception, includes distance/current action).
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
