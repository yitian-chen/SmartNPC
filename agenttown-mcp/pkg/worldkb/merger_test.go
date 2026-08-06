package worldkb

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ─── test helpers ──────────────────────────────────────────────
// (named with mw prefix to avoid collision with serializer_test.go's contains)
func mwMarshal(v any) ([]byte, error)            { return json.MarshalIndent(v, "", "  ") }
func mwRead(p string) ([]byte, error)            { return os.ReadFile(p) }
func mwStat(p string) (os.FileInfo, error)       { return os.Stat(p) }
func mwContains(s, substr string) bool           { return strings.Contains(s, substr) }
func containsCode(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// minimalGenerated builds a minimal valid generated map with one
// zone/object/agent. Returns map[string]any to match the new MergeMaps API.
func minimalGenerated() map[string]any {
	return map[string]any{
		"$schema":         "agenttown-world-generated/v1",
		"schema_version":  "1.0",
		"generated_at":    "2026-07-31T03:00:00Z",
		"generator":       map[string]any{"name": "test", "version": "0.1"},
		"source":          map[string]any{"map_package": "/Game/Test", "map_name": "Test"},
		"coordinate_system": map[string]any{
			"space": "UE5_world", "distance_unit": "centimeter",
			"rotation_unit": "degree", "rotation_order": "pitch_yaw_roll",
		},
		"zones": []any{
			map[string]any{
				"id":           "main_workshop",
				"editor_label": "Z1",
				"actor_path":   "/p/z1",
				"bounds": map[string]any{
					"center":   []any{1.0, 2.0, 3.0},
					"extent":   []any{4.0, 5.0, 6.0},
					"rotation": []any{0.0, 0.0, 0.0},
				},
				"entry_point":  []any{7.0, 8.0, 9.0},
				"entry_facing": []any{1.0, 0.0, 0.0},
			},
		},
		"objects": []any{
			map[string]any{
				"id":                    "workbench_01",
				"category":              "workbench",
				"zone_id":               "main_workshop",
				"editor_label":          "W1",
				"actor_class":           "/p/w1",
				"actor_position":        []any{1.0, 2.0, 3.0},
				"interaction_point":     []any{4.0, 5.0, 6.0},
				"interaction_facing":    []any{1.0, 0.0, 0.0},
				"available_interactions": []any{"assemble"},
				"default_state":         "idle",
			},
		},
		"agents": []any{
			map[string]any{
				"id":                 "H-01",
				"type":               "humanoid",
				"initial_zone":       "main_workshop",
				"editor_label":       "LaoChen",
				"actor_class":        "/p/lc",
				"initial_position":   []any{1.0, 2.0, 3.0},
				"action_table":       "/Game/DT",
				"main_behavior_tree": "/Game/BT",
			},
		},
		"validation_summary": map[string]any{"errors": 0, "warnings": 0},
	}
}

// minimalAuthored builds a minimal authored map matching the generated above
// (NEW schema). Returns map[string]any to match the new MergeMaps API.
func minimalAuthored() map[string]any {
	return map[string]any{
		"version":   "1.0",
		"narrative": map[string]any{"setting": "小镇", "theme": "测试"},
		"zones": map[string]any{
			"main_workshop": map[string]any{
				"display_name": "主车间",
				"description":  "d",
				"aliases":      []any{"工坊", "工坊"}, // duplicate for dedup test
				"connections": []any{
					map[string]any{"to": "main_workshop", "type": "road", "bidirectional": true}, // self-loop
				},
			},
		},
		"objects": map[string]any{
			"workbench_01": map[string]any{
				"display_name": "工作台",
				"tags":         []any{"crafting"},
			},
		},
		"agents": map[string]any{
			"H-01": map[string]any{
				"display_name": "老陈",
				"description":  "测试 agent",
				"profession":  "worker",
				"personality": map[string]any{
					"traits":      []any{"calm"},
					"speech_style": "concise",
				},
			},
		},
	}
}

func TestMerge_HappyPath(t *testing.T) {
	kb, warnings, err := MergeMaps(minimalGenerated(), minimalAuthored())
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
	if len(kb.Zones[0].Connections) != 1 {
		t.Errorf("Connections not set: %v", kb.Zones[0].Connections)
	}
	if len(kb.Zones[0].Aliases) != 1 {
		t.Errorf("Aliases dedup failed: %v", kb.Zones[0].Aliases)
	}
	if kb.Narrative.Setting != "小镇" {
		t.Errorf("narrative not from authored: %q", kb.Narrative.Setting)
	}
	if len(kb.Objects) != 1 || kb.Objects[0].DisplayName != "工作台" {
		t.Errorf("object overlay failed: %+v", kb.Objects)
	}
	if kb.Objects[0].InteractionRadius != defaultInteractionRadius {
		t.Errorf("default radius not applied: %v", kb.Objects[0].InteractionRadius)
	}
	if len(kb.Objects[0].Tags) != 1 || kb.Objects[0].Tags[0] != "crafting" {
		t.Errorf("object tags not overlayed: %v", kb.Objects[0].Tags)
	}
	if len(kb.Agents) != 1 || kb.Agents[0].DisplayName != "老陈" {
		t.Errorf("agent overlay failed: %+v", kb.Agents)
	}
	if kb.Agents[0].Profession != "worker" {
		t.Errorf("agent profession not overlayed: %q", kb.Agents[0].Profession)
	}
	if len(kb.Agents[0].Personality.Traits) != 1 || kb.Agents[0].Personality.Traits[0] != "calm" {
		t.Errorf("agent personality not overlayed: %v", kb.Agents[0].Personality)
	}
}

func TestMerge_SchemaVersionMismatch(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["version"] = "2.0"
	_, warnings, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("expected no error (warning-only), got: %v", err)
	}
	found := false
	for _, w := range warnings {
		if w.Code == "SCHEMA_VERSION_MISMATCH" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected SCHEMA_VERSION_MISMATCH warning, got warnings: %+v", warnings)
	}
}

func TestMerge_DanglingAuthoredZone(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["zones"].(map[string]any)["ghost_zone"] = map[string]any{"display_name": "Ghost"}
	_, _, err := MergeMaps(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DanglingAuthoredObject(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["objects"].(map[string]any)["ghost_obj"] = map[string]any{"display_name": "Ghost"}
	_, _, err := MergeMaps(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DanglingAuthoredAgent(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["agents"].(map[string]any)["H-99"] = map[string]any{"display_name": "Ghost"}
	_, _, err := MergeMaps(gen, auth)
	if err == nil || !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected dangling id error, got: %v", err)
	}
}

func TestMerge_DuplicateGeneratedZoneID(t *testing.T) {
	gen := minimalGenerated()
	zones := gen["zones"].([]any)
	gen["zones"] = append(zones, zones[0])
	_, _, err := MergeMaps(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("expected duplicate id error, got: %v", err)
	}
}

func TestMerge_MissingGeneratedID(t *testing.T) {
	gen := minimalGenerated()
	gen["zones"].([]any)[0].(map[string]any)["id"] = ""
	_, _, err := MergeMaps(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Fatalf("expected missing id error, got: %v", err)
	}
}

func TestMerge_WrongVectorArity(t *testing.T) {
	gen := minimalGenerated()
	gen["zones"].([]any)[0].(map[string]any)["entry_point"] = []any{1.0, 2.0} // only 2
	_, _, err := MergeMaps(gen, minimalAuthored())
	if err == nil || !strings.Contains(err.Error(), "expected 3 floats") {
		t.Fatalf("expected vector arity error, got: %v", err)
	}
}

func TestMerge_WarningWhenAuthoredMissing(t *testing.T) {
	gen := minimalGenerated()
	auth := map[string]any{
		"version":   "1.0",
		"narrative": map[string]any{"setting": "t"},
		"zones":     map[string]any{},
		"objects":   map[string]any{},
		"agents":    map[string]any{},
	}
	_, warnings, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 3 {
		t.Errorf("expected 3 warnings (zone/object/agent), got %d: %v", len(warnings), warnings)
	}
}

func TestMerge_PerAgentRelationshipsFlattened(t *testing.T) {
	gen := minimalGenerated()
	// Add a second agent so relationship has a valid target.
	gen["agents"] = append(gen["agents"].([]any), map[string]any{
		"id":               "H-02",
		"type":             "humanoid",
		"initial_zone":     "main_workshop",
		"initial_position": []any{1.0, 2.0, 3.0},
	})
	auth := minimalAuthored()
	auth["agents"].(map[string]any)["H-02"] = map[string]any{"display_name": "小李"}
	auth["agents"].(map[string]any)["H-01"] = map[string]any{
		"display_name": "老陈",
		"relationships": []any{
			map[string]any{"from": "H-01", "to": "H-02", "familiarity": 80.0, "type": "colleague"},
		},
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(kb.Relationships) != 1 || kb.Relationships[0].From != "H-01" {
		t.Errorf("per-agent relationship not flattened: %+v", kb.Relationships)
	}
}

func TestMerge_AuthoredInitialZoneOverrides(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	// generated has initial_zone="main_workshop"; authored overrides to a
	// different zone that must exist in generated. Add it first.
	gen["zones"] = append(gen["zones"].([]any), map[string]any{
		"id":           "zone_b",
		"editor_label": "ZB",
		"actor_path":   "/p/zb",
		"bounds": map[string]any{
			"center": []any{0.0, 0.0, 0.0},
			"extent": []any{1.0, 1.0, 1.0},
		},
		"entry_point":  []any{0.0, 0.0, 0.0},
		"entry_facing": []any{1.0, 0.0, 0.0},
	})
	auth["zones"].(map[string]any)["zone_b"] = map[string]any{"display_name": "B"}
	auth["agents"].(map[string]any)["H-01"] = map[string]any{
		"display_name": "老陈",
		"initial_zone": "zone_b",
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := kb.GetAgent("H-01")
	if got.InitialZone != "zone_b" {
		t.Errorf("authored initial_zone not applied: %q", got.InitialZone)
	}
}

func TestMerge_DeterministicSort(t *testing.T) {
	gen := minimalGenerated()
	// Add zones in non-sorted order.
	gen["zones"] = []any{
		map[string]any{
			"id": "z_b",
			"bounds": map[string]any{
				"center": []any{0.0, 0.0, 0.0},
				"extent": []any{1.0, 1.0, 1.0},
			},
			"entry_point":  []any{0.0, 0.0, 0.0},
			"entry_facing": []any{1.0, 0.0, 0.0},
		},
		map[string]any{
			"id": "z_a",
			"bounds": map[string]any{
				"center": []any{0.0, 0.0, 0.0},
				"extent": []any{1.0, 1.0, 1.0},
			},
			"entry_point":  []any{0.0, 0.0, 0.0},
			"entry_facing": []any{1.0, 0.0, 0.0},
		},
	}
	auth := map[string]any{
		"version":   "1.0",
		"narrative": map[string]any{"setting": "t"},
		"zones": map[string]any{
			"z_a": map[string]any{},
			"z_b": map[string]any{},
		},
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kb.Zones[0].ID != "z_a" || kb.Zones[1].ID != "z_b" {
		t.Errorf("zones not sorted: %v %v", kb.Zones[0].ID, kb.Zones[1].ID)
	}
}

// ─── MergeAndWriteBytes (runtime WS-push pipeline) ──────────────

func TestMergeAndWriteBytes_HappyPath(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"
	manifestPath := dir + "/world_kb.manifest.json"

	genBytes, err := mwMarshal(minimalGenerated())
	if err != nil {
		t.Fatalf("marshal generated: %v", err)
	}
	authBytes, err := mwMarshal(minimalAuthored())
	if err != nil {
		t.Fatalf("marshal authored: %v", err)
	}

	kb, _, err := MergeAndWriteBytes(genBytes, authBytes, outPath, manifestPath)
	if err != nil {
		t.Fatalf("MergeAndWriteBytes: %v", err)
	}
	if kb == nil || len(kb.Zones) != 1 || len(kb.Agents) != 1 {
		t.Fatalf("unexpected kb: %+v", kb)
	}
	// Indexes built (Merge tail calls buildIndex).
	if kb.GetAgent("H-01") == nil {
		t.Error("GetAgent returned nil — index not built")
	}
	// YAML re-loadable.
	reloaded, err := Load(outPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Narrative.Setting != "小镇" {
		t.Errorf("narrative.setting = %q, want 小镇", reloaded.Narrative.Setting)
	}
	// Manifest written with sha256 from input bytes.
	manifestBytes, _ := mwRead(manifestPath)
	if !mwContains(string(manifestBytes), "sha256") {
		t.Errorf("manifest missing sha256: %s", manifestBytes)
	}
}

func TestMergeAndWriteBytes_BadJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	// Malformed generated JSON.
	_, _, err := MergeAndWriteBytes([]byte("{not json"), []byte(`{"version":"1.0"}`), outPath, "")
	if err == nil {
		t.Fatal("expected parse error for malformed generated bytes")
	}
	if !mwContains(err.Error(), "parse generated") {
		t.Errorf("error should mention parse generated: %v", err)
	}
	// Output must not be written on parse failure.
	if _, statErr := mwStat(outPath); statErr == nil {
		t.Error("out file should not exist after parse failure")
	}
}

// TestMergeAndWriteBytes_EmptyAuthoredDegradesGracefully 验证 UE 推送
// world_kb 时 authored 字段缺失（nil）或为空白时，MergeAndWriteBytes 降级为
// 空对象 {} 而非报错。复现 stable 端 2026-08-05 日志：UE 不发 authored，
// MCP 报 "parse authored: unexpected end of JSON input"。
func TestMergeAndWriteBytes_EmptyAuthoredDegradesGracefully(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	genBytes, err := mwMarshal(minimalGenerated())
	if err != nil {
		t.Fatalf("marshal generated: %v", err)
	}

	// 三种空 authored 形态都应降级成功，不报 parse authored 错误。
	for _, label := range []string{"nil", "empty", "whitespace-only"} {
		var authBytes []byte
		switch label {
		case "nil":
			authBytes = nil
		case "empty":
			authBytes = []byte("")
		case "whitespace-only":
			authBytes = []byte("  \n  ")
		}
		kb, _, err := MergeAndWriteBytes(genBytes, authBytes, outPath, "")
		if err != nil {
			t.Errorf("[%s] MergeAndWriteBytes returned error: %v; want nil (authored should degrade to {})", label, err)
			continue
		}
		if kb == nil || len(kb.Zones) != 1 || len(kb.Agents) != 1 {
			t.Errorf("[%s] unexpected kb: %+v", label, kb)
		}
	}
}

func TestMergeAndWriteBytes_EmptyManifestSkipped(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	genBytes, _ := mwMarshal(minimalGenerated())
	authBytes, _ := mwMarshal(minimalAuthored())

	if _, _, err := MergeAndWriteBytes(genBytes, authBytes, outPath, ""); err != nil {
		t.Fatalf("MergeAndWriteBytes: %v", err)
	}
	if _, err := Load(outPath); err != nil {
		t.Errorf("reload: %v", err)
	}
}

// ─── normalizeEntityIDs (协议容错：大写 id 小写化) ──────────────

// TestMergeAndWriteBytes_UppercaseObjectIDsLowercased 验证 UE 推送的
// 大写开头 object id（如 "Charge"/"WorkBench"）被 MCP 规范化为小写，
// 不再触发 validator INVALID_ID_FORMAT 错误。复现 stable 端 2026-08-05
// 日志：4 个 object id 全大写开头（zone id 已合规），merge 失败。
func TestMergeAndWriteBytes_UppercaseObjectIDsLowercased(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	gen := minimalGenerated()
	// 用大写开头 object id 复现 stable 端 UE 推送（zone id 已合规不动）
	gen["objects"].([]any)[0].(map[string]any)["id"] = "WorkBench"
	genBytes, err := mwMarshal(gen)
	if err != nil {
		t.Fatalf("marshal generated: %v", err)
	}

	// authored 用大写 key，验证 map key 同步小写化
	auth := minimalAuthored()
	auth["objects"] = map[string]any{
		"WorkBench": map[string]any{"display_name": "工作台"},
	}
	authBytes, err := mwMarshal(auth)
	if err != nil {
		t.Fatalf("marshal authored: %v", err)
	}

	kb, changes, err := MergeAndWriteBytes(genBytes, authBytes, outPath, "")
	if err != nil {
		t.Fatalf("MergeAndWriteBytes should succeed with normalization, got: %v", err)
	}
	// validator 通过 → object id 已小写化
	if got := kb.GetObject("workbench"); got == nil {
		t.Errorf("expected object id %q after normalization, GetObject returned nil", "workbench")
	}
	// zone id 已合规，不受影响
	if got := kb.GetZone("main_workshop"); got == nil {
		t.Errorf("zone id %q should be unaffected", "main_workshop")
	}
	// authored overlay 通过小写 key 应用
	if obj := kb.GetObject("workbench"); obj != nil && obj.DisplayName != "工作台" {
		t.Errorf("authored overlay not applied: DisplayName=%q want %q", obj.DisplayName, "工作台")
	}
	// normalize 报告非空
	if len(changes) == 0 {
		t.Error("expected non-empty normalize changes report, got empty")
	}
	// YAML 写入的也是小写 id
	reloaded, err := Load(outPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.GetObject("workbench") == nil {
		t.Errorf("reloaded YAML missing normalized object id: objects=%v", reloaded.Objects)
	}
}

// TestMergeAndWriteBytes_AgentIDPreserved 验证 agent id（如 "H-01"）
// 的大写不被小写化——agent id 正则允许大写连字符，与 zone/object id 不同。
func TestMergeAndWriteBytes_AgentIDPreserved(t *testing.T) {
	dir := t.TempDir()
	outPath := dir + "/world_kb.yaml"

	genBytes, _ := mwMarshal(minimalGenerated())
	authBytes, _ := mwMarshal(minimalAuthored())

	kb, changes, err := MergeAndWriteBytes(genBytes, authBytes, outPath, "")
	if err != nil {
		t.Fatalf("MergeAndWriteBytes: %v", err)
	}
	// agent id 保持原样
	if got := kb.GetAgent("H-01"); got == nil {
		t.Error("GetAgent(H-01) returned nil — agent id should not be lowercased")
	}
	if got := kb.GetAgent("h-01"); got != nil {
		t.Error("GetAgent(h-01) should return nil — agent id should remain uppercase")
	}
	// 合规 id 不触发 normalize
	if len(changes) != 0 {
		t.Errorf("expected no normalize changes for compliant ids, got: %v", changes)
	}
}
