// Package prompt — object status segment builder.
//
// ObjectStatusContext injects real-time smart object availability into the
// tactical prompt, letting the LLM avoid planning actions that target
// occupied objects (the "workbench deadlock" bug where multiple NPCs
// simultaneously target a single-instance workbench).
//
// Data sources (both from perception_update, captured in PerceptionPayload):
//   - ObjectStatusSummary: per-category aggregate across ALL zones
//     (e.g. {"work": {total:2, idle:1, occupied:1}} counts workbench +
//     sorting_conveyor together regardless of which zone the NPC is in)
//   - NearbyObjects: per-instance state for the NPC's current zone only
//     (e.g. WorkBench state=occupied), used to disambiguate which
//     semantic_group within a multi-group category is occupied
//
// KB provides the category → semantic_group mapping (one category may
// contain multiple semantic_groups; e.g. "work" contains both "workbench"
// and "sorting_conveyor"). The prompt lists both views so the LLM can
// combine them: if "work" shows 1 idle / 1 occupied and WorkBench is
// occupied, then sorting_conveyor must be the idle one.
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// ObjectStatusContext constructs the 【物体实时占用】 segment for the
// tactical prompt. Returns "" when status is empty/nil or KB is unavailable,
// causing BuildTactical to skip the segment entirely (graceful degradation
// for cold start or mock UE without object_status_summary).
//
// Output format (one line per category, sorted alphabetically; nearby
// instances sorted by id):
//
//	【物体实时占用】
//	按 category 聚合（跨区域）：
//	- charging（含 charger）：6 个实例，6 空闲 / 0 占用
//	- work（含 workbench, sorting_conveyor）：2 个实例，1 空闲 / 1 占用
//	你附近的实例状态（当前 zone）：
//	- WorkBench（semantic_group=workbench）：占用中
func ObjectStatusContext(
	status map[string]protocol.ObjectCategoryStatus,
	nearby []protocol.NearbyObject,
	kb *worldkb.KB,
) string {
	if kb == nil || len(status) == 0 {
		return ""
	}
	// Build category → []semantic_group mapping from KB.
	catToSG := buildCategoryToSemanticGroups(kb)

	var lines []string
	lines = append(lines, "【物体实时占用】")
	lines = append(lines, "按 category 聚合（跨区域）：")

	// Sort categories for deterministic prompt output.
	cats := make([]string, 0, len(status))
	for c := range status {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	for _, cat := range cats {
		s := status[cat]
		sgs := catToSG[cat]
		line := fmt.Sprintf("- %s", cat)
		if len(sgs) > 0 {
			line += "（含 " + strings.Join(sgs, ", ") + "）"
		}
		line += fmt.Sprintf("：%d 个实例，%d 空闲 / %d 占用", s.Total, s.Idle, s.Occupied)
		if s.Broken > 0 {
			line += fmt.Sprintf(" / %d 故障", s.Broken)
		}
		lines = append(lines, line)
	}

	// Per-instance nearby objects (current zone only). Disambiguates which
	// semantic_group within a multi-group category is occupied.
	if len(nearby) > 0 {
		lines = append(lines, "你附近的实例状态（当前 zone）：")
		sorted := make([]protocol.NearbyObject, len(nearby))
		copy(sorted, nearby)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		for _, nb := range sorted {
			sg := lookupSemanticGroup(kb, nb.ID)
			label := nb.ID
			if sg != "" {
				label = fmt.Sprintf("%s（semantic_group=%s）", nb.ID, sg)
			}
			lines = append(lines, fmt.Sprintf("- %s：%s", label, translateObjectState(nb.State)))
		}
	}

	return strings.Join(lines, "\n") + "\n"
}

// translateObjectState maps UE5's English object state strings to the
// Chinese labels used in the tactical prompt. Unknown values pass through
// as-is (defensive against UE5 adding new states).
func translateObjectState(state string) string {
	switch state {
	case "":
		return "未知"
	case "idle":
		return "空闲"
	case "occupied":
		return "占用中"
	case "broken":
		return "故障"
	default:
		return state
	}
}

// buildCategoryToSemanticGroups builds a map from UE5 category string to
// the list of MCP semantic_group values that belong to it, using the KB.
// A category with no matching KB objects is absent from the map (the
// prompt line then shows the category without a "含 ..." clause).
func buildCategoryToSemanticGroups(kb *worldkb.KB) map[string][]string {
	out := make(map[string][]string)
	for _, o := range kb.ListObjects() {
		if o.Category == "" || o.SemanticGroup == "" {
			continue
		}
		sgs := out[o.Category]
		if !contains(sgs, o.SemanticGroup) {
			out[o.Category] = append(sgs, o.SemanticGroup)
		}
	}
	// Sort each slice for deterministic output.
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// lookupSemanticGroup finds the semantic_group for a given object ID by
// scanning the KB. Returns "" if the object is not found or has no
// semantic_group (legacy object).
func lookupSemanticGroup(kb *worldkb.KB, objectID string) string {
	if o := kb.GetObject(objectID); o != nil {
		return o.SemanticGroup
	}
	return ""
}
