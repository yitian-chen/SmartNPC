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
	if kb.Site.ID != "industrial_park" {
		t.Errorf("Site.ID = %q", kb.Site.ID)
	}
	if len(kb.Zones) != 3 {
		t.Errorf("len(Zones) = %d, want 3", len(kb.Zones))
	}
	if len(kb.Locations) != 2 {
		t.Errorf("len(Locations) = %d, want 2", len(kb.Locations))
	}
	if len(kb.Objects) != 2 {
		t.Errorf("len(Objects) = %d, want 2", len(kb.Objects))
	}
	if len(kb.Agents) != 1 {
		t.Errorf("len(Agents) = %d, want 1", len(kb.Agents))
	}

	// Index sanity.
	if kb.GetZone("main_workshop") == nil {
		t.Error("zoneByID missing main_workshop")
	}
	if kb.GetLocation("workbench_01") == nil {
		t.Error("locationByID missing workbench_01")
	}
	if kb.GetObject("charging_station_01") == nil {
		t.Error("objectByID missing charging_station_01")
	}
	if kb.GetAgent("H-01") == nil {
		t.Error("agentByID missing H-01")
	}

	// Coordinates: workbench_01 interaction_point = [19500, 10500, 0]
	l := kb.GetLocation("workbench_01")
	if l.InteractionPoint != [3]float64{19500, 10500, 0} {
		t.Errorf("workbench_01 interaction_point = %v, want [19500 10500 0]", l.InteractionPoint)
	}
	if l.InteractionRadius != 1500 {
		t.Errorf("workbench_01 interaction_radius = %v, want 1500", l.InteractionRadius)
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
site: {id: x, name: X}
zones:
  - id: dup_zone
    entry_point: [0, 0, 0]
    ue5_bounds: {center: [0,0,0], half_size: [1,1,1]}
  - id: dup_zone
    entry_point: [1, 1, 1]
    ue5_bounds: {center: [0,0,0], half_size: [1,1,1]}
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
site: {id: x, name: X}
zones:
  - id: z
    entry_point: [0, 0]          # only 2 elements — should fail
    ue5_bounds: {center: [0,0,0], half_size: [1,1,1]}
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
