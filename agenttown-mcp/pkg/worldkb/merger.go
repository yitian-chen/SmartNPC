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
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// normalizeEntityIDs 把 generated 中的 zone/object id 及其引用转为小写，
// 同步重建 authored 的 map key。agent id 允许大写（如 "H-01"），不动。
//
// UE 推送的世界 KB 偶尔出现大写开头的 object id（如 "Charge"/"WorkBench"），
// 与协议 §5.1 的 idRegex `^[a-z][a-z0-9_]{2,63}$` 冲突。MCP 作为协议容错
// 入口统一小写化，避免 validator 拒绝导致整个 world_kb merge 失败（详见
// stable 端 2026-08-05 日志：4 个 object id 全大写开头，merge 报
// INVALID_ID_FORMAT）。
//
// 返回被修改的 id 列表（如 ["object: Charge→charge"]），便于上层日志记录。
// 引用类字段（object.zone_id / agent.initial_zone / authored.connections.to）
// 的小写化是 id 变更的必要副作用，不单独记录。
func normalizeEntityIDs(gen *GeneratedDoc, auth *AuthoredDoc) []string {
	var changes []string

	// zone id 小写化。
	for i := range gen.Zones {
		if lid := strings.ToLower(gen.Zones[i].ID); lid != gen.Zones[i].ID {
			changes = append(changes, fmt.Sprintf("zone: %s→%s", gen.Zones[i].ID, lid))
			gen.Zones[i].ID = lid
		}
	}

	// object id + zone_id 引用小写化。
	for i := range gen.Objects {
		if lid := strings.ToLower(gen.Objects[i].ID); lid != gen.Objects[i].ID {
			changes = append(changes, fmt.Sprintf("object: %s→%s", gen.Objects[i].ID, lid))
			gen.Objects[i].ID = lid
		}
		gen.Objects[i].ZoneID = strings.ToLower(gen.Objects[i].ZoneID)
	}

	// agent.initial_zone 引用小写化（agent id 本身不动，允许大写）。
	for i := range gen.Agents {
		gen.Agents[i].InitialZone = strings.ToLower(gen.Agents[i].InitialZone)
	}

	if auth == nil {
		return changes
	}

	// authored.zones map key 小写化 + connections.to 引用同步。
	if len(auth.Zones) > 0 {
		newZones := make(map[string]AuthoredZone, len(auth.Zones))
		for k, v := range auth.Zones {
			lk := strings.ToLower(k)
			if lk != k {
				changes = append(changes, fmt.Sprintf("authored.zone: %s→%s", k, lk))
			}
			for i := range v.Connections {
				v.Connections[i].To = strings.ToLower(v.Connections[i].To)
			}
			newZones[lk] = v
		}
		auth.Zones = newZones
	}

	// authored.objects map key 小写化。
	if len(auth.Objects) > 0 {
		newObjects := make(map[string]AuthoredObject, len(auth.Objects))
		for k, v := range auth.Objects {
			lk := strings.ToLower(k)
			if lk != k {
				changes = append(changes, fmt.Sprintf("authored.object: %s→%s", k, lk))
			}
			newObjects[lk] = v
		}
		auth.Objects = newObjects
	}

	// authored.agents map key 不动（agent id 允许大写），
	// 但 agent.initial_zone 引用要小写化。
	for id, ag := range auth.Agents {
		ag.InitialZone = strings.ToLower(ag.InitialZone)
		auth.Agents[id] = ag
	}

	return changes
}

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
//
// 第二个返回值是 normalize 报告（被小写化的 entity id 列表），便于上层日志
// 记录协议容错事件。空切片表示无需规范化。
func MergeAndWriteBytes(genBytes, authBytes []byte, outPath, manifestPath string) (*KB, []string, error) {
	var gen GeneratedDoc
	if err := json.Unmarshal(genBytes, &gen); err != nil {
		return nil, nil, fmt.Errorf("parse generated: %w", err)
	}
	// UE 推送 world_kb 时 authored 字段可能缺失（payload 里没这个 key）或为空，
	// 导致上层传入 nil/[]byte{}。AuthoredDoc 是可选的人类覆盖层，空值应降级为
	// 仅含 version 的空对象而非报错——否则 stable 端 UE 不发 authored 时每次
	// 都 merge 失败。version 取 generated 的 schema_version 以通过 Merge 校验。
	if len(bytes.TrimSpace(authBytes)) == 0 {
		authBytes = []byte(fmt.Sprintf(`{"version":%q}`, gen.SchemaVersion))
	}
	var auth AuthoredDoc
	if err := json.Unmarshal(authBytes, &auth); err != nil {
		return nil, nil, fmt.Errorf("parse authored: %w", err)
	}

	// UE 推送的世界 KB 偶尔出现大写开头的 object/zone id（如 "Charge"），
	// 与协议 idRegex 冲突。MCP 作为协议容错入口统一小写化。
	changes := normalizeEntityIDs(&gen, &auth)

	kb, _, err := Merge(&gen, &auth)
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
		if gen.Source.MapPackage != "" {
			sourceMap = gen.Source.MapPackage
		}
		if err := WriteManifest(genBytes, authBytes, mergedBytes, manifestPath, sourceMap); err != nil {
			return nil, nil, fmt.Errorf("write manifest: %w", err)
		}
	}

	return kb, changes, nil
}
