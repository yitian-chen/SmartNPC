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
	// main_workshop entry_point = [16000, 10000, 0]
	want := [3]float64{16000, 10000, 0}
	if coord != want {
		t.Errorf("coord = %v, want %v", coord, want)
	}
}

func TestGetPosition_Location(t *testing.T) {
	kb, err := Load(sampleYAMLPath(t))
	if err != nil {
		t.Fatal(err)
	}
	coord, kind, err := kb.GetPosition("workbench_01")
	if err != nil {
		t.Fatalf("GetPosition: %v", err)
	}
	if kind != "location" {
		t.Errorf("kind = %q, want location", kind)
	}
	want := [3]float64{19500, 10500, 0}
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
	// main_workshop bounds: center [20000,10000,0], half_size [5000,5000,5000]
	// So [20000, 10000, 0] is inside.
	got := kb.WhichZone([3]float64{20000, 10000, 0})
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

func TestWhichLocation_Hit(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	// workbench_01 position [20000,10000,0], radius 1500cm
	// A point 1000cm away should hit.
	got := kb.WhichLocation([3]float64{20500, 10000, 0})
	if got != "workbench_01" {
		t.Errorf("WhichLocation(near workbench_01) = %q, want workbench_01", got)
	}
}

func TestWhichLocation_Miss(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	// Far from any location
	got := kb.WhichLocation([3]float64{50000, 50000, 0})
	if got != "" {
		t.Errorf("WhichLocation(far away) = %q, want empty", got)
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
		{"workbench_01", "workbench_01", "location"}, // zone/location checked before object
		{"charging_station_01", "charging_station_01", "location"},
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
	// Should contain main_workshop with its Chinese name.
	found := false
	for _, z := range zones {
		if z.ID == "main_workshop" {
			if z.Name == "" {
				t.Errorf("main_workshop has empty Name")
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListZones missing main_workshop; got %v", zones)
	}
}

func TestListLocations(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	locs := kb.ListLocations()
	if len(locs) == 0 {
		t.Fatal("ListLocations returned empty for non-empty KB")
	}
	found := false
	for _, l := range locs {
		if l.ID == "workbench_01" {
			if l.Name == "" {
				t.Errorf("workbench_01 has empty Name")
			}
			if l.Zone == "" {
				t.Errorf("workbench_01 has empty Zone")
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListLocations missing workbench_01; got %v", locs)
	}
}

func TestListObjects(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	objs := kb.ListObjects()
	if len(objs) == 0 {
		t.Fatal("ListObjects returned empty for non-empty KB")
	}
	// Should contain workbench_01 with Name borrowed from Location and
	// AvailableActions including "assemble".
	found := false
	for _, o := range objs {
		if o.ID == "workbench_01" {
			if o.Name == "" {
				t.Errorf("workbench_01 has empty Name (should be borrowed from Location)")
			}
			if len(o.AvailableActions) == 0 {
				t.Errorf("workbench_01 has empty AvailableActions")
			}
			hasAssemble := false
			for _, a := range o.AvailableActions {
				if a == "assemble" {
					hasAssemble = true
					break
				}
			}
			if !hasAssemble {
				t.Errorf("workbench_01 AvailableActions missing 'assemble', got %v", o.AvailableActions)
			}
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListObjects missing workbench_01; got %v", objs)
	}
}

func TestGetAvailableActions(t *testing.T) {
	kb, _ := Load(sampleYAMLPath(t))
	acts := kb.GetAvailableActions("workbench_01")
	if len(acts) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(acts))
	}
	// Should contain assemble and inspect
	want := map[string]bool{"assemble": false, "inspect": false}
	for _, a := range acts {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for a, found := range want {
		if !found {
			t.Errorf("missing action %q in %v", a, acts)
		}
	}
}

func TestNilKB_AllMethodsSafe(t *testing.T) {
	var kb *KB
	if kb.GetZone("x") != nil {
		t.Error("nil GetZone should return nil")
	}
	if kb.GetLocation("x") != nil {
		t.Error("nil GetLocation should return nil")
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
	if kb.WhichLocation([3]float64{}) != "" {
		t.Error("nil WhichLocation should return empty")
	}
	if kb.GetAvailableActions("x") != nil {
		t.Error("nil GetAvailableActions should return nil")
	}
	if _, _, err := kb.ResolveTarget("x"); err == nil {
		t.Error("nil ResolveTarget should error")
	}
	if zones := kb.ListZones(); zones != nil {
		t.Errorf("nil ListZones should return nil, got %v", zones)
	}
	if locs := kb.ListLocations(); locs != nil {
		t.Errorf("nil ListLocations should return nil, got %v", locs)
	}
}
