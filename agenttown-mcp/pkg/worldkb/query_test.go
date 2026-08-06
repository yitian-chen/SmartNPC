package worldkb

import "testing"

func TestGetPosition_Zone(t *testing.T) {
	kb, err := Load(sampleYAMLPath(t))
	if err != nil {
		t.Fatal(err)
	}
	coord, kind, err := kb.GetPosition("main_workshop")
	if err != nil {
		t.Fatalf("GetPosition: %v", err)
	}
	if kind != "zone" {
		t.Errorf("kind = %q, want zone", kind)
	}
	// main_workshop entry_point = [6210, 5780, -21200]
	want := [3]float64{6210, 5780, -21200}
	if coord != want {
		t.Errorf("coord = %v, want %v", coord, want)
	}
}

func TestGetPosition_Object(t *testing.T) {
	kb, err := Load(sampleYAMLPath(t))
	if err != nil {
		t.Fatal(err)
	}
	coord, kind, err := kb.GetPosition("workbench")
	if err != nil {
		t.Fatalf("GetPosition: %v", err)
	}
	if kind != "object" {
		t.Errorf("kind = %q, want object", kind)
	}
	want := [3]float64{7330, 7100, -21140}
	if coord != want {
		t.Errorf("coord = %v, want %v", coord, want)
	}
}

func TestGetPosition_Unknown(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	_, _, err := kb.GetPosition("nonexistent_id")
	if err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestWhichZone_Hit(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	// main_workshop bounds: center [6210, 5780, -21200], extent [7500, 7500, 1000]
	// (XY AABB: X∈[-1290, 13710], Y∈[-1720, 13280]).
	// Use a point strictly inside main_workshop interior.
	got := kb.WhichZone([3]float64{6210, 5780, -21200})
	if got != "main_workshop" {
		t.Errorf("WhichZone(main_workshop interior) = %q", got)
	}
}

func TestWhichZone_Miss(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	got := kb.WhichZone([3]float64{50000, 50000, 0})
	if got != "" {
		t.Errorf("WhichZone(far away) = %q, want empty", got)
	}
}

func TestWhichObject_Hit(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	// workbench actor_position [7330, 7100, -21140], radius 1500cm
	// A point 500cm away should hit.
	got := kb.WhichObject([3]float64{7830, 7100, -21140})
	if got != "workbench" {
		t.Errorf("WhichObject(near workbench) = %q, want workbench", got)
	}
}

func TestWhichObject_Miss(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	// Far from any object
	got := kb.WhichObject([3]float64{50000, 50000, 0})
	if got != "" {
		t.Errorf("WhichObject(far away) = %q, want empty", got)
	}
}

func TestResolveTarget(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	cases := []struct {
		desc     string
		wantID   string
		wantKind string
	}{
		{"main_workshop", "main_workshop", "zone"},
		{"workbench", "workbench", "object"},
		{"charge", "charge", "object"},
		{"H-01", "H-01", "agent"},
	}
	for _, c := range cases {
		id, kind, err := kb.ResolveTarget(c.desc)
		if err != nil {
			t.Errorf("ResolveTarget(%q): %v", c.desc, err)
			continue
		}
		if id != c.wantID || kind != c.wantKind {
			t.Errorf("ResolveTarget(%q) = (%q, %q), want (%q, %q)",
				c.desc, id, kind, c.wantID, c.wantKind)
		}
	}
}

func TestResolveTarget_Unknown(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	_, _, err := kb.ResolveTarget("ghost_id")
	if err == nil {
		t.Fatal("expected error for unknown target")
	}
}

func TestListZones(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	zones := kb.ListZones()
	if len(zones) == 0 {
		t.Fatal("ListZones returned empty for non-empty KB")
	}
	// Should contain main_workshop with its Chinese display_name.
	found := false
	for _, z := range zones {
		if z.ID == "main_workshop" {
			if z.DisplayName == "" {
				t.Errorf("main_workshop has empty DisplayName")
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListZones missing main_workshop; got %v", zones)
	}
}

func TestListObjects(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	objs := kb.ListObjects()
	if len(objs) == 0 {
		t.Fatal("ListObjects returned empty for non-empty KB")
	}
	// Should contain workbench with native DisplayName and
	// AvailableInteractions including "assemble".
	found := false
	for _, o := range objs {
		if o.ID == "workbench" {
			if o.DisplayName == "" {
				t.Errorf("workbench has empty DisplayName")
			}
			if o.Category != "work" {
				t.Errorf("workbench Category = %q, want \"work\"", o.Category)
			}
			if o.ZoneID != "main_workshop" {
				t.Errorf("workbench ZoneID = %q, want main_workshop", o.ZoneID)
			}
			if len(o.AvailableInteractions) == 0 {
				t.Errorf("workbench has empty AvailableInteractions")
			}
			hasAssemble := false
			for _, a := range o.AvailableInteractions {
				if a == "assemble" {
					hasAssemble = true
					break
				}
			}
			if !hasAssemble {
				t.Errorf("workbench AvailableInteractions missing 'assemble', got %v", o.AvailableInteractions)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListObjects missing workbench; got %v", objs)
	}
}

func TestGetAvailableInteractions(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	acts := kb.GetAvailableInteractions("workbench")
	// workbench has one interaction: assemble
	want := map[string]bool{"assemble": false}
	for _, a := range acts {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for a, found := range want {
		if !found {
			t.Errorf("missing interaction %q in %v", a, acts)
		}
	}
}

func TestNilKB_AllMethodsSafe(t *testing.T) {
	var kb *KB
	if kb.GetZone("x") != nil {
		t.Error("nil GetZone should return nil")
	}
	if kb.GetObject("x") != nil {
		t.Error("nil GetObject should return nil")
	}
	if kb.GetAgent("x") != nil {
		t.Error("nil GetAgent should return nil")
	}
	if _, _, err := kb.GetPosition("x"); err == nil {
		t.Error("nil GetPosition should error")
	}
	if kb.WhichZone([3]float64{}) != "" {
		t.Error("nil WhichZone should return empty")
	}
	if kb.WhichObject([3]float64{}) != "" {
		t.Error("nil WhichObject should return empty")
	}
	if kb.GetAvailableInteractions("x") != nil {
		t.Error("nil GetAvailableInteractions should return nil")
	}
	if _, _, err := kb.ResolveTarget("x"); err == nil {
		t.Error("nil ResolveTarget should error")
	}
	if zones := kb.ListZones(); zones != nil {
		t.Errorf("nil ListZones should return nil, got %v", zones)
	}
	if objs := kb.ListObjects(); objs != nil {
		t.Errorf("nil ListObjects should return nil, got %v", objs)
	}
}
