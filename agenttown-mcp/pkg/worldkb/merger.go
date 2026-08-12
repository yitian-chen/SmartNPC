package worldkb

// merger.go implements a flexible key-value deep-merge pipeline that combines
// the generated half (UE-exported spatial facts) with the authored half
// (human narrative) into a single KB. Inputs arrive as map[string]any
// (from JSON unmarshal) via the world_kb WebSocket message; see
// MergeAndWriteBytes for the runtime entry point.
//
// Merge rules (§8.2):
//   - Entity existence is keyed by generated (zone/object/agent must exist
//     in generated before authored overlay applies).
//   - Authored same-name scalar/array fields override generated.
//   - Dict fields: recursive deep merge.
//   - Protected spatial fields (bounds/entry_point/actor_path/...) come only
//     from generated — authored is skipped for those keys.
//   - Authored dangling IDs (not in generated) are fatal errors.
//   - Generated entities without authored narrative are warnings (not fatal).
//   - Schema version mismatch is a warning (not fatal) — tolerates OLD
//     authored schema using schema_version vs NEW version.
//   - OLD-authored compat shim runs at merge time: connected_to→connections,
//     role→profession, personality[]→personality{traits}, home_zone→initial_zone,
//     site→narrative, top-level relationships[] flattened.
//   - Unknown fields (not modeled by typed structs) round-trip via Extra.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Protected fields — authored data must not override these (§8.3)
// ---------------------------------------------------------------------------

// protectedZoneFields are spatial/fact fields that only generated data may set.
var protectedZoneFields = []string{
	"bounds", "entry_point", "entry_facing", "actor_path",
}

var protectedObjectFields = []string{
	"actor_class", "actor_position", "interaction_point",
	"interaction_facing", "default_state",
}

// protectedAgentFields does NOT include initial_zone — authored is allowed to
// override the generated initial_zone as a narrative refinement (matches the
// pre-refactor AuthoredAgent.InitialZone behavior).
var protectedAgentFields = []string{
	"actor_class", "initial_position",
	"action_table", "main_behavior_tree",
}

// defaultInteractionRadius is applied when neither generated nor authored
// specifies one.
const defaultInteractionRadius = 1500.0

// MergeWarning is a non-fatal merge warning. Fatal problems (dangling IDs,
// malformed vectors) are returned as the error result.
type MergeWarning struct {
	EntityType string // "zone" | "object" | "agent" | "" (top-level)
	EntityID   string
	Code       string // e.g. "SCHEMA_VERSION_MISMATCH"; empty for narrative-missing
	Message    string
}

// ---------------------------------------------------------------------------
// Known field sets — keys consumed by typed projection (not stashed in Extra)
// ---------------------------------------------------------------------------

var knownZoneKeys = map[string]bool{
	"id": true, "display_name": true, "description": true,
	"aliases": true, "bounds": true, "entry_point": true,
	"entry_facing": true, "connections": true,
}

var knownObjectKeys = map[string]bool{
	"id": true, "display_name": true, "description": true,
	"category": true, "semantic_group": true, "zone_id": true,
	"actor_class": true, "actor_position": true,
	"interaction_point": true, "interaction_facing": true,
	"interaction_radius": true, "available_interactions": true,
	"default_state": true, "tags": true,
}

var knownAgentKeys = map[string]bool{
	"id": true, "display_name": true, "description": true,
	"type": true, "profession": true, "personality": true,
	"initial_zone": true, "initial_position": true,
	"actor_class": true, "action_table": true,
	"main_behavior_tree": true,
}

// ---------------------------------------------------------------------------
// Normalize entity IDs (protocol tolerance: lowercase zone/object ids)
// ---------------------------------------------------------------------------

// normalizeEntityIDsMaps lowercases zone/object ids and their references in
// the generated map, and the corresponding map keys + connection refs in the
// authored map. Agent ids are untouched (allow uppercase, e.g. "H-01").
// Returns a change list for logging.
func normalizeEntityIDsMaps(genMap, authMap map[string]any) []string {
	var changes []string

	// zone id lowercase
	if zones, ok := genMap["zones"].([]any); ok {
		for _, z := range zones {
			zm, ok := z.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := zm["id"].(string); ok {
				if lid := strings.ToLower(id); lid != id {
					changes = append(changes, fmt.Sprintf("zone: %s→%s", id, lid))
					zm["id"] = lid
				}
			}
		}
	}

	// object id + zone_id ref lowercase
	if objs, ok := genMap["objects"].([]any); ok {
		for _, o := range objs {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := om["id"].(string); ok {
				if lid := strings.ToLower(id); lid != id {
					changes = append(changes, fmt.Sprintf("object: %s→%s", id, lid))
					om["id"] = lid
				}
			}
			if zid, ok := om["zone_id"].(string); ok {
				om["zone_id"] = strings.ToLower(zid)
			}
		}
	}

	// agent.initial_zone ref lowercase (agent id untouched)
	if agents, ok := genMap["agents"].([]any); ok {
		for _, a := range agents {
			am, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if iz, ok := am["initial_zone"].(string); ok {
				am["initial_zone"] = strings.ToLower(iz)
			}
		}
	}

	// authored.zones map keys + connections.to refs lowercase
	if authZones, ok := authMap["zones"].(map[string]any); ok && len(authZones) > 0 {
		newZones := make(map[string]any, len(authZones))
		for k, v := range authZones {
			lk := strings.ToLower(k)
			if lk != k {
				changes = append(changes, fmt.Sprintf("authored.zone: %s→%s", k, lk))
			}
			if zm, ok := v.(map[string]any); ok {
				if conns, ok := zm["connections"].([]any); ok {
					for _, c := range conns {
						if cm, ok := c.(map[string]any); ok {
							if to, ok := cm["to"].(string); ok {
								cm["to"] = strings.ToLower(to)
							}
						}
					}
				}
			}
			newZones[lk] = v
		}
		authMap["zones"] = newZones
	}

	// authored.objects map keys lowercase
	if authObjs, ok := authMap["objects"].(map[string]any); ok && len(authObjs) > 0 {
		newObjs := make(map[string]any, len(authObjs))
		for k, v := range authObjs {
			lk := strings.ToLower(k)
			if lk != k {
				changes = append(changes, fmt.Sprintf("authored.object: %s→%s", k, lk))
			}
			newObjs[lk] = v
		}
		authMap["objects"] = newObjs
	}

	// authored.agents: keys untouched, but initial_zone ref lowercase
	if authAgents, ok := authMap["agents"].(map[string]any); ok {
		for id, v := range authAgents {
			am, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if iz, ok := am["initial_zone"].(string); ok {
				am["initial_zone"] = strings.ToLower(iz)
			}
			authAgents[id] = am
		}
	}

	return changes
}

// ---------------------------------------------------------------------------
// MergeMaps — core merge
// ---------------------------------------------------------------------------

// MergeMaps combines a generated map with an authored overlay and returns
// the merged KB plus any non-fatal warnings. Fatal inconsistencies return
// an error.
func MergeMaps(genMap, authMap map[string]any) (*KB, []MergeWarning, error) {
	if genMap == nil || authMap == nil {
		return nil, nil, fmt.Errorf("generated and authored maps must both be non-nil")
	}

	var warnings []MergeWarning
	kb := &KB{}

	// Schema version: warning-only (was fatal in the typed merger).
	genVer, _ := genMap["schema_version"].(string)
	authVer, _ := authMap["version"].(string)
	if authVer == "" {
		authVer, _ = authMap["schema_version"].(string) // OLD schema fallback
	}
	if genVer != "" && authVer != "" && genVer != authVer {
		warnings = append(warnings, MergeWarning{
			Code:    "SCHEMA_VERSION_MISMATCH",
			Message: fmt.Sprintf("generated schema_version=%q, authored version=%q — continuing merge", genVer, authVer),
		})
	}
	if genVer != "" {
		kb.Version = genVer
	} else if authVer != "" {
		kb.Version = authVer
	}

	// Narrative: NEW narrative{setting,theme}, OLD site string (shim),
	// or UE5 site object {id, display_name, description} (shim).
	if narr, ok := authMap["narrative"].(map[string]any); ok {
		if s, ok := narr["setting"].(string); ok {
			kb.Narrative.Setting = s
		}
		if th, ok := narr["theme"].(string); ok {
			kb.Narrative.Theme = th
		}
	} else if site, ok := authMap["site"].(string); ok {
		// OLD schema shim: site string → narrative.setting
		kb.Narrative.Setting = site
	} else if siteObj, ok := authMap["site"].(map[string]any); ok {
		// UE5 shim: site{display_name, description} → narrative{setting, theme}
		if dn, ok := siteObj["display_name"].(string); ok {
			kb.Narrative.Setting = dn
		}
		if desc, ok := siteObj["description"].(string); ok {
			kb.Narrative.Theme = desc
		}
	}

	// ---- Zones ----
	authZones, _ := authMap["zones"].(map[string]any)
	zoneByID := make(map[string]int)
	genZones, _ := genMap["zones"].([]any)
	for i, z := range genZones {
		zm, ok := z.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("generated.zones[%d]: not an object", i)
		}
		id, _ := zm["id"].(string)
		if id == "" {
			return nil, nil, fmt.Errorf("generated.zones[%d]: missing id", i)
		}
		if _, dup := zoneByID[id]; dup {
			return nil, nil, fmt.Errorf("generated.zones[%d]: duplicate id %q", i, id)
		}

		merged := cloneMap(zm)
		if az, ok := authZones[id]; ok {
			azm, ok := az.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("authored.zones[%q]: not an object", id)
			}
			applyAuthoredShimZone(azm)
			deepMergeMaps(merged, azm, protectedZoneFields)
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "zone", EntityID: id,
				Message: "generated zone has no authored narrative",
			})
		}

		zone, err := projectZone(merged)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", id, err)
		}
		kb.Zones = append(kb.Zones, zone)
		zoneByID[id] = len(kb.Zones) - 1
	}
	for id := range authZones {
		if _, ok := zoneByID[id]; !ok {
			return nil, nil, fmt.Errorf("authored.zones[%q]: dangling id (not in generated)", id)
		}
	}

	// ---- Objects ----
	// Authored key matches generated instance by exact ID OR group prefix:
	// authored "Charge" matches generated "Charge-1"..."Charge-6" (one
	// authored overlay applies to every instance in the group). This bridges
	// the authored KB (semantic group names as keys) with the generated KB
	// (concrete instance IDs with -<n> suffixes for multi-instance groups).
	authObjects, _ := authMap["objects"].(map[string]any)
	objectByID := make(map[string]int)
	authoredMatched := make(map[string]bool, len(authObjects))
	genObjects, _ := genMap["objects"].([]any)
	for i, o := range genObjects {
		om, ok := o.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("generated.objects[%d]: not an object", i)
		}
		id, _ := om["id"].(string)
		if id == "" {
			return nil, nil, fmt.Errorf("generated.objects[%d]: missing id", i)
		}
		if _, dup := objectByID[id]; dup {
			return nil, nil, fmt.Errorf("generated.objects[%d]: duplicate id %q", i, id)
		}

		merged := cloneMap(om)
		matchedAuthKey := matchAuthoredObject(authObjects, id)
		if matchedAuthKey != "" {
			aom, ok := authObjects[matchedAuthKey].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("authored.objects[%q]: not an object", matchedAuthKey)
			}
			deepMergeMaps(merged, aom, protectedObjectFields)
			authoredMatched[matchedAuthKey] = true
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "object", EntityID: id,
				Message: "generated object has no authored narrative",
			})
		}

		obj, err := projectObject(merged)
		if err != nil {
			return nil, nil, fmt.Errorf("generated object %q: %w", id, err)
		}
		kb.Objects = append(kb.Objects, obj)
		objectByID[id] = len(kb.Objects) - 1
	}
	for id := range authObjects {
		if !authoredMatched[id] {
			return nil, nil, fmt.Errorf("authored.objects[%q]: dangling id (not in generated)", id)
		}
	}

	// ---- Agents ----
	authAgents, _ := authMap["agents"].(map[string]any)
	agentByID := make(map[string]int)
	genAgents, _ := genMap["agents"].([]any)
	for i, a := range genAgents {
		am, ok := a.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("generated.agents[%d]: not an object", i)
		}
		id, _ := am["id"].(string)
		if id == "" {
			return nil, nil, fmt.Errorf("generated.agents[%d]: missing id", i)
		}
		if _, dup := agentByID[id]; dup {
			return nil, nil, fmt.Errorf("generated.agents[%d]: duplicate id %q", i, id)
		}

		merged := cloneMap(am)
		if aa, ok := authAgents[id]; ok {
			aam, ok := aa.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("authored.agents[%q]: not an object", id)
			}
			applyAuthoredShimAgent(aam)
			deepMergeMaps(merged, aam, protectedAgentFields)
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "agent", EntityID: id,
				Message: "generated agent has no authored narrative",
			})
		}

		agent, err := projectAgent(merged)
		if err != nil {
			return nil, nil, fmt.Errorf("generated agent %q: %w", id, err)
		}
		kb.Agents = append(kb.Agents, agent)
		agentByID[id] = len(kb.Agents) - 1
	}
	for id := range authAgents {
		if _, ok := agentByID[id]; !ok {
			return nil, nil, fmt.Errorf("authored.agents[%q]: dangling id (not in generated)", id)
		}
	}

	// ---- Relationships (NEW per-agent + OLD top-level) ----
	rels, err := flattenRelationships(authMap, authAgents, agentByID)
	if err != nil {
		return nil, nil, err
	}
	kb.Relationships = rels

	// Deterministic ordering: sort all entity slices by ID.
	sort.SliceStable(kb.Zones, func(i, j int) bool { return kb.Zones[i].ID < kb.Zones[j].ID })
	sort.SliceStable(kb.Objects, func(i, j int) bool { return kb.Objects[i].ID < kb.Objects[j].ID })
	sort.SliceStable(kb.Agents, func(i, j int) bool { return kb.Agents[i].ID < kb.Agents[j].ID })

	kb.buildIndex()
	return kb, warnings, nil
}

// ---------------------------------------------------------------------------
// Deep merge
// ---------------------------------------------------------------------------

// matchAuthoredObject finds the authored key that applies to a generated
// object ID. Exact match wins; otherwise the longest authored key K such that
// the generated ID equals K or starts with K+"-" (group prefix match, e.g.
// authored "charge" matches generated "charge-1"..."charge-6"). Returns ""
// when no authored overlay applies.
//
// All comparisons run against post-normalization lowercase IDs (caller
// normalizes via normalizeEntityIDsMaps before MergeMaps).
func matchAuthoredObject(authObjects map[string]any, generatedID string) string {
	if len(authObjects) == 0 || generatedID == "" {
		return ""
	}
	if _, ok := authObjects[generatedID]; ok {
		return generatedID
	}
	best := ""
	for authKey := range authObjects {
		if authKey == "" {
			continue
		}
		if generatedID == authKey {
			return authKey
		}
		if strings.HasPrefix(generatedID, authKey+"-") {
			// Prefer the longest matching prefix to avoid ambiguity.
			if len(authKey) > len(best) {
				best = authKey
			}
		}
	}
	return best
}

// deepMergeMaps overlays src onto dst (mutates dst). Authored wins for
// scalar/array keys; nested maps recurse. Keys in the protected list are
// skipped (authored cannot override).
func deepMergeMaps(dst, src map[string]any, protected []string) {
	var protectedSet map[string]bool
	if len(protected) > 0 {
		protectedSet = make(map[string]bool, len(protected))
		for _, p := range protected {
			protectedSet[p] = true
		}
	}
	for k, srcVal := range src {
		if protectedSet != nil && protectedSet[k] {
			continue
		}
		if dstVal, ok := dst[k]; ok {
			dstMap, dstIsMap := dstVal.(map[string]any)
			srcMap, srcIsMap := srcVal.(map[string]any)
			if dstIsMap && srcIsMap {
				deepMergeMaps(dstMap, srcMap, nil)
				continue
			}
		}
		dst[k] = srcVal
	}
}

// cloneMap returns a deep copy of m so callers can mutate without touching
// the original.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return cloneMap(val)
	case []any:
		out := make([]any, len(val))
		for i, e := range val {
			out[i] = cloneValue(e)
		}
		return out
	default:
		return v
	}
}

// ---------------------------------------------------------------------------
// OLD→NEW authored compat shim (mutates authored entity maps in-place)
// ---------------------------------------------------------------------------

// applyAuthoredShimZone converts OLD zone keys to NEW: connected_to []string
// → connections [{to: s}]. Consumed OLD keys are deleted so they don't leak
// into Extra.
func applyAuthoredShimZone(m map[string]any) {
	if ct, ok := m["connected_to"]; ok {
		if _, hasConn := m["connections"]; !hasConn {
			if tos, ok := toStringSlice(ct); ok && len(tos) > 0 {
				conns := make([]any, 0, len(tos))
				for _, to := range tos {
					conns = append(conns, map[string]any{"to": to})
				}
				m["connections"] = conns
			}
		}
		delete(m, "connected_to")
	}
}

// applyAuthoredShimAgent converts OLD agent keys to NEW:
//   - role []string → profession string (joined with "、")
//   - personality []string → personality {traits: [...]}
//   - home_zone → initial_zone
//
// Consumed OLD keys (role, home_zone) are deleted. personality is converted
// in-place (same key, new shape).
func applyAuthoredShimAgent(m map[string]any) {
	if role, ok := m["role"]; ok {
		if _, hasProf := m["profession"]; !hasProf {
			if roles, ok := toStringSlice(role); ok && len(roles) > 0 {
				m["profession"] = strings.Join(roles, "、")
			}
		}
		delete(m, "role")
	}
	if p, ok := m["personality"]; ok {
		if traits, ok := toStringSlice(p); ok {
			m["personality"] = map[string]any{"traits": traits}
		}
	}
	if hz, ok := m["home_zone"]; ok {
		if _, hasIZ := m["initial_zone"]; !hasIZ {
			if hzs, ok := hz.(string); ok {
				m["initial_zone"] = hzs
			}
		}
		delete(m, "home_zone")
	}
}

// ---------------------------------------------------------------------------
// Projection — map[string]any → typed struct (unknown keys → Extra)
// ---------------------------------------------------------------------------

func projectZone(m map[string]any) (Zone, error) {
	id, _ := m["id"].(string)
	z := Zone{ID: id}

	if v, ok := m["display_name"].(string); ok {
		z.DisplayName = v
	}
	if v, ok := m["description"].(string); ok {
		z.Description = v
	}
	if aliases, ok := m["aliases"]; ok {
		if s, ok := toStringSlice(aliases); ok {
			z.Aliases = dedupStrings(s)
		}
	}

	// Bounds (required).
	bm, ok := m["bounds"].(map[string]any)
	if !ok {
		return z, fmt.Errorf("missing or invalid bounds")
	}
	centerSlice, err := anyToFloat64Slice(bm["center"])
	if err != nil {
		return z, fmt.Errorf("bounds.center: %w", err)
	}
	center, err := toVec3(centerSlice, "bounds.center", id)
	if err != nil {
		return z, err
	}
	extentSlice, err := anyToFloat64Slice(bm["extent"])
	if err != nil {
		return z, fmt.Errorf("bounds.extent: %w", err)
	}
	extent, err := toVec3(extentSlice, "bounds.extent", id)
	if err != nil {
		return z, err
	}
	rotSlice, _ := anyToFloat64Slice(bm["rotation"])
	rotation, err := toVec3Optional(rotSlice, "bounds.rotation", id)
	if err != nil {
		return z, err
	}
	z.Bounds = Bounds{Center: center, Extent: extent, Rotation: rotation}

	// EntryPoint (required).
	epSlice, err := anyToFloat64Slice(m["entry_point"])
	if err != nil {
		return z, fmt.Errorf("entry_point: %w", err)
	}
	entry, err := toVec3(epSlice, "entry_point", id)
	if err != nil {
		return z, err
	}
	z.EntryPoint = entry

	// EntryFacing (required).
	efSlice, err := anyToFloat64Slice(m["entry_facing"])
	if err != nil {
		return z, fmt.Errorf("entry_facing: %w", err)
	}
	facing, err := toVec3(efSlice, "entry_facing", id)
	if err != nil {
		return z, err
	}
	z.EntryFacing = facing

	// Connections.
	if conns, ok := m["connections"].([]any); ok {
		z.Connections = projectConnections(conns)
	}

	z.Extra = extractExtra(m, knownZoneKeys)
	return z, nil
}

func projectObject(m map[string]any) (Object, error) {
	id, _ := m["id"].(string)
	o := Object{ID: id, InteractionRadius: defaultInteractionRadius}

	if v, ok := m["display_name"].(string); ok {
		o.DisplayName = v
	}
	if v, ok := m["description"].(string); ok {
		o.Description = v
	}
	if v, ok := m["category"].(string); ok {
		o.Category = v
	}
	if v, ok := m["semantic_group"].(string); ok {
		o.SemanticGroup = v
	}
	if v, ok := m["zone_id"].(string); ok {
		o.ZoneID = v
	}
	if v, ok := m["actor_class"].(string); ok {
		o.ActorClass = v
	}
	if v, ok := m["default_state"].(string); ok {
		o.DefaultState = v
	}
	if tags, ok := m["tags"]; ok {
		if s, ok := toStringSlice(tags); ok {
			o.Tags = s
		}
	}

	// Vectors.
	actorPosSlice, err := anyToFloat64Slice(m["actor_position"])
	if err != nil {
		return o, fmt.Errorf("actor_position: %w", err)
	}
	ap, err := toVec3(actorPosSlice, "actor_position", id)
	if err != nil {
		return o, err
	}
	o.ActorPosition = ap

	ipSlice, err := anyToFloat64Slice(m["interaction_point"])
	if err != nil {
		return o, fmt.Errorf("interaction_point: %w", err)
	}
	ip, err := toVec3(ipSlice, "interaction_point", id)
	if err != nil {
		return o, err
	}
	o.InteractionPoint = ip

	ifSlice, err := anyToFloat64Slice(m["interaction_facing"])
	if err != nil {
		return o, fmt.Errorf("interaction_facing: %w", err)
	}
	ifacing, err := toVec3(ifSlice, "interaction_facing", id)
	if err != nil {
		return o, err
	}
	o.InteractionFacing = ifacing

	if v, ok := m["interaction_radius"].(float64); ok && v > 0 {
		o.InteractionRadius = v
	}

	// AvailableInteractions: []string or [{name, description}].
	if raw, ok := m["available_interactions"]; ok {
		if arr, ok := raw.([]any); ok && len(arr) > 0 {
			if _, isObj := arr[0].(map[string]any); isObj {
				// Object-array form: extract names, stash raw in Extra.
				names := make([]string, 0, len(arr))
				for _, e := range arr {
					if obj, ok := e.(map[string]any); ok {
						if name, ok := obj["name"].(string); ok {
							names = append(names, name)
						}
					}
				}
				o.AvailableInteractions = names
				if o.Extra == nil {
					o.Extra = map[string]any{}
				}
				o.Extra["available_interactions"] = raw
			} else if s, ok := toStringSlice(raw); ok {
				o.AvailableInteractions = s
			}
		} else if s, ok := toStringSlice(raw); ok {
			// []string directly (e.g. test-built maps).
			o.AvailableInteractions = s
		}
	}

	o.Extra = mergeExtra(o.Extra, extractExtra(m, knownObjectKeys))
	return o, nil
}

func projectAgent(m map[string]any) (Agent, error) {
	id, _ := m["id"].(string)
	a := Agent{ID: id}

	if v, ok := m["display_name"].(string); ok {
		a.DisplayName = v
	}
	if v, ok := m["description"].(string); ok {
		a.Description = v
	}
	if v, ok := m["type"].(string); ok {
		a.Type = v
	}
	if v, ok := m["profession"].(string); ok {
		a.Profession = v
	}
	if v, ok := m["initial_zone"].(string); ok {
		a.InitialZone = v
	}
	if v, ok := m["actor_class"].(string); ok {
		a.ActorClass = v
	}
	if v, ok := m["action_table"].(string); ok {
		a.ActionTable = v
	}
	if v, ok := m["main_behavior_tree"].(string); ok {
		a.MainBehaviorTree = v
	}

	// Personality.
	if pm, ok := m["personality"].(map[string]any); ok {
		if traits, ok := toStringSlice(pm["traits"]); ok {
			a.Personality.Traits = traits
		}
		if v, ok := pm["speech_style"].(string); ok {
			a.Personality.SpeechStyle = v
		}
	}

	// InitialPosition.
	ipSlice, err := anyToFloat64Slice(m["initial_position"])
	if err != nil {
		return a, fmt.Errorf("initial_position: %w", err)
	}
	ip, err := toVec3(ipSlice, "initial_position", id)
	if err != nil {
		return a, err
	}
	a.InitialPosition = ip

	a.Extra = extractExtra(m, knownAgentKeys)
	return a, nil
}

func projectConnections(conns []any) []Connection {
	if len(conns) == 0 {
		return nil
	}
	out := make([]Connection, 0, len(conns))
	for _, c := range conns {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		conn := Connection{}
		if v, ok := cm["to"].(string); ok {
			conn.To = v
		}
		if v, ok := cm["type"].(string); ok {
			conn.Type = v
		}
		if v, ok := cm["bidirectional"].(bool); ok {
			conn.Bidirectional = v
		}
		out = append(out, conn)
	}
	return out
}

// ---------------------------------------------------------------------------
// Relationships
// ---------------------------------------------------------------------------

func flattenRelationships(authMap map[string]any, authAgents map[string]any, agentByID map[string]int) ([]Relationship, error) {
	var rels []Relationship
	seen := make(map[string]bool)

	// NEW: per-agent relationships.
	for id := range agentByID {
		am, ok := authAgents[id].(map[string]any)
		if !ok {
			continue
		}
		arr, ok := am["relationships"].([]any)
		if !ok {
			continue
		}
		for _, r := range arr {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			rel := relationshipFromMap(rm)
			if rel.From == "" || rel.To == "" {
				return nil, fmt.Errorf("authored.agents[%q].relationships: missing from/to", id)
			}
			key := rel.From + "->" + rel.To
			if !seen[key] {
				seen[key] = true
				rels = append(rels, rel)
			}
		}
	}

	// OLD: top-level relationships (lenient — skip invalid entries).
	if topRels, ok := authMap["relationships"].([]any); ok {
		for _, r := range topRels {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			rel := relationshipFromMap(rm)
			if rel.From == "" || rel.To == "" {
				continue
			}
			key := rel.From + "->" + rel.To
			if !seen[key] {
				seen[key] = true
				rels = append(rels, rel)
			}
		}
	}

	return rels, nil
}

func relationshipFromMap(m map[string]any) Relationship {
	r := Relationship{}
	if v, ok := m["from"].(string); ok {
		r.From = v
	}
	if v, ok := m["to"].(string); ok {
		r.To = v
	}
	if v, ok := m["familiarity"].(float64); ok {
		r.Familiarity = int(v)
	}
	if v, ok := m["affection"].(float64); ok {
		r.Affection = int(v)
	}
	if v, ok := m["type"].(string); ok {
		r.Type = v
	}
	return r
}

// ---------------------------------------------------------------------------
// Extra helpers
// ---------------------------------------------------------------------------

// extractExtra returns a map of all keys in m not in the known set. Returns
// nil if there are no unknown keys (keeps structs clean for the common case).
func extractExtra(m map[string]any, known map[string]bool) map[string]any {
	var extra map[string]any
	for k, v := range m {
		if known[k] {
			continue
		}
		if extra == nil {
			extra = map[string]any{}
		}
		extra[k] = v
	}
	return extra
}

// mergeExtra merges src into dst; allocates dst if nil.
func mergeExtra(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// anyToFloat64Slice converts a JSON-unmarshaled number array ([]any of float64)
// or a Go []float64 into a []float64. nil returns (nil, nil).
func anyToFloat64Slice(v any) ([]float64, error) {
	switch s := v.(type) {
	case []float64:
		return s, nil
	case []any:
		out := make([]float64, 0, len(s))
		for _, e := range s {
			f, ok := e.(float64)
			if !ok {
				return nil, fmt.Errorf("expected number, got %T", e)
			}
			out = append(out, f)
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("expected number array, got %T", v)
	}
}

// toStringSlice converts a JSON-unmarshaled string array ([]any of string)
// or a Go []string into a []string. nil returns (nil, true).
func toStringSlice(v any) ([]string, bool) {
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			str, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, str)
		}
		return out, true
	case nil:
		return nil, true
	default:
		return nil, false
	}
}

// dedupStrings returns a copy of s with duplicates removed, preserving order.
func dedupStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// MergeAndWriteBytes — runtime WS-push entry point
// ---------------------------------------------------------------------------

// MergeAndWriteBytes accepts the generated and authored documents as raw JSON
// bytes (typically received from a UE-pushed world_kb WebSocket message),
// merges, validates, atomically writes world_kb.yaml, optionally writes a
// manifest, and returns the merged KB ready for in-memory swap.
//
// outPath: world_kb.yaml target (atomic temp-file + rename).
// manifestPath: if non-empty, manifest.json is written here.
//
// 第二个返回值是 normalize 报告（被小写化的 entity id 列表），便于上层日志
// 记录协议容错事件。空切片表示无需规范化。
func MergeAndWriteBytes(genBytes, authBytes []byte, outPath, manifestPath string) (*KB, []string, error) {
	var genMap map[string]any
	if err := json.Unmarshal(genBytes, &genMap); err != nil {
		return nil, nil, fmt.Errorf("parse generated: %w", err)
	}
	// Empty authored degrades to {} — authored is an optional human overlay.
	if len(bytes.TrimSpace(authBytes)) == 0 {
		authBytes = []byte(`{}`)
	}
	var authMap map[string]any
	if err := json.Unmarshal(authBytes, &authMap); err != nil {
		return nil, nil, fmt.Errorf("parse authored: %w", err)
	}

	changes := normalizeEntityIDsMaps(genMap, authMap)

	kb, _, err := MergeMaps(genMap, authMap)
	if err != nil {
		return nil, nil, fmt.Errorf("merge: %w", err)
	}

	if issues := Validate(kb); issues.HasErrors() {
		first := issues.Errors[0]
		return nil, nil, fmt.Errorf("validate: [%s] %s: %s", first.Entity, first.Code, first.Message)
	}

	mergedBytes, err := WriteYAML(kb, outPath)
	if err != nil {
		return nil, nil, fmt.Errorf("write yaml: %w", err)
	}

	if manifestPath != "" {
		sourceMap := ""
		if sm, ok := genMap["source"].(map[string]any); ok {
			if mp, ok := sm["map_package"].(string); ok {
				sourceMap = mp
			}
		}
		if err := WriteManifest(genBytes, authBytes, mergedBytes, manifestPath, sourceMap); err != nil {
			return nil, nil, fmt.Errorf("write manifest: %w", err)
		}
	}

	return kb, changes, nil
}
