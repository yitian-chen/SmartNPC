package worldkb

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// rawKB mirrors world_kb.yaml structure for the first-pass unmarshal.
// Field names use the YAML snake_case keys directly via struct tags.
// Each entity also carries an Extra bag for unknown keys (preserved on
// round-trip so UE-pushed fields not modeled by the typed structs survive).
type rawKB struct {
	Version       string      `yaml:"version"`
	Narrative     rawNarrative `yaml:"narrative"`
	Zones         []rawZone   `yaml:"zones"`
	Objects       []rawObject `yaml:"objects"`
	Agents        []rawAgent  `yaml:"agents"`
	Relationships []rawRel    `yaml:"relationships"`
}

type rawNarrative struct {
	Setting string `yaml:"setting"`
	Theme   string `yaml:"theme"`
}

type rawZone struct {
	ID          string         `yaml:"id"`
	DisplayName string         `yaml:"display_name"`
	Description string         `yaml:"description"`
	Aliases     []string       `yaml:"aliases"`
	Bounds      rawBounds      `yaml:"bounds"`
	EntryPoint  []float64      `yaml:"entry_point"`
	EntryFacing []float64      `yaml:"entry_facing"`
	Connections []rawConnection `yaml:"connections"`
	Extra       map[string]any `yaml:"-"`
}

type rawConnection struct {
	To            string `yaml:"to"`
	Type          string `yaml:"type"`
	Bidirectional bool   `yaml:"bidirectional"`
}

type rawObject struct {
	ID                    string         `yaml:"id"`
	DisplayName           string         `yaml:"display_name"`
	Description           string         `yaml:"description"`
	Category              string         `yaml:"category"`
	SemanticGroup         string         `yaml:"semantic_group"`
	ZoneID                string         `yaml:"zone_id"`
	ActorClass            string         `yaml:"actor_class"`
	ActorPosition         []float64      `yaml:"actor_position"`
	InteractionPoint      []float64      `yaml:"interaction_point"`
	InteractionFacing     []float64      `yaml:"interaction_facing"`
	InteractionRadius     float64        `yaml:"interaction_radius"`
	AvailableInteractions []string       `yaml:"available_interactions"`
	DefaultState          string         `yaml:"default_state"`
	Tags                  []string       `yaml:"tags"`
	Extra                 map[string]any `yaml:"-"`
}

type rawAgent struct {
	ID               string         `yaml:"id"`
	DisplayName      string         `yaml:"display_name"`
	Description      string         `yaml:"description"`
	Type             string         `yaml:"type"`
	Profession       string         `yaml:"profession"`
	Personality      rawPersonality `yaml:"personality"`
	InitialZone      string         `yaml:"initial_zone"`
	InitialPosition  []float64      `yaml:"initial_position"`
	ActorClass       string         `yaml:"actor_class"`
	ActionTable      string         `yaml:"action_table"`
	MainBehaviorTree string         `yaml:"main_behavior_tree"`
	Extra            map[string]any `yaml:"-"`
}

type rawPersonality struct {
	Traits      []string `yaml:"traits"`
	SpeechStyle string   `yaml:"speech_style"`
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
	Extent   []float64 `yaml:"extent"`
	Rotation []float64 `yaml:"rotation"`
}

// ---------------------------------------------------------------------------
// yaml.Node decode with Extra capture
// ---------------------------------------------------------------------------

// decodeEntityWithExtra decodes a mapping node into the known struct fields
// (via node.Decode(known)) and captures any mapping keys not claimed by a
// known yaml tag into *extra. Known yaml tags are derived from the struct's
// field tags via reflection.
func decodeEntityWithExtra(node *yaml.Node, known any, extra *map[string]any) error {
	if node.Kind != yaml.MappingNode {
		return node.Decode(known)
	}
	knownKeys := knownYAMLKeys(known)
	// First pass: decode known fields normally.
	if err := node.Decode(known); err != nil {
		return err
	}
	// Second pass: capture unknown keys.
	for i := 0; i < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		if knownKeys[keyNode.Value] {
			continue
		}
		var val any
		if err := node.Content[i+1].Decode(&val); err != nil {
			return fmt.Errorf("extra key %q: %w", keyNode.Value, err)
		}
		if *extra == nil {
			*extra = map[string]any{}
		}
		(*extra)[keyNode.Value] = val
	}
	return nil
}

// knownYAMLKeys returns the set of yaml tag names declared on a struct's
// exported fields (e.g. `yaml:"display_name"` → "display_name"). Fields
// with `yaml:"-"` or no tag are skipped.
func knownYAMLKeys(structPtr any) map[string]bool {
	t := reflect.TypeOf(structPtr)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		if comma := strings.Index(tag, ","); comma >= 0 {
			name = tag[:comma]
		}
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

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

	// Decode top-level into a yaml.Node so we can capture Extra on each entity.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var raw rawKB
	if doc.Kind == 0 {
		// Empty file — raw stays zero-value.
	} else if err := doc.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	// Re-walk the top-level mapping to find entity arrays and capture Extra
	// per entity. We do this by re-decoding each entity as a yaml.Node then
	// calling decodeEntityWithExtra.
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		decodeEntitiesExtra(&doc, &raw)
	}

	kb := &KB{
		Version: raw.Version,
		Narrative: Narrative{
			Setting: raw.Narrative.Setting,
			Theme:   raw.Narrative.Theme,
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
		rotation, err := toVec3Optional(rz.Bounds.Rotation, "bounds.rotation", rz.ID)
		if err != nil {
			return nil, fmt.Errorf("zone %q: %w", rz.ID, err)
		}
		z := Zone{
			ID:          rz.ID,
			DisplayName: rz.DisplayName,
			Description: rz.Description,
			Aliases:     rz.Aliases,
			EntryPoint:  entry,
			EntryFacing: facing,
			Bounds: Bounds{
				Center:   center,
				Extent:   extent,
				Rotation: rotation,
			},
			Connections: convertRawConnections(rz.Connections),
			Extra:       rz.Extra,
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
		ID:                    ro.ID,
		DisplayName:           ro.DisplayName,
		Description:           ro.Description,
		Category:              ro.Category,
		SemanticGroup:         ro.SemanticGroup,
		ZoneID:                ro.ZoneID,
		ActorClass:            ro.ActorClass,
		ActorPosition:         actorPos,
		InteractionPoint:      ip,
		InteractionFacing:     ifacing,
		InteractionRadius:     radius,
		AvailableInteractions: ro.AvailableInteractions,
		DefaultState:          ro.DefaultState,
		Tags:                  ro.Tags,
		Extra:                 ro.Extra,
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
			Description:      ra.Description,
			Type:             ra.Type,
			Profession:       ra.Profession,
			Personality: Personality{
				Traits:      ra.Personality.Traits,
				SpeechStyle: ra.Personality.SpeechStyle,
			},
			InitialZone:      ra.InitialZone,
			InitialPosition:  ip,
			ActorClass:       ra.ActorClass,
			ActionTable:      ra.ActionTable,
			MainBehaviorTree: ra.MainBehaviorTree,
			Extra:            ra.Extra,
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

// decodeEntitiesExtra walks the top-level document node to find the zones/
// objects/agents arrays and re-decodes each entity with Extra capture. This
// is the second pass that fills the Extra fields on rawZone/rawObject/rawAgent
// (the first pass via doc.Decode(&raw) left them zero).
func decodeEntitiesExtra(doc *yaml.Node, raw *rawKB) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i].Value
		val := root.Content[i+1]
		switch key {
		case "zones":
			decodeEntityArrayExtra(val, &raw.Zones, func(i int) any { return &raw.Zones[i] })
		case "objects":
			decodeEntityArrayExtra(val, &raw.Objects, func(i int) any { return &raw.Objects[i] })
		case "agents":
			decodeEntityArrayExtra(val, &raw.Agents, func(i int) any { return &raw.Agents[i] })
		}
	}
}

// decodeEntityArrayExtra iterates a sequence node and decodes each entry
// into the corresponding raw entity (with Extra capture). ptrAt returns a
// pointer to the i-th element so we can decode in-place.
func decodeEntityArrayExtra(seqNode *yaml.Node, sliceAddr any, ptrAt func(int) any) {
	if seqNode == nil || seqNode.Kind != yaml.SequenceNode {
		return
	}
	n := len(seqNode.Content)
	for i := 0; i < n; i++ {
		// Grow the slice if needed (shouldn't be needed since first-pass
		// Decode already sized it, but defensive).
		growEntitySlice(sliceAddr, i+1)
		ptr := ptrAt(i)
		// Clear the Extra slot so decodeEntityWithExtra can re-fill cleanly.
		// (The first-pass Decode left Extra as nil since it has yaml:"-".)
		var extra map[string]any
		if err := decodeEntityWithExtra(seqNode.Content[i], ptr, &extra); err != nil {
			// Best-effort: if extra capture fails, the first-pass decode
			// already populated known fields — keep them and skip extra.
			continue
		}
		// Stash Extra back into the struct via reflection (since yaml:"-" means
		// Decode didn't touch it).
		setExtraField(ptr, extra)
	}
}

// growEntitySlice ensures the slice backing sliceAddr has length >= n.
// sliceAddr must be a pointer to a slice.
func growEntitySlice(sliceAddr any, n int) {
	v := reflect.ValueOf(sliceAddr).Elem()
	if v.Len() >= n {
		return
	}
	if v.Cap() < n {
		newSlice := reflect.MakeSlice(v.Type(), n, n)
		reflect.Copy(newSlice, v)
		v.Set(newSlice)
		return
	}
	v.SetLen(n)
}

// setExtraField sets the Extra field (which has yaml:"-") on a raw entity
// pointer via reflection.
func setExtraField(structPtr any, extra map[string]any) {
	v := reflect.ValueOf(structPtr).Elem()
	field := v.FieldByName("Extra")
	if !field.IsValid() || !field.CanSet() {
		return
	}
	field.Set(reflect.ValueOf(extra))
}

// buildIndex populates the zoneByID/objectByID/agentByID lookup maps from
// the Zones/Objects/Agents slices. It must be called after any mutation
// that re-slices or re-orders these entities (e.g. the deterministic sort
// in Merge). Pointers in the maps reference the slice backing array, so
// callers must not re-assign the slices after indexing.
func (k *KB) buildIndex() {
	k.zoneByID = make(map[string]*Zone, len(k.Zones))
	for i := range k.Zones {
		k.zoneByID[k.Zones[i].ID] = &k.Zones[i]
	}
	k.objectByID = make(map[string]*Object, len(k.Objects))
	for i := range k.Objects {
		k.objectByID[k.Objects[i].ID] = &k.Objects[i]
	}
	k.agentByID = make(map[string]*Agent, len(k.Agents))
	for i := range k.Agents {
		k.agentByID[k.Agents[i].ID] = &k.Agents[i]
	}
}

// NewKB constructs a KB from the given slices and builds the lookup indexes.
// Intended for tests and callers that need an in-memory KB without loading
// from disk. The slices are stored by reference; do not mutate them after
// construction.
func NewKB(zones []Zone, objects []Object, agents []Agent) *KB {
	kb := &KB{
		Zones:  zones,
		Objects: objects,
		Agents: agents,
	}
	kb.buildIndex()
	return kb
}

// convertRawConnections converts YAML-loaded rawConnection into KB Connection.
func convertRawConnections(conns []rawConnection) []Connection {
	if len(conns) == 0 {
		return nil
	}
	out := make([]Connection, 0, len(conns))
	for _, c := range conns {
		out = append(out, Connection{
			To:            c.To,
			Type:          c.Type,
			Bidirectional: c.Bidirectional,
		})
	}
	return out
}

// toVec3 converts a YAML list of 3 numbers to a [3]float64. Returns an
// error identifying the field and owning entity if the arity is wrong.
func toVec3(v []float64, field, owner string) ([3]float64, error) {
	if len(v) != 3 {
		return [3]float64{}, fmt.Errorf("%s of %s: expected 3 floats, got %d", field, owner, len(v))
	}
	return [3]float64{v[0], v[1], v[2]}, nil
}

// toVec3Optional behaves like toVec3 but treats an empty/nil slice as [0,0,0]
// rather than an error. Used for optional fields like bounds.rotation.
func toVec3Optional(v []float64, field, owner string) ([3]float64, error) {
	if len(v) == 0 {
		return [3]float64{}, nil
	}
	return toVec3(v, field, owner)
}
