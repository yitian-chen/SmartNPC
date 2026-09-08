package worldkb

// serializer.go writes a KB to world_kb.yaml (deterministic, sorted by ID)
// and a world_kb.manifest.json with SHA256 hashes.
//
// YAML output uses snake_case keys. Each entity is serialized as a
// map[string]any (known fields + merged Extra) so unknown fields round-trip.
// yaml.v3 emits map keys alphabetically — this is accepted (deterministic;
// field order differs from the pre-refactor struct-ordered output but is
// stable across runs). Atomic file replacement (temp file + rename)
// prevents half-written output on failure.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// toYAMLMap converts a KB to a map[string]any tree ready for yaml.Marshal.
// Known fields are emitted with snake_case keys; each entity's Extra bag
// is merged in alongside the known fields. Known fields win on key
// conflicts (see mergeExtraIntoMap); Extra only fills gaps for fields the
// typed projection does not model.
func toYAMLMap(kb *KB) map[string]any {
	zones := make([]any, 0, len(kb.Zones))
	for _, z := range kb.Zones {
		zones = append(zones, zoneToMap(&z))
	}
	objects := make([]any, 0, len(kb.Objects))
	for _, o := range kb.Objects {
		objects = append(objects, objectToMap(&o))
	}
	agents := make([]any, 0, len(kb.Agents))
	for _, a := range kb.Agents {
		agents = append(agents, agentToMap(&a))
	}

	out := map[string]any{
		"version": kb.Version,
		"narrative": map[string]any{
			"setting": kb.Narrative.Setting,
			"theme":   kb.Narrative.Theme,
		},
		"zones":   zones,
		"objects": objects,
		"agents":  agents,
	}
	if len(kb.Relationships) > 0 {
		rels := make([]any, 0, len(kb.Relationships))
		for _, r := range kb.Relationships {
			rels = append(rels, relationshipToMap(&r))
		}
		out["relationships"] = rels
	}
	return out
}

func zoneToMap(z *Zone) map[string]any {
	m := map[string]any{
		"id":          z.ID,
		"display_name": z.DisplayName,
		"description":  z.Description,
		"bounds": map[string]any{
			"center":   vec3ToSlice(z.Bounds.Center),
			"extent":   vec3ToSlice(z.Bounds.Extent),
			"rotation": vec3ToSliceOptional(z.Bounds.Rotation),
		},
		"entry_point":  vec3ToSlice(z.EntryPoint),
		"entry_facing": vec3ToSlice(z.EntryFacing),
	}
	if len(z.Aliases) > 0 {
		m["aliases"] = z.Aliases
	}
	if len(z.Connections) > 0 {
		conns := make([]any, 0, len(z.Connections))
		for _, c := range z.Connections {
			cm := map[string]any{"to": c.To}
			if c.Type != "" {
				cm["type"] = c.Type
			}
			if c.Bidirectional {
				cm["bidirectional"] = c.Bidirectional
			}
			conns = append(conns, cm)
		}
		m["connections"] = conns
	}
	mergeExtraIntoMap(m, z.Extra)
	return m
}

func objectToMap(o *Object) map[string]any {
	m := map[string]any{
		"id":                  o.ID,
		"display_name":        o.DisplayName,
		"description":         o.Description,
		"category":            o.Category,
		"zone_id":             o.ZoneID,
		"actor_class":         o.ActorClass,
		"actor_position":      vec3ToSlice(o.ActorPosition),
		"interaction_point":   vec3ToSlice(o.InteractionPoint),
		"interaction_facing":  vec3ToSlice(o.InteractionFacing),
		"interaction_radius":  o.InteractionRadius,
		"default_state":       o.DefaultState,
	}
	if o.SemanticGroup != "" {
		m["semantic_group"] = o.SemanticGroup
	}
	if len(o.AvailableInteractions) > 0 {
		m["available_interactions"] = o.AvailableInteractions
	}
	if len(o.Tags) > 0 {
		m["tags"] = o.Tags
	}
	mergeExtraIntoMap(m, o.Extra)
	return m
}

func agentToMap(a *Agent) map[string]any {
	m := map[string]any{
		"id":                 a.ID,
		"display_name":       a.DisplayName,
		"description":        a.Description,
		"type":               a.Type,
		"profession":         a.Profession,
		"initial_zone":       a.InitialZone,
		"initial_position":   vec3ToSlice(a.InitialPosition),
		"actor_class":        a.ActorClass,
		"action_table":       a.ActionTable,
		"main_behavior_tree": a.MainBehaviorTree,
	}
	if len(a.Personality.Traits) > 0 || a.Personality.SpeechStyle != "" {
		pm := map[string]any{}
		if len(a.Personality.Traits) > 0 {
			pm["traits"] = a.Personality.Traits
		}
		if a.Personality.SpeechStyle != "" {
			pm["speech_style"] = a.Personality.SpeechStyle
		}
		m["personality"] = pm
	}
	mergeExtraIntoMap(m, a.Extra)
	return m
}

func relationshipToMap(r *Relationship) map[string]any {
	m := map[string]any{
		"from": r.From,
		"to":   r.To,
	}
	if r.Familiarity != 0 {
		m["familiarity"] = r.Familiarity
	}
	if r.Affection != 0 {
		m["affection"] = r.Affection
	}
	if r.Type != "" {
		m["type"] = r.Type
	}
	return m
}

// mergeExtraIntoMap copies keys from extra into m, but does NOT override
// keys already present in m. Known/typed fields (written by zoneToMap /
// objectToMap / agentToMap before calling this) win; Extra only fills gaps
// for genuinely unknown fields.
//
// This matters for available_interactions: projectObject stashes the raw
// UE5 object form [{name, description}] into Extra while also extracting
// names into the typed []string slot. The typed slot is the canonical YAML
// representation (loader only parses []string); letting Extra overwrite it
// would produce YAML the loader cannot read back.
func mergeExtraIntoMap(m map[string]any, extra map[string]any) {
	for k, v := range extra {
		if _, exists := m[k]; exists {
			continue
		}
		m[k] = v
	}
}

func vec3ToSlice(v [3]float64) []float64 {
	return []float64{v[0], v[1], v[2]}
}

// vec3ToSliceOptional returns nil for an all-zero vector so YAML omits the
// field (cleaner output for optional fields like bounds.rotation).
func vec3ToSliceOptional(v [3]float64) []float64 {
	if v == ([3]float64{}) {
		return nil
	}
	return []float64{v[0], v[1], v[2]}
}

// WriteYAML serializes kb to path as deterministically-ordered YAML.
// Output is written to a temp file in the same directory, then atomically
// renamed. The byte content is also returned for manifest hashing.
func WriteYAML(kb *KB, path string) ([]byte, error) {
	if kb == nil {
		return nil, fmt.Errorf("kb is nil")
	}

	doc := toYAMLMap(kb)
	data, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("yaml marshal: %w", err)
	}

	if err := atomicWrite(path, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return data, nil
}

// atomicWrite writes data to a temp file in the same dir as path, then renames
// it to path. Ensures we never leave a half-written file on failure.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".worldkb-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup if rename didn't happen.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// Manifest is the world_kb.manifest.json structure.
type Manifest struct {
	SchemaVersion   string `json:"schema_version"`
	GeneratedSHA256 string `json:"generated_sha256"`
	AuthoredSHA256  string `json:"authored_sha256"`
	MergedSHA256    string `json:"merged_sha256"`
	SourceMap       string `json:"source_map,omitempty"`
	MergedAt        string `json:"merged_at"`
}

// WriteManifest computes SHA256 hashes of the generated, authored, and merged
// documents and writes a manifest JSON to manifestPath.
func WriteManifest(generatedBytes, authoredBytes, mergedBytes []byte, manifestPath, sourceMap string) error {
	m := Manifest{
		SchemaVersion:   "1.0",
		GeneratedSHA256: sha256Hex(generatedBytes),
		AuthoredSHA256:  sha256Hex(authoredBytes),
		MergedSHA256:    sha256Hex(mergedBytes),
		SourceMap:       sourceMap,
		MergedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("json marshal manifest: %w", err)
	}
	if err := atomicWrite(manifestPath, data); err != nil {
		return fmt.Errorf("write %s: %w", manifestPath, err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
