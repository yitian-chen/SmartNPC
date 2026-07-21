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
	Locations     []rawLocation `yaml:"locations"`
	Objects       []rawObject   `yaml:"objects"`
	Agents        []rawAgent    `yaml:"agents"`
	Relationships []rawRel      `yaml:"relationships"`
}

type rawSite struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type rawZone struct {
	ID          string    `yaml:"id"`
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	UE5Bounds   rawBounds `yaml:"ue5_bounds"`
	EntryPoint  []float64 `yaml:"entry_point"`
	ConnectedTo []string  `yaml:"connected_to"`
	Locations   []string  `yaml:"locations"`
}

type rawLocation struct {
	ID                string    `yaml:"id"`
	Name              string    `yaml:"name"`
	Zone              string    `yaml:"zone"`
	Type              string    `yaml:"type"`
	Position          []float64 `yaml:"position"`
	InteractionPoint  []float64 `yaml:"interaction_point"`
	InteractionRadius float64   `yaml:"interaction_radius"`
	Facing            []float64 `yaml:"facing"`
	AvailableActions  []string  `yaml:"available_actions"`
	UE5Ref            string    `yaml:"ue5_ref"`
}

type rawObject struct {
	ID               string   `yaml:"id"`
	AvailableActions []string `yaml:"available_actions"`
	RequiredRole     []string `yaml:"required_role"`
	Capacity         int      `yaml:"capacity"`
	UE5Ref           string   `yaml:"ue5_ref"`
}

type rawAgent struct {
	ID              string    `yaml:"id"`
	Name            string    `yaml:"name"`
	Type            string    `yaml:"type"`
	Role            []string  `yaml:"role"`
	Capabilities    []string  `yaml:"capabilities"`
	DefaultZone     string    `yaml:"default_zone"`
	DefaultPosition []float64 `yaml:"default_position"`
	UE5Class        string    `yaml:"ue5_class"`
	UE5Variant      string    `yaml:"ue5_variant"`
}

type rawRel struct {
	From        string `yaml:"from"`
	To          string `yaml:"to"`
	Familiarity int    `yaml:"familiarity"`
	Affection   int    `yaml:"affection"`
	Type        string `yaml:"type"`
}

type rawBounds struct {
	Center   []float64 `yaml:"center"`
	HalfSize []float64 `yaml:"half_size"`
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
			ID:   raw.Site.ID,
			Name: raw.Site.Name,
		},
		zoneByID:     make(map[string]*Zone),
		locationByID: make(map[string]*Location),
		objectByID:   make(map[string]*Object),
		agentByID:    make(map[string]*Agent),
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
		center, err := toVec3(rz.UE5Bounds.Center, "ue5_bounds.center", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		half, err := toVec3(rz.UE5Bounds.HalfSize, "ue5_bounds.half_size", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		z := Zone{
			ID:          rz.ID,
			Name:        rz.Name,
			Description: rz.Description,
			EntryPoint:  entry,
			UE5Bounds: Bounds{
				Center:   center,
				HalfSize: half,
			},
			ConnectedTo: rz.ConnectedTo,
			Locations:   rz.Locations,
		}
		kb.Zones = append(kb.Zones, z)
		kb.zoneByID[z.ID] = &kb.Zones[len(kb.Zones)-1]
	}

	// Locations
	kb.Locations = make([]Location, 0, len(raw.Locations))
	for i, rl := range raw.Locations {
		if rl.ID == "" {
			return nil, fmt.Errorf("location[%d]: missing id", i)
		}
		if _, dup := kb.locationByID[rl.ID]; dup {
			return nil, fmt.Errorf("location[%d]: duplicate id %q", i, rl.ID)
		}
		pos, err := toVec3(rl.Position, "position", rl.ID)
		if err != nil {
			return nil, fmt.Errorf("location %q: %w", rl.ID, err)
		}
		ip, err := toVec3(rl.InteractionPoint, "interaction_point", rl.ID)
		if err != nil {
			return nil, fmt.Errorf("location %q: %w", rl.ID, err)
		}
		facing, err := toVec3(rl.Facing, "facing", rl.ID)
		if err != nil {
			return nil, fmt.Errorf("location %q: %w", rl.ID, err)
		}
		l := Location{
			ID:                rl.ID,
			Name:              rl.Name,
			Zone:              rl.Zone,
			Type:              rl.Type,
			Position:          pos,
			InteractionPoint:  ip,
			InteractionRadius: rl.InteractionRadius,
			Facing:            facing,
			AvailableActions:  rl.AvailableActions,
			UE5Ref:            rl.UE5Ref,
		}
		kb.Locations = append(kb.Locations, l)
		kb.locationByID[l.ID] = &kb.Locations[len(kb.Locations)-1]
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
		kb.Objects = append(kb.Objects, Object{
			ID:               ro.ID,
			AvailableActions: ro.AvailableActions,
			RequiredRole:     ro.RequiredRole,
			Capacity:         ro.Capacity,
			UE5Ref:           ro.UE5Ref,
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
		dp, err := toVec3(ra.DefaultPosition, "default_position", ra.ID)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", ra.ID, err)
		}
		kb.Agents = append(kb.Agents, Agent{
			ID:              ra.ID,
			Name:            ra.Name,
			Type:            ra.Type,
			Role:            ra.Role,
			Capabilities:    ra.Capabilities,
			DefaultZone:     ra.DefaultZone,
			DefaultPosition: dp,
			UE5Class:        ra.UE5Class,
			UE5Variant:      ra.UE5Variant,
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
