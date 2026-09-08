package worldkb

import "testing"

func TestValidate_HappyPath(t *testing.T) {
	kb, _, err := MergeMaps(minimalGenerated(), minimalAuthored())
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if set.HasErrors() {
		t.Errorf("expected no errors, got: %+v", set.Errors)
	}
}

func TestValidate_InvalidZoneIDFormat(t *testing.T) {
	gen := minimalGenerated()
	gen["zones"].([]any)[0].(map[string]any)["id"] = "Bad-ID" // uppercase + hyphen not allowed for zone
	auth := minimalAuthored()
	auth["zones"] = map[string]any{"Bad-ID": map[string]any{}}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if !set.HasErrors() {
		t.Errorf("expected INVALID_ID_FORMAT error")
	}
	if !containsCode(set.Errors, "INVALID_ID_FORMAT") {
		t.Errorf("expected INVALID_ID_FORMAT, got: %+v", set.Errors)
	}
}

func TestValidate_InvalidAgentID_AllowsHyphen(t *testing.T) {
	// H-01 is valid for agent (allows hyphen + uppercase).
	gen := minimalGenerated()
	auth := minimalAuthored()
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	for _, e := range set.Errors {
		if e.Code == "INVALID_AGENT_ID_FORMAT" && e.Entity == "H-01" {
			t.Errorf("H-01 should be valid agent id, got error: %+v", e)
		}
	}
}

func TestValidate_ObjectZoneRefInvalid(t *testing.T) {
	gen := minimalGenerated()
	gen["objects"].([]any)[0].(map[string]any)["zone_id"] = "nonexistent_zone"
	auth := minimalAuthored()
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if !containsCode(set.Errors, "OBJECT_ZONE_REF_INVALID") {
		t.Errorf("expected OBJECT_ZONE_REF_INVALID, got: %+v", set.Errors)
	}
}

func TestValidate_AgentInitialZoneRefInvalid(t *testing.T) {
	gen := minimalGenerated()
	gen["agents"].([]any)[0].(map[string]any)["initial_zone"] = "nonexistent_zone"
	auth := minimalAuthored()
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if !containsCode(set.Errors, "AGENT_INITIAL_ZONE_REF_INVALID") {
		t.Errorf("expected AGENT_INITIAL_ZONE_REF_INVALID, got: %+v", set.Errors)
	}
}

func TestValidate_ZoneConnectionRefInvalid(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["zones"].(map[string]any)["main_workshop"] = map[string]any{
		"display_name": "x",
		"connections": []any{
			map[string]any{"to": "nonexistent_zone", "type": "road"},
		},
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if !containsCode(set.Errors, "ZONE_CONNECTION_REF_INVALID") {
		t.Errorf("expected ZONE_CONNECTION_REF_INVALID, got: %+v", set.Errors)
	}
}

func TestValidate_EmptyObjectCategory_Warning(t *testing.T) {
	gen := minimalGenerated()
	gen["objects"].([]any)[0].(map[string]any)["category"] = ""
	auth := minimalAuthored()
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	set := Validate(kb)
	if set.HasErrors() {
		t.Errorf("expected no errors, got: %+v", set.Errors)
	}
	if !containsCode(set.Warnings, "EMPTY_OBJECT_CATEGORY") {
		t.Errorf("expected EMPTY_OBJECT_CATEGORY warning, got: %+v", set.Warnings)
	}
}

func TestValidate_NilKB(t *testing.T) {
	set := Validate(nil)
	if !set.HasErrors() {
		t.Errorf("expected NIL_KB error")
	}
}
