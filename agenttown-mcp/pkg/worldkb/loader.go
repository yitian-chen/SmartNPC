package worldkb

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// rawKB mirrors world_kb.yaml structure for the first-pass unmarshal.
// Field names use the YAML snake_case keys directly via struct tags.
type rawKB struct {
	Version       string        `yaml:"version"`
	Site          rawSite       `yaml:"site"`
	Zones         []rawZone     `yaml:"zones"`
	Objects       []rawObject   `yaml:"objects"`
	Agents        []rawAgent    `yaml:"agents"`
	Relationships []rawRel      `yaml:"relationships"`
}

type rawSite struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description"`
}

type rawZone struct {
	ID          string    `yaml:"id"`
	DisplayName string    `yaml:"display_name"`
	Description string    `yaml:"description"`
	Bounds      rawBounds `yaml:"bounds"`
	EntryPoint  []float64 `yaml:"entry_point"`
	EntryFacing []float64 `yaml:"entry_facing"`
	ConnectedTo []string  `yaml:"connected_to"`
}

type rawObject struct {
	ID                string    `yaml:"id"`
	DisplayName       string    `yaml:"display_name"`
	Description       string    `yaml:"description"`
	Category          string    `yaml:"category"`
	ZoneID            string    `yaml:"zone_id"`
	ActorClass        string    `yaml:"actor_class"`
	ActorPosition     []float64 `yaml:"actor_position"`
	InteractionPoint  []float64 `yaml:"interaction_point"`
	InteractionFacing []float64 `yaml:"interaction_facing"`
	InteractionRadius float64   `yaml:"interaction_radius"`
	AvailableActions  []string  `yaml:"available_actions"`
	DefaultState      string    `yaml:"default_state"`
	RequiredRoles     []string  `yaml:"required_roles"`
	Capacity          int       `yaml:"capacity"`
}

type rawAgent struct {
	ID               string    `yaml:"id"`
	DisplayName      string    `yaml:"display_name"`
	Type             string    `yaml:"type"`
	Role             []string  `yaml:"role"`
	Personality      []string  `yaml:"personality"`
	InitialZone      string    `yaml:"initial_zone"`
	InitialPosition  []float64 `yaml:"initial_position"`
	HomeZone         string    `yaml:"home_zone"`
	CoreMemories     []string  `yaml:"core_memories"`
	ActorClass       string    `yaml:"actor_class"`
	ActionTable      string    `yaml:"action_table"`
	MainBehaviorTree string    `yaml:"main_behavior_tree"`
}

type rawRel struct {
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	Familiarity int    `yaml:"familiarity"`
	Affection   int    `yaml:"affection"`
	Type        string `yaml:"type"`
}

type rawBounds struct {
	Center []float64 `yaml:"center"`
	Extent []float64 `yaml:"extent"`
}

// Load reads and parses the world_kb.yaml file at path, validates required
// fields, and builds the in-memory KB with lookup indexes.
//
// Any structural error (missing file, malformed YAML, missing required
// fields, duplicate IDs, wrong array arity) returns an error. Callers
// should treat errors as fatal — the MCP server cannot serve tools that
// need semantic→coordinate translation without a valid KB.
func Load(path string) (*KB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var raw rawKB
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	kb := &KB{
		Version: raw.Version,
		Site: Site{
			ID:          raw.Site.ID,
			DisplayName: raw.Site.DisplayName,
			Description: raw.Site.Description,
		},
		zoneByID:   make(map[string]*Zone),
		objectByID: make(map[string]*Object),
		agentByID:  make(map[string]*Agent),
	}

	// Zones
	kb.Zones = make([]Zone, 0, len(raw.Zones))
	for i, rz := range raw.Zones {
		if rz.ID == "" {
			return nil, fmt.Errorf("zone[%d]: missing id", i)
		}
		if _, dup := kb.zoneByID[rz.ID]; dup {
			return nil, fmt.Errorf("zone[%d]: duplicate id %q", i, rz.ID)
		}
		entry, err := toVec3(rz.EntryPoint, "entry_point", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		facing, err := toVec3(rz.EntryFacing, "entry_facing", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		center, err := toVec3(rz.Bounds.Center, "bounds.center", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		extent, err := toVec3(rz.Bounds.Extent, "bounds.extent", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		z := Zone{
			ID:          rz.ID,
			DisplayName: rz.DisplayName,
			Description: rz.Description,
			EntryPoint:  entry,
			EntryFacing: facing,
			Bounds: Bounds{
				Center: center,
				Extent: extent,
			},
			ConnectedTo: rz.ConnectedTo,
		}
		kb.Zones = append(kb.Zones, z)
		kb.zoneByID[z.ID] = &kb.Zones[len(kb.Zones)-1]
	}

	// Objects
	kb.Objects = make([]Object, 0, len(raw.Objects))
	for i, ro := range raw.Objects {
		if ro.ID == "" {
			return nil, fmt.Errorf("object[%d]: missing id", i)
		}
		if _, dup := kb.objectByID[ro.ID]; dup {
			return nil, fmt.Errorf("object[%d]: duplicate id %q", i, ro.ID)
		}
		actorPos, err := toVec3(ro.ActorPosition, "actor_position", ro.ID)
		if err != nil {
			return nil, fmt.Errorf("object %q: %w", ro.ID, err)
		}
		ip, err := toVec3(ro.InteractionPoint, "interaction_point", ro.ID)
		if err != nil {
			return nil, fmt.Errorf("object %q: %w", ro.ID, err)
		}
		ifacing, err := toVec3(ro.InteractionFacing, "interaction_facing", ro.ID)
		if err != nil {
			return nil, fmt.Errorf("object %q: %w", ro.ID, err)
		}
		radius := ro.InteractionRadius
		if radius == 0 {
			radius = defaultInteractionRadius
		}
		kb.Objects = append(kb.Objects, Object{
			ID:                ro.ID,
			DisplayName:       ro.DisplayName,
			Description:       ro.Description,
			Category:          ro.Category,
			ZoneID:            ro.ZoneID,
			ActorClass:        ro.ActorClass,
			ActorPosition:     actorPos,
			InteractionPoint:  ip,
			InteractionFacing: ifacing,
			InteractionRadius: radius,
			AvailableActions:  ro.AvailableActions,
			DefaultState:      ro.DefaultState,
			RequiredRoles:     ro.RequiredRoles,
			Capacity:          ro.Capacity,
		})
		kb.objectByID[ro.ID] = &kb.Objects[len(kb.Objects)-1]
	}

	// Agents
	kb.Agents = make([]Agent, 0, len(raw.Agents))
	for i, ra := range raw.Agents {
		if ra.ID == "" {
			return nil, fmt.Errorf("agent[%d]: missing id", i)
		}
		if _, dup := kb.agentByID[ra.ID]; dup {
			return nil, fmt.Errorf("agent[%d]: duplicate id %q", i, ra.ID)
		}
		ip, err := toVec3(ra.InitialPosition, "initial_position", ra.ID)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", ra.ID, err)
		}
		kb.Agents = append(kb.Agents, Agent{
			ID:               ra.ID,
			DisplayName:      ra.DisplayName,
			Type:             ra.Type,
			Role:             ra.Role,
			Personality:      ra.Personality,
			InitialZone:      ra.InitialZone,
			InitialPosition:  ip,
			HomeZone:         ra.HomeZone,
			CoreMemories:     ra.CoreMemories,
			ActorClass:       ra.ActorClass,
			ActionTable:      ra.ActionTable,
			MainBehaviorTree: ra.MainBehaviorTree,
		})
		kb.agentByID[ra.ID] = &kb.Agents[len(kb.Agents)-1]
	}

	// Relationships (optional, no index)
	kb.Relationships = make([]Relationship, 0, len(raw.Relationships))
	for i, rr := range raw.Relationships {
		if rr.From == "" || rr.To == "" {
			return nil, fmt.Errorf("relationship[%d]: missing from/to", i)
		}
		kb.Relationships = append(kb.Relationships, Relationship{
			From:        rr.From,
			To:          rr.To,
			Familiarity: rr.Familiarity,
			Affection:   rr.Affection,
			Type:        rr.Type,
		})
	}

	return kb, nil
}

// toVec3 converts a YAML list of 3 numbers to a [3]float64. Returns an
// error identifying the field and owning entity if the arity is wrong.
func toVec3(v []float64, field, owner string) ([3]float64, error) {
	if len(v) != 3 {
		return [3]float64{}, fmt.Errorf("%s of %s: expected 3 floats, got %d", field, owner, len(v))
	}
	return [3]float64{v[0], v[1], v[2]}, nil
}
