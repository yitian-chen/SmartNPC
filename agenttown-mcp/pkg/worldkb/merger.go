package worldkb

// merger.go implements the deep-merge pipeline that combines the
// generated half (UE-exported spatial facts) with the authored half
// (human narrative) into a single KB. Inputs arrive as JSON bytes via
// the world_kb WebSocket message; see MergeAndWriteBytes for the
// runtime entry point.
//
// Merge rules (§8.2):
//   - Entity existence is keyed by generated (zone/object/agent must exist
//     in generated before authored overlay applies).
//   - Authored same-name scalar fields override generated.
//   - Dict fields: recursive deep merge.
//   - Array fields: authored fully replaces generated (no whitelist yet).
//   - Connections: authored is authoritative; targets validated by validator.
//   - Protected spatial fields (bounds/entry_point/actor_path/...) come only
//     from generated — authored structurally cannot set them.
//   - Authored dangling IDs (not in generated) are errors.
//   - Generated entities without authored narrative are warnings (not fatal).
//   - Per-agent relationships (NEW schema: authored.agents[id].relationships)
//     are flattened into kb.Relationships.

import (
	"encoding/json"
	"fmt"
	"sort"
)

// MergeError is a non-fatal warning (e.g. generated entity has no authored
// narrative). Fatal problems (dangling IDs, protected field violations) are
// returned as the error result.
type MergeWarning struct {
	EntityType string // "zone" | "object" | "agent"
	EntityID   string
	Message    string
}

// Merge combines a generated document with an authored overlay and returns
// the merged KB plus any non-fatal warnings. Fatal inconsistencies return
// an error.
func Merge(gen *GeneratedDoc, auth *AuthoredDoc) (*KB, []MergeWarning, error) {
	if gen == nil || auth == nil {
		return nil, nil, fmt.Errorf("generated and authored documents must both be non-nil")
	}

	// Schema version sanity: NEW schema uses auth.Version (string) vs gen.SchemaVersion.
	// Keep the check for forward compatibility — both default to "1.0".
	if gen.SchemaVersion != auth.Version {
		return nil, nil, fmt.Errorf("schema version mismatch: generated=%q authored=%q",
			gen.SchemaVersion, auth.Version)
	}

	var warnings []MergeWarning

	kb := &KB{
		Version: gen.SchemaVersion,
		Narrative: Narrative{
			Setting: auth.Narrative.Setting,
			Theme:   auth.Narrative.Theme,
		},
	}

	// ---- Zones ----
	zoneByID := make(map[string]int, len(gen.Zones))
	for i, gz := range gen.Zones {
		if gz.ID == "" {
			return nil, nil, fmt.Errorf("generated.zones[%d]: missing id", i)
		}
		if _, dup := zoneByID[gz.ID]; dup {
			return nil, nil, fmt.Errorf("generated.zones[%d]: duplicate id %q", i, gz.ID)
		}

		entry, err := toVec3(gz.EntryPoint, "entry_point", gz.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", gz.ID, err)
		}
		facing, err := toVec3(gz.EntryFacing, "entry_facing", gz.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", gz.ID, err)
		}
		center, err := toVec3(gz.Bounds.Center, "bounds.center", gz.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", gz.ID, err)
		}
		extent, err := toVec3(gz.Bounds.Extent, "bounds.extent", gz.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", gz.ID, err)
		}
		// Rotation is optional in NEW schema — default to zero if absent.
		rotation, err := toVec3Optional(gz.Bounds.Rotation, "bounds.rotation", gz.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated zone %q: %w", gz.ID, err)
		}

		mz := Zone{
			ID:          gz.ID,
			Bounds:      Bounds{Center: center, Extent: extent, Rotation: rotation},
			EntryPoint:  entry,
			EntryFacing: facing,
		}

		// Apply authored overlay if present.
		if az, ok := auth.Zones[gz.ID]; ok {
			mz.DisplayName = az.DisplayName
			mz.Description = az.Description
			mz.Aliases = dedupStrings(az.Aliases)
			mz.Connections = convertAuthoredConnections(az.Connections)
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "zone", EntityID: gz.ID,
				Message: "generated zone has no authored narrative (display_name/description/connections missing)",
			})
		}

		kb.Zones = append(kb.Zones, mz)
		zoneByID[gz.ID] = len(kb.Zones) - 1
	}

	// Detect authored zones not present in generated (dangling IDs).
	for id := range auth.Zones {
		if _, ok := zoneByID[id]; !ok {
			return nil, nil, fmt.Errorf("authored.zones[%q]: dangling id (not in generated)", id)
		}
	}

	// ---- Objects ----
	objectByID := make(map[string]int, len(gen.Objects))
	for i, go_ := range gen.Objects {
		if go_.ID == "" {
			return nil, nil, fmt.Errorf("generated.objects[%d]: missing id", i)
		}
		if _, dup := objectByID[go_.ID]; dup {
			return nil, nil, fmt.Errorf("generated.objects[%d]: duplicate id %q", i, go_.ID)
		}

		actorPos, err := toVec3(go_.ActorPosition, "actor_position", go_.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated object %q: %w", go_.ID, err)
		}
		ip, err := toVec3(go_.InteractionPoint, "interaction_point", go_.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated object %q: %w", go_.ID, err)
		}
		ifacing, err := toVec3(go_.InteractionFacing, "interaction_facing", go_.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated object %q: %w", go_.ID, err)
		}

		mo := Object{
			ID:                    go_.ID,
			Category:              go_.Category,
			ZoneID:                go_.ZoneID,
			ActorClass:            go_.ActorClass,
			ActorPosition:         actorPos,
			InteractionPoint:      ip,
			InteractionFacing:     ifacing,
			AvailableInteractions: go_.AvailableInteractions,
			DefaultState:          go_.DefaultState,
			InteractionRadius:     defaultInteractionRadius,
		}

		if ao, ok := auth.Objects[go_.ID]; ok {
			mo.DisplayName = ao.DisplayName
			mo.Description = ao.Description
			mo.Tags = ao.Tags
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "object", EntityID: go_.ID,
				Message: "generated object has no authored narrative (display_name/description missing)",
			})
		}

		kb.Objects = append(kb.Objects, mo)
		objectByID[go_.ID] = len(kb.Objects) - 1
	}

	for id := range auth.Objects {
		if _, ok := objectByID[id]; !ok {
			return nil, nil, fmt.Errorf("authored.objects[%q]: dangling id (not in generated)", id)
		}
	}

	// ---- Agents ----
	agentByID := make(map[string]int, len(gen.Agents))
	for i, ga := range gen.Agents {
		if ga.ID == "" {
			return nil, nil, fmt.Errorf("generated.agents[%d]: missing id", i)
		}
		if _, dup := agentByID[ga.ID]; dup {
			return nil, nil, fmt.Errorf("generated.agents[%d]: duplicate id %q", i, ga.ID)
		}

		ip, err := toVec3(ga.InitialPosition, "initial_position", ga.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("generated agent %q: %w", ga.ID, err)
		}

		ma := Agent{
			ID:               ga.ID,
			Type:             ga.Type,
			InitialZone:      ga.InitialZone,
			InitialPosition:  ip,
			ActorClass:       ga.ActorClass,
			ActionTable:      ga.ActionTable,
			MainBehaviorTree: ga.MainBehaviorTree,
		}

		if aa, ok := auth.Agents[ga.ID]; ok {
			ma.DisplayName = aa.DisplayName
			ma.Description = aa.Description
			ma.Profession = aa.Profession
			ma.Personality = Personality{
				Traits:      aa.Personality.Traits,
				SpeechStyle: aa.Personality.SpeechStyle,
			}
			// NEW schema: authored.agents[id].initial_zone is allowed to override
			// the generated initial_zone if explicitly set (acts as a narrative
			// refinement). Protected-field enforcement stays on spatial coords.
			if aa.InitialZone != "" {
				ma.InitialZone = aa.InitialZone
			}
			// Flatten per-agent relationships into kb.Relationships.
			for i, r := range aa.Relationships {
				if r.From == "" || r.To == "" {
					return nil, nil, fmt.Errorf("authored.agents[%q].relationships[%d]: missing from/to", ga.ID, i)
				}
				kb.Relationships = append(kb.Relationships, Relationship{
					From:        r.From,
					To:          r.To,
					Familiarity: r.Familiarity,
					Affection:   r.Affection,
					Type:        r.Type,
				})
			}
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "agent", EntityID: ga.ID,
				Message: "generated agent has no authored narrative (display_name/profession/personality missing)",
			})
		}

		kb.Agents = append(kb.Agents, ma)
		agentByID[ga.ID] = len(kb.Agents) - 1
	}

	for id := range auth.Agents {
		if _, ok := agentByID[id]; !ok {
			return nil, nil, fmt.Errorf("authored.agents[%q]: dangling id (not in generated)", id)
		}
	}

	// Deterministic ordering: sort all entity slices by ID.
	sort.SliceStable(kb.Zones, func(i, j int) bool { return kb.Zones[i].ID < kb.Zones[j].ID })
	sort.SliceStable(kb.Objects, func(i, j int) bool { return kb.Objects[i].ID < kb.Objects[j].ID })
	sort.SliceStable(kb.Agents, func(i, j int) bool { return kb.Agents[i].ID < kb.Agents[j].ID })

	// Build lookup indexes after sorting so map pointers are stable.
	kb.buildIndex()

	return kb, warnings, nil
}

// convertAuthoredConnections converts NEW schema's structured connections
// (with type/bidirectional) into the KB Connection type.
func convertAuthoredConnections(conns []AuthoredConnection) []Connection {
	if len(conns) == 0 {
		return nil
	}
	out := make([]Connection, 0, len(conns))
	for _, c := range conns {
		out = append(out, Connection{
			To:            c.To,
			Type:          c.Type,
			Bidirectional: c.Bidirectional,
		})
	}
	return out
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

// MergeAndWriteBytes accepts the generated and authored documents as raw JSON
// bytes (typically received from a UE-pushed world_kb WebSocket message),
// merges, validates, atomically writes world_kb.yaml, optionally writes a
// manifest, and returns the merged KB ready for in-memory swap.
// It merges, validates, atomically writes world_kb.yaml, optionally writes
// a manifest, and returns the merged KB ready for in-memory swap.
//
// outPath: world_kb.yaml target (atomic temp-file + rename).
// manifestPath: if non-empty, manifest.json is written here. The manifest's
// SHA256 hashes are computed from genBytes/authBytes directly (no disk re-read).
func MergeAndWriteBytes(genBytes, authBytes []byte, outPath, manifestPath string) (*KB, error) {
	var gen GeneratedDoc
	if err := json.Unmarshal(genBytes, &gen); err != nil {
		return nil, fmt.Errorf("parse generated: %w", err)
	}
	var auth AuthoredDoc
	if err := json.Unmarshal(authBytes, &auth); err != nil {
		return nil, fmt.Errorf("parse authored: %w", err)
	}

	kb, _, err := Merge(&gen, &auth)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}

	if issues := Validate(kb); issues.HasErrors() {
		first := issues.Errors[0]
		return nil, fmt.Errorf("validate: [%s] %s: %s", first.Entity, first.Code, first.Message)
	}

	mergedBytes, err := WriteYAML(kb, outPath)
	if err != nil {
		return nil, fmt.Errorf("write yaml: %w", err)
	}

	if manifestPath != "" {
		sourceMap := ""
		if gen.Source.MapPackage != "" {
			sourceMap = gen.Source.MapPackage
		}
		if err := WriteManifest(genBytes, authBytes, mergedBytes, manifestPath, sourceMap); err != nil {
			return nil, fmt.Errorf("write manifest: %w", err)
		}
	}

	return kb, nil
}
