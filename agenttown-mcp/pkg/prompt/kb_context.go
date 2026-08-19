// Package prompt — KB context segment builder.
//
// KBContext lists zones/objects from the world KB for injection into tactical
// and strategic prompts, letting the LLM see legal IDs and avoid fabricating
// non-existent zones/objects.
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// KBContext assembles the zone/object listing segment for prompt injection.
// Shared by strategic and tactical layers.
//
// Format design: each object group gets its own line, keyed by semantic_group
// (the UE5-facing parameter value). Multi-instance groups (e.g. Charge-1...
// Charge-6 all share semantic_group=charger) collapse to one line so the LLM
// sees the valid semantic_group value without being confused by instance IDs.
func KBContext(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	var lines []string
	if zs := kb.ListZones(); len(zs) > 0 {
		parts := make([]string, 0, len(zs))
		for _, z := range zs {
			if z.DisplayName != "" && z.DisplayName != z.ID {
				parts = append(parts, fmt.Sprintf("%s（id=%s）", z.DisplayName, z.ID))
			} else {
				parts = append(parts, z.ID)
			}
		}
		lines = append(lines, "可前往区域（move_to 的 target_id 用 id）: "+strings.Join(parts, "、")+"。")
	}
	if os := kb.ListObjects(); len(os) > 0 {
		lines = append(lines, "可交互物体（InteractSmartObject 和复合动作的 semantic_group 用下列 semantic_group 值，InteractSmartObject 的 interaction 用下列可用动词；复合动作会自动移动到对应位置，无需自己调用 move_to）:")
		for _, g := range groupObjectsBySemantic(os) {
			lines = append(lines, formatObjectGroup(g))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// objectGroup is a collapsed view of all smart object instances sharing one
// semantic_group. Used by KBContext to avoid listing every Charge-N instance.
type objectGroup struct {
	SemanticGroup         string
	DisplayName           string
	Description           string
	ZoneID                string
	AvailableInteractions []string
	InstanceCount         int
}

// groupObjectsBySemantic collapses a list of objects into one entry per
// semantic_group. Objects with an empty SemanticGroup fall back to grouping
// by ID (each its own group), preserving the legacy per-instance display.
// Available interactions are unioned across instances; zone is taken from the
// first instance (instances of one group share a zone in current UE5 maps).
// Output is sorted by SemanticGroup for deterministic prompt output.
func groupObjectsBySemantic(objs []worldkb.ObjectInfo) []objectGroup {
	byKey := make(map[string]*objectGroup, len(objs))
	order := make([]string, 0, len(objs))
	for _, o := range objs {
		key := o.SemanticGroup
		if key == "" {
			key = o.ID
		}
		if g, ok := byKey[key]; ok {
			g.InstanceCount++
			for _, act := range o.AvailableInteractions {
				if !contains(g.AvailableInteractions, act) {
					g.AvailableInteractions = append(g.AvailableInteractions, act)
				}
			}
			continue
		}
		g := &objectGroup{
			SemanticGroup:         key,
			DisplayName:           o.DisplayName,
			Description:           o.Description,
			ZoneID:                o.ZoneID,
			AvailableInteractions: append([]string(nil), o.AvailableInteractions...),
			InstanceCount:         1,
		}
		byKey[key] = g
		order = append(order, key)
	}
	out := make([]objectGroup, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SemanticGroup < out[j].SemanticGroup })
	return out
}

// formatObjectGroup renders one object group line for the prompt.
// Trailing description (authored one-liner like "坐一坐，恢复少量疲劳")
// helps the LLM understand what the object is for beyond bare verb names.
func formatObjectGroup(g objectGroup) string {
	label := g.SemanticGroup
	if g.DisplayName != "" {
		label = fmt.Sprintf("%s（semantic_group=%s）", g.DisplayName, g.SemanticGroup)
	} else {
		label = fmt.Sprintf("semantic_group=%s", g.SemanticGroup)
	}
	zoneInfo := ""
	if g.ZoneID != "" {
		zoneInfo = "，位于 zone=" + g.ZoneID
	}
	interactionInfo := ""
	if len(g.AvailableInteractions) > 0 {
		interactionInfo = "，可用 interaction: " + strings.Join(g.AvailableInteractions, "/")
	}
	countInfo := ""
	if g.InstanceCount > 1 {
		countInfo = fmt.Sprintf("，%d 个实例", g.InstanceCount)
	}
	descInfo := ""
	if d := strings.TrimRight(strings.TrimSpace(g.Description), "。"); d != "" {
		descInfo = "，" + d
	}
	return "  - " + label + zoneInfo + interactionInfo + countInfo + descInfo
}

// contains reports whether s contains v. Tiny helper to avoid importing
// slices (keep the package dependency surface minimal).
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// StrategicZoneObjectMap constructs the "which zone has which interactive
// objects" mapping view. Lets the strategic LLM see at a glance which zones
// support interact activities and which are empty. Objects are collapsed by
// semantic_group so multi-instance groups (e.g. 6 charging piles) appear
// once per zone.
//
// KB empty or no zones → returns empty string (graceful degradation).
func StrategicZoneObjectMap(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	zones := kb.ListZones()
	if len(zones) == 0 {
		return ""
	}
	objs := kb.ListObjects()
	byZone := make(map[string][]worldkb.ObjectInfo, len(zones))
	for _, o := range objs {
		if o.ZoneID == "" {
			continue
		}
		byZone[o.ZoneID] = append(byZone[o.ZoneID], o)
	}
	var sb strings.Builder
	for _, z := range zones {
		label := z.ID
		if z.DisplayName != "" && z.DisplayName != z.ID {
			label = fmt.Sprintf("%s（id=%s）", z.DisplayName, z.ID)
		}
		objsInZone := byZone[z.ID]
		if len(objsInZone) == 0 {
			sb.WriteString("  - " + label + "：无可交互物体\n")
			continue
		}
		groups := groupObjectsBySemantic(objsInZone)
		parts := make([]string, 0, len(groups))
		for _, g := range groups {
			olabel := g.SemanticGroup
			if g.DisplayName != "" {
				olabel = fmt.Sprintf("%s（semantic_group=%s）", g.DisplayName, g.SemanticGroup)
			}
			parts = append(parts, olabel)
		}
		sb.WriteString("  - " + label + "：" + strings.Join(parts, "、") + "\n")
	}
	return sb.String()
}
