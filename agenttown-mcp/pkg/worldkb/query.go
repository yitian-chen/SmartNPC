package worldkb

import (
	"errors"
	"fmt"
	"math"
)

// GetZone returns the zone by ID, or nil if not found.
func (k *KB) GetZone(id string) *Zone {
	if k == nil {
		return nil
	}
	return k.zoneByID[id]
}

// GetObject returns the object by ID, or nil if not found.
func (k *KB) GetObject(id string) *Object {
	if k == nil {
		return nil
	}
	return k.objectByID[id]
}

// GetAgent returns the agent by ID, or nil if not found.
func (k *KB) GetAgent(id string) *Agent {
	if k == nil {
		return nil
	}
	return k.agentByID[id]
}

// GetPosition resolves a semantic ID to a world-space coordinate.
//
// For zones, returns the entry_point with kind="zone".
// For objects, returns the interaction_point with kind="object".
// Returns an error if the ID is unknown or is not a zone/object.
func (k *KB) GetPosition(id string) (coord [3]float64, kind string, err error) {
	if k == nil {
		return [3]float64{}, "", errors.New("world kb not loaded")
	}
	if z := k.zoneByID[id]; z != nil {
		return z.EntryPoint, "zone", nil
	}
	if o := k.objectByID[id]; o != nil {
		return o.InteractionPoint, "object", nil
	}
	return [3]float64{}, "", fmt.Errorf("unknown target %q: not a zone or object", id)
}

// WhichZone returns the zone ID whose AABB contains pos, or "" if none.
// Checks X/Y only (Z ignored — UE5 buildings are flat in the current phase).
func (k *KB) WhichZone(pos [3]float64) string {
	if k == nil {
		return ""
	}
	for _, z := range k.Zones {
		c := z.Bounds.Center
		h := z.Bounds.Extent
		if pos[0] >= c[0]-h[0] && pos[0] <= c[0]+h[0] &&
			pos[1] >= c[1]-h[1] && pos[1] <= c[1]+h[1] {
			return z.ID
		}
	}
	return ""
}

// WhichObject returns the object ID whose interaction radius (centered on
// ActorPosition) contains pos, or "" if none. Uses Euclidean distance on
// X/Y (Z ignored). Replaces the former WhichLocation.
func (k *KB) WhichObject(pos [3]float64) string {
	if k == nil {
		return ""
	}
	for _, o := range k.Objects {
		if o.InteractionRadius <= 0 {
			continue
		}
		dx := pos[0] - o.ActorPosition[0]
		dy := pos[1] - o.ActorPosition[1]
		if math.Sqrt(dx*dx+dy*dy) <= o.InteractionRadius {
			return o.ID
		}
	}
	return ""
}

// GetAvailableInteractions returns the interactions allowed at an object ID.
// Returns nil if the object is unknown or has no interactions defined.
func (k *KB) GetAvailableInteractions(objectID string) []string {
	if k == nil {
		return nil
	}
	if o := k.objectByID[objectID]; o != nil {
		return o.AvailableInteractions
	}
	return nil
}

// ResolveTarget resolves a semantic descriptor to an ID and kind.
//
// Exact match against zone/object/agent IDs. For "workbench_01"
// this returns ("workbench_01", "object", nil). Returns an error if no
// exact match is found. Fuzzy matching is left to a future extension.
func (k *KB) ResolveTarget(desc string) (id string, kind string, err error) {
	if k == nil {
		return "", "", errors.New("world kb not loaded")
	}
	if desc == "" {
		return "", "", errors.New("empty target descriptor")
	}
	if k.zoneByID[desc] != nil {
		return desc, "zone", nil
	}
	if k.objectByID[desc] != nil {
		return desc, "object", nil
	}
	if k.agentByID[desc] != nil {
		return desc, "agent", nil
	}
	return "", "", fmt.Errorf("no entity matches %q", desc)
}

// ZoneInfo is a compact zone summary for prompt injection.
type ZoneInfo struct {
	ID          string
	DisplayName string
}

// ObjectInfo is a compact object summary for prompt injection. Includes
// ZoneID so consumers can express zone-object relationships in prompts.
// Category lets consumers pick the right tool for the object type
// (e.g. workbench→work_at_workbench, charging_station→charge_at_station).
// SemanticGroup is the UE5-facing group name (e.g. "charger", "workbench")
// used as the `semantic_group` parameter value for InteractSmartObject and
// composite actions — distinct from the instance ID (e.g. "Charge-1").
type ObjectInfo struct {
	ID                    string
	DisplayName           string
	Category              string
	SemanticGroup         string
	ZoneID                string
	AvailableInteractions []string
}

// ListZones returns all zones in declaration order. Returns nil if the KB
// is nil or empty. Used by the tactical layer to inject the full zone list
// into the LLM prompt so the agent knows which zones exist.
func (k *KB) ListZones() []ZoneInfo {
	if k == nil {
		return nil
	}
	out := make([]ZoneInfo, 0, len(k.Zones))
	for _, z := range k.Zones {
		out = append(out, ZoneInfo{ID: z.ID, DisplayName: z.DisplayName})
	}
	return out
}

// ListObjects returns all smart objects in declaration order. Returns nil if
// the KB is nil or empty. Used by the tactical layer prompt to inject the
// full object list (with zone_id and available interactions) so the LLM
// cannot invent object IDs like "workbench_02" that don't exist in the KB.
func (k *KB) ListObjects() []ObjectInfo {
	if k == nil {
		return nil
	}
	out := make([]ObjectInfo, 0, len(k.Objects))
	for _, o := range k.Objects {
		interactions := make([]string, len(o.AvailableInteractions))
		copy(interactions, o.AvailableInteractions)
		out = append(out, ObjectInfo{
			ID:                    o.ID,
			DisplayName:           o.DisplayName,
			Category:              o.Category,
			SemanticGroup:         o.SemanticGroup,
			ZoneID:                o.ZoneID,
			AvailableInteractions: interactions,
		})
	}
	return out
}
