// Package worldkb loads and queries the World Knowledge Base (world_kb.yaml).
//
// The KB is the "world dictionary" shared by the UE5 side and the Agent
// process. It maps semantic IDs (e.g. "workbench_01") to coordinates and
// available actions, so the MCP layer can translate LLM-intent targets into
// concrete coordinates before dispatching action_command to UE.
//
// Schema is defined in docs/AgentTown_WorldKB_Design.md §8.7 and produced by
// the worldkb-merge CLI from world.generated.json + world.authored.json.
package worldkb

// KB is the in-memory World Knowledge Base loaded from world_kb.yaml.
// Public slices preserve declaration order; the byID maps are lookup indexes
// built by Load and must not be mutated after construction.
type KB struct {
	Version       string
	Site          Site
	Zones         []Zone
	Objects       []Object
	Agents        []Agent
	Relationships []Relationship

	// Lookup indexes (populated by Load). Pointers into the slices above.
	zoneByID   map[string]*Zone
	objectByID map[string]*Object
	agentByID  map[string]*Agent
}

// Site is the top-level site metadata.
type Site struct {
	ID          string
	DisplayName string
	Description string
}

// Zone is a region of the world (e.g. "main_workshop").
type Zone struct {
	ID          string
	DisplayName string
	Description string
	Bounds      Bounds
	EntryPoint  [3]float64
	EntryFacing [3]float64
	ConnectedTo []string
}

// Object is a smart object that can be interacted with. Combines the former
// Location fields (interaction_point, zone_id, etc) with the smart-object
// metadata (category, capacity, required_roles). Per design doc §6.5 the
// separate locations[] array is gone — all spatial fields live here.
type Object struct {
	ID                string
	DisplayName       string
	Description       string
	Category          string
	ZoneID            string
	ActorClass        string
	ActorPosition     [3]float64
	InteractionPoint  [3]float64
	InteractionFacing [3]float64
	InteractionRadius float64
	AvailableActions  []string
	DefaultState      string
	RequiredRoles     []string
	Capacity          int
}

// Agent is an NPC definition.
type Agent struct {
	ID               string
	DisplayName      string
	Type             string
	Role             []string
	Personality      []string
	InitialZone      string
	InitialPosition  [3]float64
	HomeZone         string
	CoreMemories     []string
	ActorClass       string
	ActionTable      string
	MainBehaviorTree string
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
type Bounds struct {
	Center [3]float64
	Extent [3]float64
}
