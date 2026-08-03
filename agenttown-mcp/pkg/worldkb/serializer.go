package worldkb

// serializer.go writes a KB to world_kb.yaml (deterministic, sorted by ID)
// and a world_kb.manifest.json with SHA256 hashes.
//
// YAML output uses snake_case keys. Atomic file replacement (temp file +
// rename) prevents half-written output on failure.

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

// yamlKB mirrors the YAML output structure with explicit snake_case tags.
// Vector fields use []float64 so yaml.v3 emits flow-style [1, 2, 3].
type yamlKB struct {
	Version       string             `yaml:"version"`
	Narrative     yamlNarrative      `yaml:"narrative"`
	Zones         []yamlZone         `yaml:"zones"`
	Objects       []yamlObject       `yaml:"objects"`
	Agents        []yamlAgent        `yaml:"agents"`
	Relationships []yamlRelationship `yaml:"relationships,omitempty"`
}

type yamlNarrative struct {
	Setting string `yaml:"setting,omitempty"`
	Theme   string `yaml:"theme,omitempty"`
}

type yamlZone struct {
	ID          string          `yaml:"id"`
	DisplayName string          `yaml:"display_name,omitempty"`
	Description string          `yaml:"description,omitempty"`
	Aliases     []string        `yaml:"aliases,omitempty"`
	Bounds      yamlBounds      `yaml:"bounds"`
	EntryPoint  []float64       `yaml:"entry_point"`
	EntryFacing []float64       `yaml:"entry_facing,omitempty"`
	Connections []yamlConnection `yaml:"connections,omitempty"`
}

type yamlConnection struct {
	To            string `yaml:"to"`
	Type          string `yaml:"type,omitempty"`
	Bidirectional bool   `yaml:"bidirectional,omitempty"`
}

type yamlBounds struct {
	Center   []float64 `yaml:"center"`
	Extent   []float64 `yaml:"extent"`
	Rotation []float64 `yaml:"rotation,omitempty"`
}

type yamlObject struct {
	ID                    string    `yaml:"id"`
	DisplayName           string    `yaml:"display_name,omitempty"`
	Description           string    `yaml:"description,omitempty"`
	Category              string    `yaml:"category,omitempty"`
	ZoneID                string    `yaml:"zone_id,omitempty"`
	ActorClass            string    `yaml:"actor_class,omitempty"`
	ActorPosition         []float64 `yaml:"actor_position,omitempty"`
	InteractionPoint      []float64 `yaml:"interaction_point"`
	InteractionFacing     []float64 `yaml:"interaction_facing,omitempty"`
	InteractionRadius     float64   `yaml:"interaction_radius,omitempty"`
	AvailableInteractions []string  `yaml:"available_interactions,omitempty"`
	DefaultState          string    `yaml:"default_state,omitempty"`
	Tags                  []string  `yaml:"tags,omitempty"`
}

type yamlAgent struct {
	ID               string         `yaml:"id"`
	DisplayName      string         `yaml:"display_name,omitempty"`
	Description      string         `yaml:"description,omitempty"`
	Type             string         `yaml:"type,omitempty"`
	Profession       string         `yaml:"profession,omitempty"`
	Personality      yamlPersonality `yaml:"personality,omitempty"`
	InitialZone      string         `yaml:"initial_zone,omitempty"`
	InitialPosition  []float64      `yaml:"initial_position,omitempty"`
	ActorClass       string         `yaml:"actor_class,omitempty"`
	ActionTable      string         `yaml:"action_table,omitempty"`
	MainBehaviorTree string         `yaml:"main_behavior_tree,omitempty"`
}

type yamlPersonality struct {
	Traits      []string `yaml:"traits,omitempty"`
	SpeechStyle string   `yaml:"speech_style,omitempty"`
}

type yamlRelationship struct {
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	Familiarity int    `yaml:"familiarity,omitempty"`
	Affection   int    `yaml:"affection,omitempty"`
	Type        string `yaml:"type,omitempty"`
}

// toYAML converts a KB to its YAML representation struct.
func toYAML(kb *KB) yamlKB {
	out := yamlKB{
		Version: kb.Version,
		Narrative: yamlNarrative{
			Setting: kb.Narrative.Setting,
			Theme:   kb.Narrative.Theme,
		},
		Zones:         make([]yamlZone, 0, len(kb.Zones)),
		Objects:       make([]yamlObject, 0, len(kb.Objects)),
		Agents:        make([]yamlAgent, 0, len(kb.Agents)),
		Relationships: make([]yamlRelationship, 0, len(kb.Relationships)),
	}

	for _, z := range kb.Zones {
		zone := yamlZone{
			ID:          z.ID,
			DisplayName: z.DisplayName,
			Description: z.Description,
			Aliases:     z.Aliases,
			Bounds: yamlBounds{
				Center:   vec3ToSlice(z.Bounds.Center),
				Extent:   vec3ToSlice(z.Bounds.Extent),
				Rotation: vec3ToSliceOptional(z.Bounds.Rotation),
			},
			EntryPoint:  vec3ToSlice(z.EntryPoint),
			EntryFacing: vec3ToSlice(z.EntryFacing),
		}
		for _, c := range z.Connections {
			zone.Connections = append(zone.Connections, yamlConnection{
				To:            c.To,
				Type:          c.Type,
				Bidirectional: c.Bidirectional,
			})
		}
		out.Zones = append(out.Zones, zone)
	}

	for _, o := range kb.Objects {
		out.Objects = append(out.Objects, yamlObject{
			ID:                    o.ID,
			DisplayName:           o.DisplayName,
			Description:           o.Description,
			Category:              o.Category,
			ZoneID:                o.ZoneID,
			ActorClass:            o.ActorClass,
			ActorPosition:         vec3ToSlice(o.ActorPosition),
			InteractionPoint:      vec3ToSlice(o.InteractionPoint),
			InteractionFacing:     vec3ToSlice(o.InteractionFacing),
			InteractionRadius:     o.InteractionRadius,
			AvailableInteractions: o.AvailableInteractions,
			DefaultState:          o.DefaultState,
			Tags:                  o.Tags,
		})
	}

	for _, a := range kb.Agents {
		out.Agents = append(out.Agents, yamlAgent{
			ID:               a.ID,
			DisplayName:      a.DisplayName,
			Description:      a.Description,
			Type:             a.Type,
			Profession:       a.Profession,
			Personality: yamlPersonality{
				Traits:      a.Personality.Traits,
				SpeechStyle: a.Personality.SpeechStyle,
			},
			InitialZone:      a.InitialZone,
			InitialPosition:  vec3ToSlice(a.InitialPosition),
			ActorClass:       a.ActorClass,
			ActionTable:      a.ActionTable,
			MainBehaviorTree: a.MainBehaviorTree,
		})
	}

	for _, r := range kb.Relationships {
		out.Relationships = append(out.Relationships, yamlRelationship{
			From:        r.From,
			To:          r.To,
			Familiarity: r.Familiarity,
			Affection:   r.Affection,
			Type:        r.Type,
		})
	}

	return out
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

	doc := toYAML(kb)
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
