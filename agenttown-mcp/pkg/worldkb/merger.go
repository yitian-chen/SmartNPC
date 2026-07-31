package worldkb

// merger.go implements the deep-merge pipeline that combines
// world.generated.json (UE-exported facts) with world.authored.json
// (human narrative) into a single MergedKB, per
// docs/AgentTown_WorldKB_Design.md §8.
//
// Merge rules (§8.2):
//   - Entity existence is keyed by generated (zone/object/agent must exist
//     in generated before authored overlay applies).
//   - Authored same-name scalar fields override generated.
//   - Dict fields: recursive deep merge.
//   - Array fields: authored fully replaces generated (no whitelist yet).
//   - connected_to: authored is authoritative; duplicates removed; targets
//     validated by the validator.
//   - Protected spatial fields (bounds/entry_point/actor_path/...) come only
//     from generated — authored structurally cannot set them.
//   - Authored dangling IDs (not in generated) are errors.
//   - Generated entities without authored narrative are warnings (not fatal).

import (
	"encoding/json"
	"fmt"
	"os"
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

	// Schema version sanity (§8.5).
	if gen.SchemaVersion != auth.SchemaVersion {
		return nil, nil, fmt.Errorf("schema version mismatch: generated=%q authored=%q",
			gen.SchemaVersion, auth.SchemaVersion)
	}

	var warnings []MergeWarning

	kb := &KB{
		Version: gen.SchemaVersion,
		Site: Site{
			ID:          auth.Site.ID,
			DisplayName: auth.Site.DisplayName,
			Description: auth.Site.Description,
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

		mz := Zone{
			ID:          gz.ID,
			Bounds:      Bounds{Center: center, Extent: extent},
			EntryPoint:  entry,
			EntryFacing: facing,
		}

		// Apply authored overlay if present.
		if az, ok := auth.Zones[gz.ID]; ok {
			mz.DisplayName = az.DisplayName
			mz.Description = az.Description
			mz.ConnectedTo = dedupStrings(az.ConnectedTo)
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "zone", EntityID: gz.ID,
				Message: "generated zone has no authored narrative (display_name/description/connected_to missing)",
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
			ID:                go_.ID,
			Category:          go_.Category,
			ZoneID:            go_.ZoneID,
			ActorClass:        go_.ActorClass,
			ActorPosition:     actorPos,
			InteractionPoint:  ip,
			InteractionFacing: ifacing,
			AvailableActions:  go_.AvailableActions,
			DefaultState:      go_.DefaultState,
			InteractionRadius: defaultInteractionRadius,
		}

		if ao, ok := auth.Objects[go_.ID]; ok {
			mo.DisplayName = ao.DisplayName
			mo.Description = ao.Description
			mo.RequiredRoles = ao.RequiredRoles
			mo.Capacity = ao.Capacity
			if ao.InteractionRadius > 0 {
				mo.InteractionRadius = ao.InteractionRadius
			}
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
			ma.Role = aa.Role
			ma.Personality = aa.Personality
			ma.HomeZone = aa.HomeZone
			ma.CoreMemories = aa.CoreMemories
		} else {
			warnings = append(warnings, MergeWarning{
				EntityType: "agent", EntityID: ga.ID,
				Message: "generated agent has no authored narrative (display_name/role/home_zone missing)",
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

	// ---- Relationships (fully from authored) ----
	for i, r := range auth.Relationships {
		if r.From == "" || r.To == "" {
			return nil, nil, fmt.Errorf("authored.relationships[%d]: missing from/to", i)
		}
		kb.Relationships = append(kb.Relationships, Relationship{
			From:        r.From,
			To:          r.To,
			Familiarity: r.Familiarity,
			Affection:   r.Affection,
			Type:        r.Type,
		})
	}

	// Deterministic ordering: sort all entity slices by ID.
	sort.SliceStable(kb.Zones, func(i, j int) bool { return kb.Zones[i].ID < kb.Zones[j].ID })
	sort.SliceStable(kb.Objects, func(i, j int) bool { return kb.Objects[i].ID < kb.Objects[j].ID })
	sort.SliceStable(kb.Agents, func(i, j int) bool { return kb.Agents[i].ID < kb.Agents[j].ID })

	return kb, warnings, nil
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

// MergeAndWrite is the one-shot pipeline used by the MCP --auto-merge-world-kb
// startup flag and by the worldkb-merge CLI. It loads the two source JSONs,
// merges them, validates the result, and atomically writes world_kb.yaml +
// manifest.json. Returns the merged KB so callers can use it directly
// without re-loading the file they just wrote.
//
// Path semantics:
//   - genPath / authPath: input JSON files (must exist).
//   - outPath: world_kb.yaml is written here (atomic temp-file + rename).
//   - manifestPath: if non-empty, manifest.json is written here.
//   - If manifestPath is empty, no manifest is written.
//
// Failures:
//   - I/O errors (missing files, permission denied) wrap the underlying error.
//   - Schema mismatch / dangling authored IDs / protected-field violations
//     return the merge error directly.
//   - Validation errors (after a successful merge) return the first Issue
//     with SeverityError wrapped in an error.
func MergeAndWrite(genPath, authPath, outPath, manifestPath string) (*KB, error) {
	gen, err := loadGeneratedDoc(genPath)
	if err != nil {
		return nil, fmt.Errorf("load generated: %w", err)
	}
	auth, err := loadAuthoredDoc(authPath)
	if err != nil {
		return nil, fmt.Errorf("load authored: %w", err)
	}

	kb, _, err := Merge(gen, auth)
	if err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}

	if issues := Validate(kb); issues.HasErrors() {
		// Surface the first error; subsequent errors are in issues.Errors
		// but a single wrapped error is enough for fail-fast startup.
		first := issues.Errors[0]
		return nil, fmt.Errorf("validate: [%s] %s: %s", first.Entity, first.Code, first.Message)
	}

	if _, err := WriteYAML(kb, outPath); err != nil {
		return nil, fmt.Errorf("write yaml: %w", err)
	}

	if manifestPath != "" {
		genBytes, err := os.ReadFile(genPath)
		if err != nil {
			return nil, fmt.Errorf("read generated for manifest: %w", err)
		}
		authBytes, err := os.ReadFile(authPath)
		if err != nil {
			return nil, fmt.Errorf("read authored for manifest: %w", err)
		}
		mergedBytes, err := os.ReadFile(outPath)
		if err != nil {
			return nil, fmt.Errorf("read merged for manifest: %w", err)
		}
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

// loadGeneratedDoc parses a world.generated.json file.
func loadGeneratedDoc(path string) (*GeneratedDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc GeneratedDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}

// loadAuthoredDoc parses a world.authored.json file.
func loadAuthoredDoc(path string) (*AuthoredDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc AuthoredDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &doc, nil
}
