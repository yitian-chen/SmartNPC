package worldkb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realUE5GeneratedPath / realUE5AuthoredPath point at the project's
// fallback KB fixtures (assets/world.{generated,authored}.json), captured
// from a real UE5 push (stable log 2026-08-06). The generated half carries
// display_name/description and available_interactions as [{name,description}];
// the authored half uses OLD schema (site/connected_to/role/personality[]/
// home_zone/top-level relationships). This is the production payload that
// broke the pre-refactor typed merger. Reading from assets/ avoids
// duplicating the fixtures in testdata/.
func realUE5GeneratedPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "assets", "world.generated.json"))
	if err != nil {
		t.Fatalf("resolve generated fixture: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("generated fixture not found at %s: %v", p, err)
	}
	return p
}

func realUE5AuthoredPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "assets", "world.authored.json"))
	if err != nil {
		t.Fatalf("resolve authored fixture: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("authored fixture not found at %s: %v", p, err)
	}
	return p
}

func loadJSONFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// ─── 1. Real UE5 payload ─────────────────────────────────────────

// TestMergeMaps_RealUE5Payload 验证真实 UE5 推送的 payload 能成功合并。
// 这是重构的核心目标：pre-refactor 的 typed GeneratedDoc/AuthoredDoc 会因
// (1) available_interactions 从 []string 变成 [{name,description}] 而
// json.Unmarshal 失败；(2) authored 用 OLD schema（schema_version/site/
// connected_to/role/personality[]/home_zone）而 AuthoredDoc 只建模 NEW
// schema；(3) generated 现在带 display_name/description 而 Generated* 不
// 建模——三重故障导致整个 merge abort。
func TestMergeMaps_RealUE5Payload(t *testing.T) {
	genBytes := loadJSONFile(t, realUE5GeneratedPath(t))
	authBytes := loadJSONFile(t, realUE5AuthoredPath(t))

	var genMap, authMap map[string]any
	if err := json.Unmarshal(genBytes, &genMap); err != nil {
		t.Fatalf("unmarshal generated: %v", err)
	}
	if err := json.Unmarshal(authBytes, &authMap); err != nil {
		t.Fatalf("unmarshal authored: %v", err)
	}

	kb, warnings, err := MergeMaps(genMap, authMap)
	if err != nil {
		t.Fatalf("MergeMaps failed on real UE5 payload: %v", err)
	}

	// Authored uses OLD schema (schema_version=1.0 vs generated schema_version=1.0
	// → actually equal here, but authored uses schema_version not version, so
	// the warning logic may or may not fire depending on whether version key
	// is present). Either way, no fatal.
	_ = warnings

	if len(kb.Zones) == 0 {
		t.Fatal("expected non-empty zones from real payload")
	}
	if len(kb.Objects) == 0 {
		t.Fatal("expected non-empty objects from real payload")
	}
	if len(kb.Agents) != 5 || kb.GetAgent("H-01") == nil {
		t.Errorf("expected 5 agents with H-01, got: %+v", kb.Agents)
	}

	// display_name from generated survives (not dropped by typed struct).
	for _, z := range kb.Zones {
		if z.DisplayName == "" {
			t.Errorf("zone %q: display_name dropped (should round-trip via projection)", z.ID)
		}
		if z.Description == "" {
			t.Errorf("zone %q: description dropped", z.ID)
		}
	}

	// available_interactions: [{name, description}] → names extracted into
	// typed slice, raw objects stashed in Extra.
	for _, o := range kb.Objects {
		if len(o.AvailableInteractions) == 0 {
			t.Errorf("object %q: AvailableInteractions empty (names not extracted)", o.ID)
		}
		// Descriptions should be in Extra.
		raw, ok := o.Extra["available_interactions"]
		if !ok {
			t.Errorf("object %q: Extra[available_interactions] missing (raw objects should be stashed)", o.ID)
			continue
		}
		arr, ok := raw.([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("object %q: Extra[available_interactions] not a non-empty array: %T", o.ID, raw)
			continue
		}
		// First element should be a {name, description} map.
		first, ok := arr[0].(map[string]any)
		if !ok {
			t.Errorf("object %q: interaction[0] not a map: %T", o.ID, arr[0])
			continue
		}
		if _, ok := first["description"].(string); !ok {
			t.Errorf("object %q: interaction[0].description missing or not string", o.ID)
		}
	}
}

// ─── 2. OLD-authored compat shim ─────────────────────────────────

// TestMergeMaps_OldAuthoredShim 验证 OLD authored schema 字段被 shim
// 转换到 NEW 类型化槽位：connected_to→Connections、role→Profession、
// personality[]→Personality.Traits、home_zone→InitialZone、顶层
// relationships[]→kb.Relationships。
func TestMergeMaps_OldAuthoredShim(t *testing.T) {
	gen := minimalGenerated()
	auth := map[string]any{
		"schema_version": "1.0",
		"site":           map[string]any{"id": "town", "display_name": "小镇", "description": "d"},
		"zones": map[string]any{
			"main_workshop": map[string]any{
				"connected_to": []any{"central_plaza"},
			},
		},
		"objects": map[string]any{},
		"agents": map[string]any{
			"H-01": map[string]any{
				"role":        []any{"supervisor", "worker"},
				"personality": []any{"沉稳", "念旧"},
				"home_zone":   "main_workshop",
			},
		},
		"relationships": []any{
			map[string]any{"from": "H-01", "to": "H-02", "familiarity": 50, "type": "colleague"},
		},
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps: %v", err)
	}

	// connected_to → Connections.
	z := kb.GetZone("main_workshop")
	if z == nil || len(z.Connections) != 1 || z.Connections[0].To != "central_plaza" {
		t.Errorf("connected_to shim failed: z=%+v", z)
	}
	// OLD connected_to should NOT also leak into Extra.
	if _, hasOld := z.Extra["connected_to"]; hasOld {
		t.Errorf("OLD connected_to leaked into Extra (should be consumed by shim)")
	}

	// site → narrative.setting (OLD shim: site as string fallback).
	// Here site is a map — the string-fallback shim won't apply. Narrative
	// stays empty. This is acceptable: OLD site-as-map is a richer form that
	// the shim doesn't promote; it lands in Extra instead.
	// (Test the string-form site shim separately below.)

	// role → Profession (joined with "、").
	a := kb.GetAgent("H-01")
	if a == nil {
		t.Fatal("agent H-01 missing")
	}
	if a.Profession != "supervisor、worker" {
		t.Errorf("role shim failed: Profession=%q want 'supervisor、worker'", a.Profession)
	}
	// personality[] → Personality.Traits.
	if len(a.Personality.Traits) != 2 || a.Personality.Traits[0] != "沉稳" {
		t.Errorf("personality[] shim failed: Traits=%+v", a.Personality.Traits)
	}
	// home_zone → InitialZone (authored override).
	if a.InitialZone != "main_workshop" {
		t.Errorf("home_zone shim failed: InitialZone=%q want main_workshop", a.InitialZone)
	}
	// Consumed OLD keys should NOT also leak into Extra.
	for _, old := range []string{"role", "home_zone"} {
		if _, hasOld := a.Extra[old]; hasOld {
			t.Errorf("OLD %s leaked into Extra (should be consumed by shim)", old)
		}
	}

	// Top-level relationships[] → kb.Relationships.
	if len(kb.Relationships) != 1 || kb.Relationships[0].From != "H-01" || kb.Relationships[0].To != "H-02" {
		t.Errorf("top-level relationships[] not flattened: %+v", kb.Relationships)
	}
}

// TestMergeMaps_OldSiteStringShim 验证 OLD site 为字符串时降级为
// narrative.setting（shim 的字符串形式）。
func TestMergeMaps_OldSiteStringShim(t *testing.T) {
	gen := minimalGenerated()
	auth := map[string]any{
		"schema_version": "1.0",
		"site":           "工业小镇",
		"zones":          map[string]any{}, "objects": map[string]any{}, "agents": map[string]any{},
	}
	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps: %v", err)
	}
	if kb.Narrative.Setting != "工业小镇" {
		t.Errorf("site string shim failed: Narrative.Setting=%q want '工业小镇'", kb.Narrative.Setting)
	}
}

// ─── 3. Extra round-trip ─────────────────────────────────────────

// TestMergeMaps_ExtraRoundTrip 验证 generated 中未建模的字段通过 Extra
// 持久化：merge → WriteYAML → Load → Extra 字段保持。
func TestMergeMaps_ExtraRoundTrip(t *testing.T) {
	gen := minimalGenerated()
	// Inject a future field on a zone that no typed struct models.
	gen["zones"].([]any)[0].(map[string]any)["custom_tag"] = "future_value"
	auth := minimalAuthored()

	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps: %v", err)
	}
	z := kb.GetZone("main_workshop")
	if z == nil {
		t.Fatal("zone missing")
	}
	if got := z.Extra["custom_tag"]; got != "future_value" {
		t.Errorf("Extra[custom_tag] = %v, want 'future_value'", got)
	}

	// Round-trip: WriteYAML → Load → Extra preserved.
	dir := t.TempDir()
	outPath := filepath.Join(dir, "world_kb.yaml")
	if _, err := WriteYAML(kb, outPath); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}
	reloaded, err := Load(outPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rz := reloaded.GetZone("main_workshop")
	if rz == nil {
		t.Fatal("reloaded zone missing")
	}
	if got := rz.Extra["custom_tag"]; got != "future_value" {
		t.Errorf("reloaded Extra[custom_tag] = %v, want 'future_value' (round-trip lost Extra)", got)
	}
}

// ─── 4. Protected-field rejection ────────────────────────────────

// TestMergeMaps_ProtectedFieldsRejected 验证 protected 字段（bounds/
// actor_position 等）即使 authored 试图覆盖也保持 generated 的值。
func TestMergeMaps_ProtectedFieldsRejected(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	// Authored tries to override protected zone bounds + object actor_position.
	auth["zones"].(map[string]any)["main_workshop"] = map[string]any{
		"display_name": "覆盖名",
		"bounds":       map[string]any{"center": []any{999.0, 999.0, 999.0}},
	}
	auth["objects"].(map[string]any)["workbench_01"] = map[string]any{
		"display_name":    "覆盖台",
		"actor_position":  []any{999.0, 999.0, 999.0},
	}

	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps: %v", err)
	}
	z := kb.GetZone("main_workshop")
	if z.Bounds.Center != [3]float64{1, 2, 3} {
		t.Errorf("protected bounds.center overridden by authored: %v", z.Bounds.Center)
	}
	o := kb.GetObject("workbench_01")
	if o.ActorPosition != [3]float64{1, 2, 3} {
		t.Errorf("protected actor_position overridden by authored: %v", o.ActorPosition)
	}
	// Non-protected authored fields still apply.
	if z.DisplayName != "覆盖名" {
		t.Errorf("non-protected display_name not applied: %q", z.DisplayName)
	}
	if o.DisplayName != "覆盖台" {
		t.Errorf("non-protected display_name not applied: %q", o.DisplayName)
	}
}

// ─── 5. Authored-wins scalar conflict ────────────────────────────

// TestMergeMaps_AuthoredWinsScalarConflict 验证 generated 和 authored 都
// 带同一个标量字段时，authored 覆盖 generated。
func TestMergeMaps_AuthoredWinsScalarConflict(t *testing.T) {
	gen := minimalGenerated()
	// generated 也带 display_name（模拟真实 UE5 推送）。
	gen["zones"].([]any)[0].(map[string]any)["display_name"] = "generated_name"
	auth := minimalAuthored()
	auth["zones"].(map[string]any)["main_workshop"] = map[string]any{
		"display_name": "authored_name",
	}

	kb, _, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps: %v", err)
	}
	if got := kb.GetZone("main_workshop").DisplayName; got != "authored_name" {
		t.Errorf("authored should win: DisplayName=%q want 'authored_name'", got)
	}
}

// ─── 6. Dangling authored fatal ──────────────────────────────────

// TestMergeMaps_DanglingAuthoredFatal 验证 authored 引用 generated 不存在
// 的实体 id 仍是 fatal error（用户选择保持严格，不降级为 warning）。
func TestMergeMaps_DanglingAuthoredFatal(t *testing.T) {
	gen := minimalGenerated()
	auth := minimalAuthored()
	auth["zones"].(map[string]any)["ghost_zone"] = map[string]any{"display_name": "Ghost"}
	_, warnings, err := MergeMaps(gen, auth)
	if err == nil {
		t.Fatal("expected fatal error for dangling authored zone, got nil")
	}
	if !strings.Contains(err.Error(), "dangling id") {
		t.Fatalf("expected 'dangling id' in error, got: %v", err)
	}
	// Warnings should not contain a "dangling" downgrade — it's fatal.
	for _, w := range warnings {
		if strings.Contains(strings.ToLower(w.Message), "dangling") && w.Code == "" {
			t.Errorf("dangling authored should be fatal, not a warning: %+v", w)
		}
	}
}

// ─── 7. available_interactions dual shape ────────────────────────

// TestMergeMaps_AvailableInteractionsDualShape 验证 available_interactions
// 两种形态都产生相同的 typed slice：
//   - []string（legacy）：直接赋值
//   - [{name, description}]（new）：提取 name 到 typed slice，原始对象存 Extra
func TestMergeMaps_AvailableInteractionsDualShape(t *testing.T) {
	// []string form.
	gen1 := minimalGenerated()
	gen1["objects"].([]any)[0].(map[string]any)["available_interactions"] = []any{"assemble", "inspect"}
	auth1 := minimalAuthored()
	kb1, _, err := MergeMaps(gen1, auth1)
	if err != nil {
		t.Fatalf("MergeMaps ([]string form): %v", err)
	}
	o1 := kb1.GetObject("workbench_01")
	if len(o1.AvailableInteractions) != 2 || o1.AvailableInteractions[0] != "assemble" {
		t.Errorf("[]string form: interactions = %v", o1.AvailableInteractions)
	}
	// []string form should NOT stash raw in Extra.
	if _, has := o1.Extra["available_interactions"]; has {
		t.Errorf("[]string form should not stash raw in Extra (already typed)")
	}

	// [{name, description}] form.
	gen2 := minimalGenerated()
	gen2["objects"].([]any)[0].(map[string]any)["available_interactions"] = []any{
		map[string]any{"name": "assemble", "description": "装配"},
		map[string]any{"name": "inspect", "description": "检查"},
	}
	auth2 := minimalAuthored()
	kb2, _, err := MergeMaps(gen2, auth2)
	if err != nil {
		t.Fatalf("MergeMaps ([{name,description}] form): %v", err)
	}
	o2 := kb2.GetObject("workbench_01")
	if len(o2.AvailableInteractions) != 2 || o2.AvailableInteractions[0] != "assemble" {
		t.Errorf("[{name,description}] form: interactions = %v", o2.AvailableInteractions)
	}
	// Object-array form SHOULD stash raw in Extra.
	raw, has := o2.Extra["available_interactions"]
	if !has {
		t.Errorf("[{name,description}] form should stash raw objects in Extra")
	} else if arr, ok := raw.([]any); !ok || len(arr) != 2 {
		t.Errorf("[{name,description}] form: Extra raw not 2-element array: %T %v", raw, raw)
	}
}

// ─── 8. Empty authored degrade ───────────────────────────────────

// TestMergeMaps_EmptyAuthoredDegrade 验证空 authored map 降级为仅含
// generated 实体，不报错。
func TestMergeMaps_EmptyAuthoredDegrade(t *testing.T) {
	gen := minimalGenerated()
	auth := map[string]any{}

	kb, warnings, err := MergeMaps(gen, auth)
	if err != nil {
		t.Fatalf("MergeMaps with empty authored: %v", err)
	}
	if len(kb.Zones) != 1 || len(kb.Objects) != 1 || len(kb.Agents) != 1 {
		t.Errorf("expected 1/1/1 entities from generated, got: zones=%d objects=%d agents=%d",
			len(kb.Zones), len(kb.Objects), len(kb.Agents))
	}
	// Empty authored means every generated entity lacks narrative → 3 warnings.
	if len(warnings) != 3 {
		t.Errorf("expected 3 warnings (no authored narrative for zone/object/agent), got %d: %+v", len(warnings), warnings)
	}
}
