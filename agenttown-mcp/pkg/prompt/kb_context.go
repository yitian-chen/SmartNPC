// Package prompt — KB context segment builder.
//
// KBContext lists zones/objects from the world KB for injection into tactical
// and strategic prompts, letting the LLM see legal IDs and avoid fabricating
// non-existent zones/objects.
package prompt

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// KBContext assembles the zone/object listing segment for prompt injection.
// Shared by strategic and tactical layers.
//
// Format design: each object gets its own line, clearly separating id /
// zone / available interactions, preventing the LLM from treating a
// "id|zone[interactions]" concatenation as a single target_object_id.
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
		lines = append(lines, "可前往区域（move_to_location 的 target 用 id）: "+strings.Join(parts, "、")+"。")
	}
	if os := kb.ListObjects(); len(os) > 0 {
		lines = append(lines, "可交互物体（interact/work_at_workbench 的 target_object_id 用 id，interact 的 interaction 用下列可用动词）:")
		for _, o := range os {
			label := o.ID
			if o.DisplayName != "" && o.DisplayName != o.ID {
				label = fmt.Sprintf("%s（id=%s）", o.DisplayName, o.ID)
			}
			zoneInfo := ""
			if o.ZoneID != "" {
				zoneInfo = "，位于 zone=" + o.ZoneID
			}
			interactionInfo := ""
			if len(o.AvailableInteractions) > 0 {
				interactionInfo = "，可用 interaction: " + strings.Join(o.AvailableInteractions, "/")
			}
			lines = append(lines, "  - "+label+zoneInfo+interactionInfo)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// StrategicZoneObjectMap constructs the "which zone has which interactive
// objects" mapping view. Lets the strategic LLM see at a glance which zones
// support interact activities and which are empty.
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
		parts := make([]string, 0, len(objsInZone))
		for _, o := range objsInZone {
			olabel := o.ID
			if o.DisplayName != "" && o.DisplayName != o.ID {
				olabel = fmt.Sprintf("%s（id=%s）", o.DisplayName, o.ID)
			}
			parts = append(parts, olabel)
		}
		sb.WriteString("  - " + label + "：" + strings.Join(parts, "、") + "\n")
	}
	return sb.String()
}
