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

// GetLocation returns the location by ID, or nil if not found.
func (k *KB) GetLocation(id string) *Location {
	if k == nil {
		return nil
	}
	return k.locationByID[id]
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
// For locations, returns the interaction_point with kind="location".
// Returns an error if the ID is unknown or is not a zone/location.
func (k *KB) GetPosition(id string) (coord [3]float64, kind string, err error) {
	if k == nil {
		return [3]float64{}, "", errors.New("world kb not loaded")
	}
	if z := k.zoneByID[id]; z != nil {
		return z.EntryPoint, "zone", nil
	}
	if l := k.locationByID[id]; l != nil {
		return l.InteractionPoint, "location", nil
	}
	return [3]float64{}, "", fmt.Errorf("unknown target %q: not a zone or location", id)
}

// WhichZone returns the zone ID whose AABB contains pos, or "" if none.
// Checks X/Y only (Z ignored — UE5 buildings are flat in the current phase).
func (k *KB) WhichZone(pos [3]float64) string {
	if k == nil {
		return ""
	}
	for _, z := range k.Zones {
		c := z.UE5Bounds.Center
		h := z.UE5Bounds.HalfSize
		if pos[0] >= c[0]-h[0] && pos[0] <= c[0]+h[0] &&
			pos[1] >= c[1]-h[1] && pos[1] <= c[1]+h[1] {
			return z.ID
		}
	}
	return ""
}

// WhichLocation returns the location ID whose interaction radius contains
// pos, or "" if none. Uses Euclidean distance on X/Y (Z ignored).
func (k *KB) WhichLocation(pos [3]float64) string {
	if k == nil {
		return ""
	}
	for _, l := range k.Locations {
		if l.InteractionRadius <= 0 {
			continue
		}
		dx := pos[0] - l.Position[0]
		dy := pos[1] - l.Position[1]
		if math.Sqrt(dx*dx+dy*dy) <= l.InteractionRadius {
			return l.ID
		}
	}
	return ""
}

// GetAvailableActions returns the actions allowed at a location ID.
// Returns nil if the location is unknown or has no actions defined.
func (k *KB) GetAvailableActions(locationID string) []string {
	if k == nil {
		return nil
	}
	if l := k.locationByID[locationID]; l != nil {
		return l.AvailableActions
	}
	return nil
}

// ResolveTarget resolves a semantic descriptor to an ID and kind.
//
// Exact match against zone/location/object/agent IDs. For "workbench_01"
// this returns ("workbench_01", "location", nil). Returns an error if no
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
	if k.locationByID[desc] != nil {
		return desc, "location", nil
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
	ID   string
	Name string
}

// LocationInfo is a compact location summary for prompt injection.
type LocationInfo struct {
	ID   string
	Name string
	Zone string
}

// ListZones returns all zones in declaration order. Returns nil if the KB
// is nil or empty. Used by the perception formatter to inject the full
// zone list into the LLM prompt so the agent knows which zones exist.
func (k *KB) ListZones() []ZoneInfo {
	if k == nil {
		return nil
	}
	out := make([]ZoneInfo, 0, len(k.Zones))
	for _, z := range k.Zones {
		out = append(out, ZoneInfo{ID: z.ID, Name: z.Name})
	}
	return out
}

// ListLocations returns all locations in declaration order. Returns nil if
// the KB is nil or empty. Used by the perception formatter to inject the
// full location list (with parent zone) into the LLM prompt.
func (k *KB) ListLocations() []LocationInfo {
	if k == nil {
		return nil
	}
	out := make([]LocationInfo, 0, len(k.Locations))
	for _, l := range k.Locations {
		out = append(out, LocationInfo{ID: l.ID, Name: l.Name, Zone: l.Zone})
	}
	return out
}

// ObjectInfo is a compact object summary for prompt injection.
// Name is borrowed from the Location with the same ID (objects mirror
// locations in the current KB schema and have no Name field of their own).
type ObjectInfo struct {
	ID               string
	Name             string
	AvailableActions []string
}

// ListObjects returns all smart objects in declaration order. Returns nil if
// the KB is nil or empty. Used by the tactical layer prompt to inject the
// full object list (with available actions) so the LLM cannot invent object
// IDs like "workbench_02" that don't exist in the KB.
func (k *KB) ListObjects() []ObjectInfo {
	if k == nil {
		return nil
	}
	out := make([]ObjectInfo, 0, len(k.Objects))
	for _, o := range k.Objects {
		name := o.ID
		if loc := k.GetLocation(o.ID); loc != nil && loc.Name != "" {
			name = loc.Name
		}
		actions := make([]string, len(o.AvailableActions))
		copy(actions, o.AvailableActions)
		out = append(out, ObjectInfo{
			ID:               o.ID,
			Name:             name,
			AvailableActions: actions,
		})
	}
	return out
}
