package worldkb

// schema.go defines the JSON schemas for the two merge inputs (world.generated.json
// and world.authored.json) plus the merged output types that the merger produces
// and the serializer writes as world_kb.yaml.
//
// NEW schema (2026-07): authored side drops site/connected_to/required_roles/
// role/personality[] and adds narrative/connections[]/aliases/tags/profession/
// personality{traits,speech_style}. Generated side adds bounds.rotation and
// renames available_actions → available_interactions.

// ---------------------------------------------------------------------------
// Generated document (world.generated.json) — §6.1-6.4
// ---------------------------------------------------------------------------

// GeneratedDoc mirrors the top-level structure of world.generated.json.
type GeneratedDoc struct {
	Schema            string             `json:"$schema"`
	SchemaVersion     string             `json:"schema_version"`
	GeneratedAt       string             `json:"generated_at"`
	Generator         GeneratedGenerator `json:"generator"`
	Source            GeneratedSource    `json:"source"`
	CoordinateSystem  GeneratedCoord     `json:"coordinate_system"`
	Zones             []GeneratedZone    `json:"zones"`
	Objects           []GeneratedObject  `json:"objects"`
	Agents            []GeneratedAgent   `json:"agents"`
	ValidationSummary GeneratedValidation `json:"validation_summary"`
}

type GeneratedGenerator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type GeneratedSource struct {
	MapPackage string `json:"map_package"`
	MapName    string `json:"map_name"`
}

type GeneratedCoord struct {
	Space         string `json:"space"`
	DistanceUnit  string `json:"distance_unit"`
	RotationUnit  string `json:"rotation_unit"`
	RotationOrder string `json:"rotation_order"`
}

type GeneratedValidation struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

// GeneratedZone mirrors §6.2 Zone entry.
type GeneratedZone struct {
	ID          string          `json:"id"`
	EditorLabel string          `json:"editor_label"`
	ActorPath   string          `json:"actor_path"`
	Bounds      GeneratedBounds `json:"bounds"`
	EntryPoint  []float64       `json:"entry_point"`
	EntryFacing []float64       `json:"entry_facing"`
}

// GeneratedBounds — NEW schema adds Rotation (pitch/yaw/roll degrees).
type GeneratedBounds struct {
	Center   []float64 `json:"center"`
	Extent   []float64 `json:"extent"`
	Rotation []float64 `json:"rotation,omitempty"`
}

// GeneratedObject mirrors §6.3 Smart Object entry.
// NEW schema renames available_actions → available_interactions.
type GeneratedObject struct {
	ID                   string   `json:"id"`
	Category             string   `json:"category"`
	ZoneID               string   `json:"zone_id"`
	EditorLabel          string   `json:"editor_label"`
	ActorClass           string   `json:"actor_class"`
	ActorPosition        []float64 `json:"actor_position"`
	InteractionPoint     []float64 `json:"interaction_point"`
	InteractionFacing    []float64 `json:"interaction_facing"`
	AvailableInteractions []string `json:"available_interactions"`
	DefaultState         string   `json:"default_state"`
}

// GeneratedAgent mirrors §6.4 Agent entry.
type GeneratedAgent struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	InitialZone      string    `json:"initial_zone"`
	EditorLabel      string    `json:"editor_label"`
	ActorClass       string    `json:"actor_class"`
	InitialPosition  []float64 `json:"initial_position"`
	ActionTable      string    `json:"action_table"`
	MainBehaviorTree string    `json:"main_behavior_tree"`
}

// ---------------------------------------------------------------------------
// Authored document (world.authored.json) — NEW schema (2026-07)
// ---------------------------------------------------------------------------
//
// Key changes vs OLD schema:
//   - Top-level: drops $schema, schema_version, site, top-level relationships[].
//     Adds version (string), narrative {setting, theme}.
//   - Zone: drops connected_to: []string. Adds aliases: []string,
//     connections: [{to, type, bidirectional}].
//   - Object: drops required_roles, capacity, interaction_radius. Adds tags.
//   - Agent: drops role: []string, personality: []string, home_zone,
//     core_memories. Adds profession: string, personality {traits, speech_style},
//     description, initial_zone, per-agent relationships[].
//   - Relationships now live per-agent (authored.agents[id].relationships)
//     rather than at the top level.

// AuthoredDoc mirrors the top-level structure of world.authored.json (NEW schema).
// Zones/Objects/Agents are ID-keyed dicts for easy overlay merging.
type AuthoredDoc struct {
	Version       string                    `json:"version"`
	Narrative     AuthoredNarrative         `json:"narrative"`
	Zones         map[string]AuthoredZone   `json:"zones"`
	Objects       map[string]AuthoredObject `json:"objects"`
	Agents        map[string]AuthoredAgent  `json:"agents"`
}

// AuthoredNarrative carries top-level narrative metadata (replaces Site).
type AuthoredNarrative struct {
	Setting string `json:"setting"`
	Theme   string `json:"theme"`
}

// AuthoredZone carries narrative + topology for a zone. Protected spatial
// fields (bounds/entry_point/entry_facing/actor_path) are NOT present here —
// authored data must not override them.
type AuthoredZone struct {
	DisplayName  string              `json:"display_name"`
	Description  string              `json:"description"`
	Aliases      []string            `json:"aliases"`
	Connections  []AuthoredConnection `json:"connections"`
}

// AuthoredConnection is a structured topology edge (NEW schema: replaces
// the old ConnectedTo []string).
type AuthoredConnection struct {
	To            string `json:"to"`
	Type          string `json:"type"`
	Bidirectional bool   `json:"bidirectional"`
}

// AuthoredObject — NEW schema drops required_roles/capacity/interaction_radius
// and adds tags.
type AuthoredObject struct {
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// AuthoredAgent — NEW schema: drops role/personality[]/home_zone/core_memories;
// adds profession, personality struct, description, initial_zone,
// per-agent relationships.
type AuthoredAgent struct {
	DisplayName   string                 `json:"display_name"`
	Description   string                 `json:"description"`
	Profession    string                 `json:"profession"`
	Personality   AuthoredPersonality    `json:"personality"`
	InitialZone   string                 `json:"initial_zone"`
	Relationships []AuthoredRelationship `json:"relationships"`
}

// AuthoredPersonality — NEW schema: structured object replacing []string.
type AuthoredPersonality struct {
	Traits      []string `json:"traits"`
	SpeechStyle string   `json:"speech_style"`
}

type AuthoredRelationship struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Familiarity int    `json:"familiarity"`
	Affection   int    `json:"affection"`
	Type        string `json:"type"`
}

// ---------------------------------------------------------------------------
// Protected fields — authored data must not override these (§8.3)
// ---------------------------------------------------------------------------

// protectedZoneFields are spatial/fact fields that only generated data may set.
// AuthoredZone does not even carry these fields, so enforcement is structural —
// but we keep the list for validator diagnostics and future schema evolution.
var protectedZoneFields = []string{
	"bounds", "entry_point", "entry_facing", "actor_path",
}

var protectedObjectFields = []string{
	"actor_class", "actor_position", "interaction_point",
	"interaction_facing", "default_state",
}

var protectedAgentFields = []string{
	"actor_class", "initial_position", "initial_zone",
	"action_table", "main_behavior_tree",
}

// defaultInteractionRadius is applied when neither generated nor authored
// specifies one. NEW schema drops interaction_radius from authored, so this
// default is always applied unless a future generated field reintroduces it.
const defaultInteractionRadius = 1500.0
