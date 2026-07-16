package perception

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFormat_Perception(t *testing.T) {
	snap := Snapshot{
		AgentID: "H-01",
		Phase:   "perception",
		Time:    TimeSpec{Day: 1, Hour: 8, Minute: 15, Display: "Day1 08:15"},
		Zone:    "main_workshop",
		Energy:  87.5,
		NearbyObjects: []string{
			"workbench_01 (工作台)",
			"charging_station_01 (充电桩)",
		},
	}
	got := FormatSnapshot(&snap)

	if !strings.Contains(got, "[感知] 上午，时间Day1 08:15。") {
		t.Errorf("missing time line; got:\n%s", got)
	}
	if !strings.Contains(got, "你在main_workshop，电池88") {
		t.Errorf("missing zone/energy line; got:\n%s", got)
	}
	if !strings.Contains(got, "附近有: workbench_01 (工作台), charging_station_01 (充电桩)。") {
		t.Errorf("missing nearby line; got:\n%s", got)
	}
	if !strings.Contains(got, "你现在想做什么？") {
		t.Errorf("missing prompt; got:\n%s", got)
	}
	if !strings.Contains(got, "简短回应") {
		t.Errorf("missing conciseness directive; got:\n%s", got)
	}
}

func TestFormat_DayStart(t *testing.T) {
	snap := Snapshot{
		Phase: "day_start",
		Time:  TimeSpec{Day: 1, Hour: 6, Minute: 0, Display: "Day1 06:00"},
		Zone:  "main_workshop",
		Energy: 100,
	}
	got := FormatSnapshot(&snap)

	if !strings.HasPrefix(got, "[SYSTEM] 新的一天开始了。") {
		t.Errorf("day_start should prepend [SYSTEM] line; got:\n%s", got)
	}
	if !strings.Contains(got, "清晨") {
		t.Errorf("06:00 should be 清晨; got:\n%s", got)
	}
}

func TestFormat_DayEnd(t *testing.T) {
	snap := Snapshot{
		Phase: "day_end",
		Time:  TimeSpec{Day: 1, Hour: 22, Minute: 0, Display: "Day1 22:00"},
		Zone:  "charging_station",
		Energy: 15,
	}
	got := FormatSnapshot(&snap)

	if !strings.Contains(got, "[SYSTEM] 一天工作结束。") {
		t.Errorf("day_end should include [SYSTEM] line; got:\n%s", got)
	}
	if !strings.Contains(got, "请准备充电休息，说说今天的感想。") {
		t.Errorf("day_end should end with rest prompt; got:\n%s", got)
	}
	if strings.Contains(got, "你现在想做什么？") {
		t.Errorf("day_end should not include mid-day prompt; got:\n%s", got)
	}
}

func TestFormat_TimeHints(t *testing.T) {
	cases := []struct {
		hour int
		want string
	}{
		{12, "午间维护时间到了，可以去充电。"},
		{17, "工作快结束了，准备收尾。"},
		{9, ""}, // no hint
	}
	for _, c := range cases {
		snap := Snapshot{
			Phase: "perception",
			Time:  TimeSpec{Hour: c.hour, Display: "Day1 00:00"},
			Zone:  "main_workshop",
		}
		got := FormatSnapshot(&snap)
		if c.want == "" {
			if strings.Contains(got, "午间维护") || strings.Contains(got, "准备收尾") {
				t.Errorf("hour %d: unexpected hint in: %s", c.hour, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("hour %d: missing hint %q; got:\n%s", c.hour, c.want, got)
		}
	}
}

func TestFormat_ScenarioInjection(t *testing.T) {
	snap := Snapshot{
		Phase: "perception",
		Time:  TimeSpec{Hour: 7, Minute: 30, Display: "Day1 07:30"},
		Zone:  "main_workshop",
		Event: "3号传送带发出异常噪音",
	}
	got := FormatSnapshot(&snap)

	if !strings.Contains(got, "[EVENT] 3号传送带发出异常噪音") {
		t.Errorf("missing [EVENT] line; got:\n%s", got)
	}
}

func TestFormat_JSONRoundTrip(t *testing.T) {
	// Verify Format accepts raw JSON (as it would from the WS event.data).
	raw := json.RawMessage(`{
		"agent_id": "H-01",
		"phase": "perception",
		"time": {"day": 1, "hour": 8, "minute": 15, "display": "Day1 08:15"},
		"zone": "main_workshop",
		"energy": 87.5,
		"nearby_objects": ["workbench_01"]
	}`)
	got := Format(raw)
	if got == "" {
		t.Fatal("Format returned empty for valid JSON")
	}
	if !strings.Contains(got, "Day1 08:15") {
		t.Errorf("expected time in output; got:\n%s", got)
	}
}

func TestFormat_InvalidJSON(t *testing.T) {
	if Format(json.RawMessage(`{not json`)) != "" {
		t.Error("expected empty string for invalid JSON")
	}
	if FormatSnapshot(nil) != "" {
		t.Error("expected empty string for nil snapshot")
	}
}

func TestPeriodOf(t *testing.T) {
	cases := []struct {
		hour int
		want string
	}{
		{6, "清晨"}, {7, "上午"}, {11, "上午"},
		{12, "中午"}, {13, "下午"}, {17, "下午"},
		{18, "傍晚"}, {23, "傍晚"}, {0, "深夜"}, {3, "深夜"},
	}
	for _, c := range cases {
		if got := periodOf(c.hour); got != c.want {
			t.Errorf("periodOf(%d) = %q, want %q", c.hour, got, c.want)
		}
	}
}
