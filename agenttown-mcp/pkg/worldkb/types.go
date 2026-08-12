package worldkb

// KB is the in-memory World Knowledge Base loaded from world_kb.yaml.
// Public slices preserve declaration order; the byID maps are lookup indexes
// built by Load and must not be mutated after construction.
type KB struct {
	Version       string
	Narrative     Narrative
	Zones         []Zone
	Objects       []Object
	Agents        []Agent
	Relationships []Relationship

	// Lookup indexes (populated by Load). Pointers into the slices above.
	zoneByID   map[string]*Zone
	objectByID map[string]*Object
	agentByID  map[string]*Agent
}

// Narrative is the top-level narrative metadata (NEW schema: replaces Site).
type Narrative struct {
	Setting string
	Theme   string
}

// Zone is a region of the world (e.g. "main_workshop").
type Zone struct {
	ID          string
	DisplayName string
	Description string
	Aliases     []string
	Bounds      Bounds
	EntryPoint  [3]float64
	EntryFacing [3]float64
	Connections []Connection
	// Extra carries any additional keys not modeled by the typed struct
	// (e.g. future UE-generated fields). Populated by the loader and the
	// map-based merger; persisted by the serializer. Not validated.
	Extra map[string]any
}

// Connection is a directional topology edge from one zone to another.
type Connection struct {
	To           string
	Type         string
	Bidirectional bool
}

// Object is a smart object that can be interacted with. Combines the former
// Location fields (interaction_point, zone_id, etc) with the smart-object
// metadata (category, tags). Per design doc §6.5 the separate locations[]
// array is gone — all spatial fields live here.
//
// SemanticGroup is the UE5-facing group name used as the `semantic_group`
// parameter value for InteractSmartObject and composite actions (e.g.
// "charger", "workbench", "sleep_pod"). It is authored in world.authored.json
// per object group and flows into the tactical prompt so the LLM emits
// UE5-recognized values. Instance IDs like "Charge-1" are NOT valid
// semantic_group values — the group name is.
type Object struct {
	ID                   string
	DisplayName          string
	Description          string
	Category             string
	SemanticGroup        string
	ZoneID               string
	ActorClass           string
	ActorPosition        [3]float64
	InteractionPoint     [3]float64
	InteractionFacing    [3]float64
	InteractionRadius    float64
	AvailableInteractions []string
	DefaultState         string
	Tags                 []string
	// Extra carries any additional keys not modeled by the typed struct.
	// See Zone.Extra for semantics.
	Extra map[string]any
}

// Agent is an NPC definition.
type Agent struct {
	ID               string
	DisplayName      string
	Description      string
	Type             string
	Profession       string
	Personality      Personality
	InitialZone      string
	InitialPosition  [3]float64
	ActorClass       string
	ActionTable      string
	MainBehaviorTree string
	// Extra carries any additional keys not modeled by the typed struct.
	// See Zone.Extra for semantics.
	Extra map[string]any
}

// Personality is the per-agent personality overlay (NEW schema: struct, not []string).
type Personality struct {
	Traits      []string
	SpeechStyle string
}

// Relationship is a directional social relation between two agents.
type Relationship struct {
	From        string
	To          string
	Familiarity int
	Affection   int
	Type        string
}

// Bounds is an axis-aligned bounding box in UE5 world space (cm).
// Rotation is the optional bounds rotation (pitch/yaw/roll degrees) per
// NEW schema (generated.bounds.rotation).
type Bounds struct {
	Center   [3]float64
	Extent   [3]float64
	Rotation [3]float64
}
