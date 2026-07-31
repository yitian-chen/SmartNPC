package worldkb

// schema.go defines the JSON schemas for the two merge inputs (world.generated.json
// and world.authored.json) per docs/AgentTown_WorldKB_Design.md §6/§7, plus the
// merged output types that the merger produces and the serializer writes as
// world_kb.yaml (§8.7).
//
// In Step 1 these merged types (MergedKB, MergedZone, etc.) live here alongside
// the legacy KB/Zone/Location/Object/Agent in types.go. In Step 2 the legacy
// types are deleted and the Merged* types are promoted to types.go as the
// authoritative KB/Zone/Object/Agent.

// ---------------------------------------------------------------------------
// Generated document (world.generated.json) — §6.1-6.4
// ---------------------------------------------------------------------------

// GeneratedDoc mirrors the top-level structure of world.generated.json.
type GeneratedDoc struct {
	Schema           string             `json:"$schema"`
	SchemaVersion    string             `json:"schema_version"`
	GeneratedAt      string             `json:"generated_at"`
	Generator        GeneratedGenerator `json:"generator"`
	Source           GeneratedSource    `json:"source"`
	CoordinateSystem GeneratedCoord     `json:"coordinate_system"`
	Zones            []GeneratedZone    `json:"zones"`
	Objects          []GeneratedObject  `json:"objects"`
	Agents           []GeneratedAgent   `json:"agents"`
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
	ID          string        `json:"id"`
	EditorLabel string        `json:"editor_label"`
	ActorPath   string        `json:"actor_path"`
	Bounds      GeneratedBounds `json:"bounds"`
	EntryPoint  []float64     `json:"entry_point"`
	EntryFacing []float64     `json:"entry_facing"`
}

type GeneratedBounds struct {
	Center []float64 `json:"center"`
	Extent []float64 `json:"extent"`
}

// GeneratedObject mirrors §6.3 Smart Object entry.
type GeneratedObject struct {
	ID                string   `json:"id"`
	Category          string   `json:"category"`
	ZoneID            string   `json:"zone_id"`
	EditorLabel       string   `json:"editor_label"`
	ActorClass        string   `json:"actor_class"`
	ActorPosition     []float64 `json:"actor_position"`
	InteractionPoint  []float64 `json:"interaction_point"`
	InteractionFacing []float64 `json:"interaction_facing"`
	AvailableActions  []string `json:"available_actions"`
	DefaultState      string   `json:"default_state"`
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
// Authored document (world.authored.json) — §7
// ---------------------------------------------------------------------------

// AuthoredDoc mirrors the top-level structure of world.authored.json.
// Zones/Objects/Agents are ID-keyed dicts for easy overlay merging.
type AuthoredDoc struct {
	Schema         string             `json:"$schema"`
	SchemaVersion  string             `json:"schema_version"`
	Site           AuthoredSite       `json:"site"`
	Zones          map[string]AuthoredZone   `json:"zones"`
	Objects        map[string]AuthoredObject `json:"objects"`
	Agents         map[string]AuthoredAgent  `json:"agents"`
	Relationships  []AuthoredRelationship    `json:"relationships"`
}

type AuthoredSite struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// AuthoredZone carries narrative + topology for a zone. Protected spatial
// fields (bounds/entry_point/entry_facing/actor_path) are NOT present here —
// authored data must not override them.
type AuthoredZone struct {
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description"`
	ConnectedTo  []string `json:"connected_to"`
}

type AuthoredObject struct {
	DisplayName    string   `json:"display_name"`
	Description    string   `json:"description"`
	RequiredRoles  []string `json:"required_roles"`
	Capacity       int      `json:"capacity"`
	// InteractionRadius is an Agent-side extension (not in §6.3/§7) that
	// preserves mock_ue.py's coordinate-based reverse-lookup. Authored may
	// override the generated default (1500cm); generated JSON omits it.
	InteractionRadius float64 `json:"interaction_radius,omitempty"`
}

type AuthoredAgent struct {
	DisplayName  string   `json:"display_name"`
	Role         []string `json:"role"`
	Personality  []string `json:"personality"`
	HomeZone     string   `json:"home_zone"`
	CoreMemories []string `json:"core_memories"`
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
// specifies one (generated JSON omits this field per §6.3).
const defaultInteractionRadius = 1500.0
