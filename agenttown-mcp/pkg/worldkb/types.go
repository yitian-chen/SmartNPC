// Package worldkb loads and queries the World Knowledge Base (world_kb.yaml).
//
// The KB is the "world dictionary" shared by the UE5 side and the Agent
// process. It maps semantic IDs (e.g. "workbench_01") to coordinates and
// available actions, so the MCP layer can translate LLM-intent targets into
// concrete coordinates before dispatching action_command to UE.
//
// Schema is defined in docs/AgentTown_Design.md §子系统1 and mirrored by the
// sample file at assets/world_kb.yaml.
package worldkb

// KB is the in-memory World Knowledge Base loaded from world_kb.yaml.
// Public slices preserve declaration order; theByID maps are lookup indexes
// built by Load and must not be mutated after construction.
type KB struct {
	Version       string
	Site          Site
	Zones         []Zone
	Locations     []Location
	Objects       []Object
	Agents        []Agent
	Relationships []Relationship

	// Lookup indexes (populated by Load). Pointers into the slices above.
	zoneByID     map[string]*Zone
	locationByID map[string]*Location
	objectByID   map[string]*Object
	agentByID    map[string]*Agent
}

// Site is the top-level site metadata.
type Site struct {
	ID   string
	Name string
}

// Zone is a region of the world (e.g. "main_workshop").
type Zone struct {
	ID          string
	Name        string
	Description string
	UE5Bounds   Bounds
	EntryPoint  [3]float64
	ConnectedTo []string
	Locations   []string
}

// Location is a named point of interest (e.g. "workbench_01").
type Location struct {
	ID                 string
	Name               string
	Zone               string
	Type               string
	Position           [3]float64
	InteractionPoint   [3]float64
	InteractionRadius  float64
	Facing             [3]float64
	AvailableActions   []string
	UE5Ref             string
}

// Object is a smart object that can be interacted with (mirrors a Location
// for now; kept separate because the design doc treats them as distinct
// entities with their own capacity/role constraints).
type Object struct {
	ID               string
	AvailableActions []string
	RequiredRole     []string
	Capacity         int
	UE5Ref           string
}

// Agent is an NPC definition.
type Agent struct {
	ID              string
	Name            string
	Type            string
	Role            []string
	Capabilities    []string
	DefaultZone     string
	DefaultPosition [3]float64
	UE5Class        string
	UE5Variant      string
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
	Center    [3]float64
	HalfSize  [3]float64
}
