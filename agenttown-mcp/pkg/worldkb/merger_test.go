package worldkb

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ─── test helpers for MergeAndWrite ─────────────────────────────
// (named with mw prefix to avoid collision with serializer_test.go's contains)
func mwMarshal(v any) ([]byte, error)                { return json.MarshalIndent(v, "", "  ") }
func mwWrite(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }
func mwRead(p string) ([]byte, error)                { return os.ReadFile(p) }
func mwStat(p string) (os.FileInfo, error)           { return os.Stat(p) }
func mwContains(s, substr string) bool               { return strings.Contains(s, substr) }

// helper: build a minimal valid generated doc with one zone/object/agent.
func minimalGenerated() *GeneratedDoc {
	return &GeneratedDoc{
		Schema:        "agenttown-world-generated/v1",
		SchemaVersion: "1.0",
		GeneratedAt:   "2026-07-31T03:00:00Z",
		Generator:     GeneratedGenerator{Name: "test", Version: "0.1"},
		Source:        GeneratedSource{MapPackage: "/Game/Test", MapName: "Test"},
		CoordinateSystem: GeneratedCoord{
			Space: "UE5_world", DistanceUnit: "centimeter",
			RotationUnit: "degree", RotationOrder: "pitch_yaw_roll",
		},
		Zones: []GeneratedZone{
			{
				ID: "main_workshop", EditorLabel: "Z1", ActorPath: "/p/z1",
				Bounds:      GeneratedBounds{Center: []float64{1, 2, 3}, Extent: []float64{4, 5, 6}},
				EntryPoint:  []float64{7, 8, 9},
				EntryFacing: []float64{1, 0, 0},
			},
		},
		Objects: []GeneratedObject{
			{
				ID: "workbench_01", Category: "workbench", ZoneID: "main_workshop",
				EditorLabel:       "W1", ActorClass: "/p/w1",
				ActorPosition:     []float64{1, 2, 3},
				InteractionPoint:  []float64{4, 5, 6},
				InteractionFacing: []float64{1, 0, 0},
				AvailableActions:  []string{"assemble"},
				DefaultState:      "idle",
			},
		},
		Agents: []GeneratedAgent{
			{
				ID: "H-01", Type: "humanoid", InitialZone: "main_workshop",
				EditorLabel: "LaoChen", ActorClass: "/p/lc",
				InitialPosition: []float64{1, 2, 3},
				ActionTable:     "/Game/DT", MainBehaviorTree: "/Game/BT",
			},
		},
	}
}

// helper: build a minimal authored doc matching the generated above.
func minimalAuthored() *AuthoredDoc {
	return &AuthoredDoc{
		Schema:        "agenttown-world-authored/v1",
		SchemaVersion: "1.0",
		Site:          AuthoredSite{ID: "town", DisplayName: "小镇", Description: "d"},
		Zones: map[string]AuthoredZone{
			"main_workshop": {
				DisplayName: "主车间", Description: "d",
				ConnectedTo: []string{"main_workshop"}, // self-loop for dedup test
			},
		},
		Objects: map[string]AuthoredObject{
			"workbench_01": {DisplayName: "工作台", RequiredRoles: []string{"worker"}, Capacity: 1},
		},
		Agents: map[string]AuthoredAgent{
			"H-01": {DisplayName: "老陈", Role: []string{"worker"}, HomeZone: "main_workshop"},
		},
	}
}

func TestMerge_HappyPath(t *testing.T) {
	kb, warnings, err := Merge(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings, got %d: %v", len(warnings), warnings)
	}
	if len(kb.Zones) != 1 || kb.Zones[0].ID != "main_workshop" {
		t.Errorf("zone mismatch: %+v", kb.Zones)
	}
	if kb.Zones[0].DisplayName != "主车间" {
		t.Errorf("DisplayName not overlayed: %q", kb.Zones[0].DisplayName)
	}
	if len(kb.Zones[0].ConnectedTo) != 1 {
		t.Errorf("ConnectedTo dedup failed: %v", kb.Zones[0].ConnectedTo)
	}
	if kb.Site.ID != "town" {
		t.Errorf("site not from authored: %q", kb.Site.ID)
	}
	if len(kb.Objects) != 1 || kb.Objects[0].DisplayName != "工作台" {
		t.Errorf("object overlay failed: %+v", kb.Objects)
	}
	if kb.Objects[0].InteractionRadius != defaultInteractionRadius {
		t.Errorf("default radius not applied: %v", kb.Objects[0].InteractionRadius)
	}
	if len(kb.Agents) != 1 || kb.Agents[0].DisplayName != "老陈" {
		t.Errorf("agent overlay failed: %+v", kb.Agents)
	}
}

func TestMerge_SchemaVersionMismatch(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.SchemaVersion = "2.0"
	_, _, err := Merge(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "schema version mismatch") {
		t.Fatalf("expected version mismatch error, got: %v", err)
	}
}

func TestMerge_DanglingAuthoredZone(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.Zones["ghost_zone"] = AuthoredZone{DisplayName: "Ghost"}
	_, _, err := Merge(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DanglingAuthoredObject(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.Objects["ghost_obj"] = AuthoredObject{DisplayName: "Ghost"}
	_, _, err := Merge(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DanglingAuthoredAgent(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.Agents["H-99"] = AuthoredAgent{DisplayName: "Ghost"}
	_, _, err := Merge(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DuplicateGeneratedZoneID(t *testing.T) {
	gen := minimalGenerated()
	gen.Zones = append(gen.Zones, gen.Zones[0])
	_, _, err := Merge(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("expected duplicate id error, got: %v", err)
	}
}

func TestMerge_MissingGeneratedID(t *testing.T) {
	gen := minimalGenerated()
	gen.Zones[0].ID = ""
	_, _, err := Merge(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Fatalf("expected missing id error, got: %v", err)
	}
}

func TestMerge_WrongVectorArity(t *testing.T) {
	gen := minimalGenerated()
	gen.Zones[0].EntryPoint = []float64{1, 2} // only 2
	_, _, err := Merge(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "expected 3 floats") {
		t.Fatalf("expected vector arity error, got: %v", err)
	}
}

func TestMerge_WarningWhenAuthoredMissing(t *testing.T) {
	gen := minimalGenerated()
	auth := &AuthoredDoc{
		Schema: "x", SchemaVersion: "1.0",
		Site: AuthoredSite{ID: "town", DisplayName: "t"},
		Zones:   map[string]AuthoredZone{},
		Objects: map[string]AuthoredObject{},
		Agents:  map[string]AuthoredAgent{},
	}
	_, warnings, err := Merge(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 3 {
		t.Errorf("expected 3 warnings (zone/object/agent), got %d: %v", len(warnings), warnings)
	}
}

func TestMerge_AuthoredInteractionRadiusOverrides(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.Objects["workbench_01"] = AuthoredObject{
		DisplayName: "工作台",
		InteractionRadius: 999.0,
	}
	kb, _, err := Merge(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb.Objects[0].InteractionRadius != 999.0 {
		t.Errorf("authored radius not applied: %v", kb.Objects[0].InteractionRadius)
	}
}

func TestMerge_Relationships(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth.Agents["H-01"] = AuthoredAgent{DisplayName: "老陈", HomeZone: "main_workshop"}
	// Add a second agent so relationship has a valid target.
	gen.Agents = append(gen.Agents, GeneratedAgent{
		ID: "H-02", Type: "humanoid", InitialZone: "main_workshop",
		InitialPosition: []float64{1, 2, 3},
	})
	auth.Agents["H-02"] = AuthoredAgent{DisplayName: "小李"}
	auth.Relationships = []AuthoredRelationship{
		{From: "H-01", To: "H-02", Familiarity: 80, Type: "colleague"},
	}
	kb, _, err := Merge(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kb.Relationships) != 1 || kb.Relationships[0].From != "H-01" {
		t.Errorf("relationship not merged: %+v", kb.Relationships)
	}
}

func TestMerge_DeterministicSort(t *testing.T) {
	gen := minimalGenerated()
	// Add zones in non-sorted order.
	gen.Zones = []GeneratedZone{
		{ID: "z_b", Bounds: GeneratedBounds{Center: []float64{0, 0, 0}, Extent: []float64{1, 1, 1}},
			EntryPoint: []float64{0, 0, 0}, EntryFacing: []float64{1, 0, 0}},
		{ID: "z_a", Bounds: GeneratedBounds{Center: []float64{0, 0, 0}, Extent: []float64{1, 1, 1}},
			EntryPoint: []float64{0, 0, 0}, EntryFacing: []float64{1, 0, 0}},
	}
	auth := &AuthoredDoc{
		SchemaVersion: "1.0", Site: AuthoredSite{ID: "t"},
		Zones: map[string]AuthoredZone{"z_a": {}, "z_b": {}},
	}
	kb, _, err := Merge(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb.Zones[0].ID != "z_a" || kb.Zones[1].ID != "z_b" {
		t.Errorf("zones not sorted: %v %v", kb.Zones[0].ID, kb.Zones[1].ID)
	}
}

// ─── MergeAndWrite (one-shot pipeline) ──────────────────────────

// writeJSONFile helper for MergeAndWrite tests: marshals v to JSON and
// writes to path.
func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := mwMarshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := mwWrite(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMergeAndWrite_HappyPath(t *testing.T) {
	dir := t.TempDir()
	genPath := dir + "/world.generated.json"
	authPath := dir + "/world.authored.json"
	outPath := dir + "/world_kb.yaml"
	manifestPath := dir + "/world_kb.manifest.json"

	writeJSONFile(t, genPath, minimalGenerated())
	writeJSONFile(t, authPath, minimalAuthored())

	kb, err := MergeAndWrite(genPath, authPath, outPath, manifestPath)
	if err != nil {
		t.Fatalf("MergeAndWrite: %v", err)
	}
	if kb == nil || len(kb.Zones) != 1 || len(kb.Objects) != 1 || len(kb.Agents) != 1 {
		t.Fatalf("unexpected kb: %+v", kb)
	}
	// YAML file should exist and be re-loadable.
	reloaded, err := Load(outPath)
	if err != nil {
		t.Fatalf("reload merged yaml: %v", err)
	}
	if reloaded.Site.ID != "town" {
		t.Errorf("reloaded site id = %q, want town", reloaded.Site.ID)
	}
	if reloaded.GetZone("main_workshop") == nil {
		t.Error("reloaded missing main_workshop zone")
	}
	// Manifest should exist and be non-empty.
	manifestBytes, err := mwRead(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(manifestBytes) == 0 || !mwContains(string(manifestBytes), "sha256") {
		t.Errorf("manifest missing sha256 field: %s", manifestBytes)
	}
}

func TestMergeAndWrite_EmptyManifestSkipped(t *testing.T) {
	dir := t.TempDir()
	genPath := dir + "/world.generated.json"
	authPath := dir + "/world.authored.json"
	outPath := dir + "/world_kb.yaml"

	writeJSONFile(t, genPath, minimalGenerated())
	writeJSONFile(t, authPath, minimalAuthored())

	_, err := MergeAndWrite(genPath, authPath, outPath, "")
	if err != nil {
		t.Fatalf("MergeAndWrite: %v", err)
	}
	// YAML should still be written.
	if _, err := Load(outPath); err != nil {
		t.Errorf("reload: %v", err)
	}
}

func TestMergeAndWrite_MissingGeneratedFile(t *testing.T) {
	dir := t.TempDir()
	authPath := dir + "/world.authored.json"
	writeJSONFile(t, authPath, minimalAuthored())

	_, err := MergeAndWrite(dir+"/nonexistent.json", authPath, dir+"/out.yaml", "")
	if err == nil {
		t.Fatal("expected error for missing generated file")
	}
	if !mwContains(err.Error(), "load generated") {
		t.Errorf("error should mention load generated: %v", err)
	}
}

func TestMergeAndWrite_MergeErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	genPath := dir + "/world.generated.json"
	authPath := dir + "/world.authored.json"
	outPath := dir + "/world_kb.yaml"

	// Schema version mismatch → merge error.
	gen := minimalGenerated()
	gen.SchemaVersion = "9.9"
	writeJSONFile(t, genPath, gen)
	writeJSONFile(t, authPath, minimalAuthored())

	_, err := MergeAndWrite(genPath, authPath, outPath, "")
	if err == nil {
		t.Fatal("expected merge error for schema mismatch")
	}
	if !mwContains(err.Error(), "merge:") {
		t.Errorf("error should mention merge: %v", err)
	}
	// Output file should NOT exist (merge failed before write).
	if _, err := mwStat(outPath); err == nil {
		t.Error("out file should not exist after merge failure")
	}
}

func TestMergeAndWrite_ValidationErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	genPath := dir + "/world.generated.json"
	authPath := dir + "/world.authored.json"
	outPath := dir + "/world_kb.yaml"

	// Build a generated doc with an invalid agent ID (starts with digit,
	// violates ^[A-Za-z]...) to trigger validator error.
	gen := minimalGenerated()
	gen.Agents[0].ID = "1bad"
	writeJSONFile(t, genPath, gen)
	// Authored must match — also use 1bad so no dangling-id error.
	auth := minimalAuthored()
	delete(auth.Agents, "H-01")
	auth.Agents["1bad"] = AuthoredAgent{DisplayName: "x", HomeZone: "main_workshop"}
	writeJSONFile(t, authPath, auth)

	_, err := MergeAndWrite(genPath, authPath, outPath, "")
	if err == nil {
		t.Fatal("expected validation error for invalid agent ID")
	}
	if !mwContains(err.Error(), "validate:") {
		t.Errorf("error should mention validate: %v", err)
	}
}
