package worldkb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleYAMLPath resolves the path to the project's sample world_kb.yaml.
// Tests rely on it as a realistic fixture.
func sampleYAMLPath(t *testing.T) string {
	t.Helper()
	// pkg/worldkb/ → ../../../assets/world_kb.yaml
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "assets", "world_kb.yaml"))
	if err != nil {
		t.Fatalf("resolve sample path: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("sample yaml not found at %s: %v", p, err)
	}
	return p
}

func TestLoad_Sample(t *testing.T) {
	kb, err := Load(sampleYAMLPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if kb.Version != "1.0" {
		t.Errorf("Version = %q, want 1.0", kb.Version)
	}
	if kb.Narrative.Setting == "" {
		t.Errorf("Narrative.Setting is empty")
	}
	if len(kb.Zones) != 7 {
		t.Errorf("len(Zones) = %d, want 7", len(kb.Zones))
	}
	if len(kb.Objects) != 4 {
		t.Errorf("len(Objects) = %d, want 4", len(kb.Objects))
	}
	if len(kb.Agents) != 1 {
		t.Errorf("len(Agents) = %d, want 1", len(kb.Agents))
	}

	// Index sanity.
	if kb.GetZone("main_workshop") == nil {
		t.Error("zoneByID missing main_workshop")
	}
	if kb.GetZone("central_plaza") == nil {
		t.Error("zoneByID missing central_plaza")
	}
	if kb.GetZone("repair_bay") == nil {
		t.Error("zoneByID missing repair_bay")
	}
	if kb.GetObject("workbench") == nil {
		t.Error("objectByID missing workbench")
	}
	if kb.GetObject("charge") == nil {
		t.Error("objectByID missing charge")
	}
	if kb.GetAgent("H-01") == nil {
		t.Error("agentByID missing H-01")
	}

	// Coordinates: workbench interaction_point = [7330, 7100, -21140]
	o := kb.GetObject("workbench")
	if o.InteractionPoint != [3]float64{7330, 7100, -21140} {
		t.Errorf("workbench interaction_point = %v, want [7330 7100 -21140]", o.InteractionPoint)
	}
	if o.InteractionRadius != 1500 {
		t.Errorf("workbench interaction_radius = %v, want 1500", o.InteractionRadius)
	}
	if o.ZoneID != "main_workshop" {
		t.Errorf("workbench zone_id = %q, want main_workshop", o.ZoneID)
	}
	if o.DisplayName != "工作台" {
		t.Errorf("workbench display_name = %q", o.DisplayName)
	}

	// main_workshop zone entry_point resolves to a coordinate.
	z := kb.GetZone("main_workshop")
	if z == nil || z.EntryPoint != [3]float64{6210, 5780, -21200} {
		t.Errorf("main_workshop entry_point = %v, want [6210 5780 -21200]", z)
	}
	if z.Bounds.Extent != [3]float64{7500, 7500, 1000} {
		t.Errorf("main_workshop bounds.extent = %v, want [7500 7500 1000]", z.Bounds.Extent)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/to/world_kb.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "read") {
		t.Errorf("error should mention read failure: %v", err)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(p, []byte("zones: [this is not closed"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure: %v", err)
	}
}

func TestLoad_DuplicateZoneID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dup.yaml")
	content := `
version: "1.0"
narrative: {setting: x, theme: y}
zones:
  - id: dup_zone
    entry_point: [0, 0, 0]
    entry_facing: [0, 0, 0]
    bounds: {center: [0,0,0], extent: [1,1,1]}
  - id: dup_zone
    entry_point: [1, 1, 1]
    entry_facing: [0, 0, 0]
    bounds: {center: [0,0,0], extent: [1,1,1]}
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for duplicate zone id")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate: %v", err)
	}
}

func TestLoad_BadVectorArity(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad_vec.yaml")
	content := `
version: "1.0"
narrative: {setting: x, theme: y}
zones:
  - id: z
    entry_point: [0, 0]          # only 2 elements — should fail
    entry_facing: [0, 0, 0]
    bounds: {center: [0,0,0], extent: [1,1,1]}
`
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for bad vector arity")
	}
	if !strings.Contains(err.Error(), "expected 3 floats") {
		t.Errorf("error should mention arity: %v", err)
	}
}
