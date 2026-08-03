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
type Object struct {
	ID                   string
	DisplayName          string
	Description          string
	Category             string
	ZoneID               string
	ActorClass           string
	ActorPosition        [3]float64
	InteractionPoint     [3]float64
	InteractionFacing    [3]float64
	InteractionRadius    float64
	AvailableInteractions []string
	DefaultState         string
	Tags                 []string
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
