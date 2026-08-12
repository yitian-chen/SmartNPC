package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// helper: build a minimal KB with the 6 real objects, matching the
// category→semantic_group mapping observed in production UE5 payloads.
func testKBWithObjects(t *testing.T) *worldkb.KB {
	t.Helper()
	objects := []worldkb.Object{
		{ID: "Charge", DisplayName: "充电桩", Category: "charging", SemanticGroup: "charger", ZoneID: "central_plaza", AvailableInteractions: []string{"charge"}},
		{ID: "Computer", DisplayName: "电脑", Category: "Net", SemanticGroup: "computer", ZoneID: "archive_station", AvailableInteractions: []string{"surf_internet"}},
		{ID: "RepairTable", DisplayName: "修理台", Category: "maintainance", SemanticGroup: "repair_table", ZoneID: "repair_bay", AvailableInteractions: []string{"repair"}},
		{ID: "SleepPod", DisplayName: "睡眠舱", Category: "rest", SemanticGroup: "sleep_pod", ZoneID: "residential_quarters", AvailableInteractions: []string{"sleep"}},
		{ID: "SortingConveyor", DisplayName: "分拣传送带", Category: "work", SemanticGroup: "sorting_conveyor", ZoneID: "logistics_hub", AvailableInteractions: []string{"sort_cargo"}},
		{ID: "WorkBench", DisplayName: "工作台", Category: "work", SemanticGroup: "workbench", ZoneID: "main_workshop", AvailableInteractions: []string{"assemble"}},
	}
	return worldkb.NewKB(nil, objects, nil)
}

func TestObjectStatusContext_EmptyStatusReturnsEmpty(t *testing.T) {
	kb := testKBWithObjects(t)
	if got := ObjectStatusContext(nil, nil, kb); got != "" {
		t.Errorf("nil status should return empty, got: %q", got)
	}
	if got := ObjectStatusContext(map[string]protocol.ObjectCategoryStatus{}, nil, kb); got != "" {
		t.Errorf("empty status should return empty, got: %q", got)
	}
}

func TestObjectStatusContext_NilKBReturnsEmpty(t *testing.T) {
	status := map[string]protocol.ObjectCategoryStatus{"work": {Total: 2, Idle: 1, Occupied: 1}}
	if got := ObjectStatusContext(status, nil, nil); got != "" {
		t.Errorf("nil KB should return empty, got: %q", got)
	}
}

func TestObjectStatusContext_SingleSemanticGroupCategory(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"charging": {Total: 6, Idle: 6, Occupied: 0},
	}
	got := ObjectStatusContext(status, nil, kb)
	if !strings.Contains(got, "【物体实时占用】") {
		t.Errorf("missing section header, got: %s", got)
	}
	if !strings.Contains(got, "- charging（含 charger）：6 个实例，6 空闲 / 0 占用") {
		t.Errorf("missing/incorrect charging line, got: %s", got)
	}
}

func TestObjectStatusContext_MultiSemanticGroupCategory(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"work": {Total: 2, Idle: 1, Occupied: 1},
	}
	got := ObjectStatusContext(status, nil, kb)
	// "work" category contains both workbench and sorting_conveyor.
	// Both should appear in the "含 ..." clause, sorted alphabetically.
	if !strings.Contains(got, "- work（含 sorting_conveyor, workbench）：2 个实例，1 空闲 / 1 占用") {
		t.Errorf("missing/incorrect multi-group work line, got: %s", got)
	}
}

func TestObjectStatusContext_BrokenCountShownWhenNonZero(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"charging": {Total: 6, Idle: 5, Occupied: 0, Broken: 1},
	}
	got := ObjectStatusContext(status, nil, kb)
	if !strings.Contains(got, "/ 1 故障") {
		t.Errorf("missing broken count, got: %s", got)
	}
}

func TestObjectStatusContext_NearbyObjectsDisambiguation(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"work": {Total: 2, Idle: 1, Occupied: 1},
	}
	nearby := []protocol.NearbyObject{
		{ID: "WorkBench", Category: "work", State: "occupied", Distance: 88.0},
	}
	got := ObjectStatusContext(status, nearby, kb)
	if !strings.Contains(got, "你附近的实例状态（当前 zone）：") {
		t.Errorf("missing nearby section header, got: %s", got)
	}
	if !strings.Contains(got, "- WorkBench（semantic_group=workbench）：占用中") {
		t.Errorf("missing/incorrect nearby instance line, got: %s", got)
	}
}

func TestObjectStatusContext_NearbyObjectWithoutKBMatch(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{"work": {Total: 1, Idle: 1}}
	nearby := []protocol.NearbyObject{
		{ID: "UnknownObject", State: "idle"},
	}
	got := ObjectStatusContext(status, nearby, kb)
	// Should still show the instance, just without semantic_group suffix.
	if !strings.Contains(got, "- UnknownObject：空闲") {
		t.Errorf("missing nearby unknown object line, got: %s", got)
	}
}

func TestObjectStatusContext_NearbyObjectEmptyState(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{"work": {Total: 1, Idle: 1}}
	nearby := []protocol.NearbyObject{
		{ID: "WorkBench", State: ""},
	}
	got := ObjectStatusContext(status, nearby, kb)
	if !strings.Contains(got, "- WorkBench（semantic_group=workbench）：未知") {
		t.Errorf("empty state should show 未知, got: %s", got)
	}
}

func TestObjectStatusContext_CategoriesSortedAlphabetically(t *testing.T) {
	kb := testKBWithObjects(t)
	status := map[string]protocol.ObjectCategoryStatus{
		"work":         {Total: 2, Idle: 1, Occupied: 1},
		"charging":     {Total: 6, Idle: 6, Occupied: 0},
		"maintainance": {Total: 1, Idle: 1, Occupied: 0},
	}
	got := ObjectStatusContext(status, nil, kb)
	// Categories should appear in alphabetical order: charging, maintainance, work.
	chargingIdx := strings.Index(got, "- charging")
	maintIdx := strings.Index(got, "- maintainance")
	workIdx := strings.Index(got, "- work")
	if chargingIdx < 0 || maintIdx < 0 || workIdx < 0 {
		t.Fatalf("missing category lines, got: %s", got)
	}
	if !(chargingIdx < maintIdx && maintIdx < workIdx) {
		t.Errorf("categories not alphabetically sorted: charging=%d maintainance=%d work=%d", chargingIdx, maintIdx, workIdx)
	}
}
