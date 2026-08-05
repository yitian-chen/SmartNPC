package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// TestBuildReactivePrompt_Defaults verifies the prompt template fills all
// placeholders and contains the expected Chinese keywords.
func TestBuildReactivePrompt_Defaults(t *testing.T) {
	in := ReactiveInput{
		AgentID:       "H-01",
		TimeOfDay:     "14:30",
		Zone:          "main_workshop",
		Energy:        45,
		Fatigue:       30,
		Health:        90,
		CurrentAction: "WorkAtWorkbench(target_object_id=workbench_01, duration_sec=3600)",
		ElapsedSec:    120,
		ActionSrc:     "tactical",
		CurrentSlot:   "14:00-18:00",
		DailyPlan:     "14:00-18:00 工作组装",
		Trigger:       TriggerZoneChange,
		TriggerDetail: "zone rest_area→main_workshop",
	}
	prompt := buildReactivePrompt(in)
	for _, want := range []string{
		"14:30", "main_workshop", "45", "30", "90",
		"WorkAtWorkbench(target_object_id=workbench_01, duration_sec=3600)",
		"tactical", "14:00-18:00", "14:00-18:00 工作组装",
		"zone rest_area→main_workshop",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\nFull prompt:\n%s", want, prompt)
		}
	}
}

// TestBuildReactivePrompt_NoCurrentAction verifies that empty CurrentAction
// renders as "无在途动作".
func TestBuildReactivePrompt_NoCurrentAction(t *testing.T) {
	in := ReactiveInput{
		AgentID:       "H-01",
		TimeOfDay:     "14:30",
		Zone:          "main_workshop",
		Energy:        45,
		CurrentAction: "",
		Trigger:       TriggerEventNotify,
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "无在途动作") {
		t.Errorf("empty CurrentAction should render as 无在途动作, got:\n%s", prompt)
	}
}

// TestBuildReactivePrompt_EmptyContext verifies that empty tactical context
// fields render fallback strings without breaking the template.
func TestBuildReactivePrompt_EmptyContext(t *testing.T) {
	in := ReactiveInput{
		AgentID:   "H-01",
		TimeOfDay: "14:30",
		Zone:      "main_workshop",
		Trigger:   TriggerPeriodic,
	}
	prompt := buildReactivePrompt(in)
	for _, want := range []string{"无在途动作", "未分解", "（未生成）", "periodic"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("empty context should render %q, got:\n%s", want, prompt)
		}
	}
}

// TestBuildReactivePrompt_EmptyTriggerDetail verifies that empty detail
// falls back to the trigger enum string.
func TestBuildReactivePrompt_EmptyTriggerDetail(t *testing.T) {
	in := ReactiveInput{
		AgentID:       "H-01",
		TimeOfDay:     "14:30",
		Zone:          "main_workshop",
		Trigger:       TriggerEventNotify,
		TriggerDetail: "",
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "event_notify") {
		t.Errorf("empty detail should fall back to trigger enum, got:\n%s", prompt)
	}
}

// TestBuildReactivePrompt_PhysicalAlertHardRule 验证 prompt 明确禁止
// 物理告警时输出 continue/observe（Fix C 的 prompt 强化部分）。
func TestBuildReactivePrompt_PhysicalAlertHardRule(t *testing.T) {
	in := ReactiveInput{
		AgentID:   "H-01",
		TimeOfDay: "14:30",
		Zone:      "main_workshop",
		Energy:    45,
		Fatigue:   70,
		Health:    90,
		Trigger:   TriggerPeriodic,
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "必须输出 replan") {
		t.Errorf("prompt should contain hard rule for physical alert, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "禁止输出 continue/observe") {
		t.Errorf("prompt should explicitly forbid continue/observe under alert, got:\n%s", prompt)
	}
}

// TestParseReactiveDecision_Continue verifies a clean continue decision.
func TestParseReactiveDecision_Continue(t *testing.T) {
	raw := `{"reaction":"continue","reason":"无需打断"}`
	dec := parseReactiveDecision(raw)
	if dec.Reaction != ReactionContinue {
		t.Errorf("reaction: got %q, want continue", dec.Reaction)
	}
	if dec.Reason != "无需打断" {
		t.Errorf("reason: got %q, want 无需打断", dec.Reason)
	}
}

// TestParseReactiveDecision_MalformedJSON verifies that malformed input
// falls back to continue.
func TestParseReactiveDecision_MalformedJSON(t *testing.T) {
	raw := `这不是 JSON`
	dec := parseReactiveDecision(raw)
	if dec.Reaction != ReactionContinue {
		t.Errorf("reaction: got %q, want continue (parse fail fallback)", dec.Reaction)
	}
	if !strings.Contains(dec.Reason, "parse_failed") {
		t.Errorf("reason should mention parse_failed: %q", dec.Reason)
	}
}

// TestParseReactiveDecision_UnknownReaction verifies that a reaction field
// outside the enum falls back to continue.
func TestParseReactiveDecision_UnknownReaction(t *testing.T) {
	raw := `{"reaction":"dance","reason":"x"}`
	dec := parseReactiveDecision(raw)
	if dec.Reaction != ReactionContinue {
		t.Errorf("reaction: got %q, want continue (unknown enum fallback)", dec.Reaction)
	}
	if !strings.Contains(dec.Reason, "unknown_reaction") {
		t.Errorf("reason should mention unknown_reaction: %q", dec.Reason)
	}
}

func TestParseReactiveDecision_Replan(t *testing.T) {
	raw := `{"reaction":"replan","reason":"fatigue=75 已突破警戒带，当前装配任务不合理"}`
	dec := parseReactiveDecision(raw)
	if dec.Reaction != ReactionReplan {
		t.Errorf("reaction: got %q, want replan", dec.Reaction)
	}
	if dec.Reason != "fatigue=75 已突破警戒带，当前装配任务不合理" {
		t.Errorf("reason: got %q", dec.Reason)
	}
}

func TestBuildReactivePrompt_ReplanOption(t *testing.T) {
	in := ReactiveInput{
		AgentID:      "H-01",
		TimeOfDay:    "14:00",
		Zone:         "main_workshop",
		Energy:       80, Fatigue: 75, Health: 90,
		CurrentAction: "WorkAtWorkbench(target_object_id=workbench_01)",
		ActionSrc:     "tactical",
		CurrentSlot:   "13:00-17:00",
		DailyPlan:     "13:00-17:00 下午装配",
		Trigger:       TriggerPeriodic,
		TriggerDetail: "周期性评估",
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "replan") {
		t.Errorf("prompt should mention 'replan' option, got: %s", prompt)
	}
	if !strings.Contains(prompt, "30 分钟内至多触发 1 次") {
		t.Errorf("prompt should mention replan frequency limit, got: %s", prompt)
	}
	if !strings.Contains(prompt, "continue|observe|replan") {
		t.Errorf("prompt JSON schema should include replan, got: %s", prompt)
	}
}

// TestBuildReactivePrompt_InjectsAgentRole 验证反应层 prompt 注入
// 【你的角色】段：传 AgentRole 后，prompt 应包含该角色画像字段。
// 这是 C4 的核心——反应层决策（continue/observe/replan）应受 NPC
// 性格影响（如"沉稳"性格更倾向 observe 而非频繁 replan）。
func TestBuildReactivePrompt_InjectsAgentRole(t *testing.T) {
	role := "名字：老陈\n职业：车间主管\n性格特质：沉稳、念旧、重视工艺\n说话风格：简洁，偶尔念叨老物件\n"
	in := ReactiveInput{
		AgentID:       "H-01",
		AgentName:     "老陈",
		AgentRole:     role,
		TimeOfDay:     "14:30",
		Zone:          "main_workshop",
		Energy:        45, Fatigue: 30, Health: 90,
		CurrentAction: "WorkAtWorkbench(target_object_id=workbench_01)",
		ActionSrc:     "tactical",
		CurrentSlot:   "14:00-18:00",
		DailyPlan:     "14:00-18:00 工作组装",
		Trigger:       TriggerPeriodic,
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "【你的角色】") {
		t.Errorf("prompt missing '【你的角色】' section header, got: %s", prompt)
	}
	for _, want := range []string{"老陈", "车间主管", "沉稳"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing role field %q, got: %s", want, prompt)
		}
	}
}

// TestBuildReactivePrompt_EmptyAgentRole 验证 AgentRole 为空串时
// prompt 中该段降级为"（无角色信息）"——保留段落占位让 LLM 知道
// 该字段存在但当前不可用，不让 prompt 结构破碎。
func TestBuildReactivePrompt_EmptyAgentRole(t *testing.T) {
	in := ReactiveInput{
		AgentID:   "H-01",
		TimeOfDay: "14:30",
		Zone:      "main_workshop",
		Trigger:   TriggerPeriodic,
	}
	prompt := buildReactivePrompt(in)
	if !strings.Contains(prompt, "【你的角色】") {
		t.Errorf("prompt should still contain '【你的角色】' header when role empty, got: %s", prompt)
	}
	if !strings.Contains(prompt, "（无角色信息）") {
		t.Errorf("prompt should fallback to '（无角色信息）' for empty AgentRole, got: %s", prompt)
	}
}

// TestParseReactiveDecision_CodeFence verifies that ```json ... ``` wrapped
// output is correctly extracted.
func TestParseReactiveDecision_CodeFence(t *testing.T) {
	raw := "```json\n{\"reaction\":\"replan\",\"reason\":\"体力过低\"}\n```"
	dec := parseReactiveDecision(raw)
	if dec.Reaction != ReactionReplan {
		t.Errorf("reaction: got %q, want replan", dec.Reaction)
	}
	if dec.Reason != "体力过低" {
		t.Errorf("reason: got %q, want 体力过低", dec.Reason)
	}
}

// TestParseReactiveDecision_AllEnums verifies each valid enum parses.
func TestParseReactiveDecision_AllEnums(t *testing.T) {
	cases := []struct {
		raw  string
		want ReactionKind
	}{
		{`{"reaction":"continue"}`, ReactionContinue},
		{`{"reaction":"observe"}`, ReactionObserve},
		{`{"reaction":"replan","reason":"fatigue 过高"}`, ReactionReplan},
	}
	for _, c := range cases {
		dec := parseReactiveDecision(c.raw)
		if dec.Reaction != c.want {
			t.Errorf("raw %q: reaction got %q, want %q", c.raw, dec.Reaction, c.want)
		}
	}
}

// TestParseReactiveDecision_LegacyEnumsDowngrade verifies that removed enums
// (interrupt/act) are downgraded to continue rather than causing errors.
func TestParseReactiveDecision_LegacyEnumsDowngrade(t *testing.T) {
	for _, raw := range []string{
		`{"reaction":"interrupt","reason":"旧版本输出"}`,
		`{"reaction":"act","reason":"旧版本输出"}`,
	} {
		dec := parseReactiveDecision(raw)
		if dec.Reaction != ReactionContinue {
			t.Errorf("legacy enum %q: got %q, want continue (downgrade)", raw, dec.Reaction)
		}
		if !strings.Contains(dec.Reason, "unknown_reaction") {
			t.Errorf("reason should mention unknown_reaction: %q", dec.Reason)
		}
	}
}

// TestShouldTriggerReactive_ZoneChange verifies zone change detection.
func TestShouldTriggerReactive_ZoneChange(t *testing.T) {
	trig, detail := shouldTriggerReactive("rest_area", "main_workshop", nil, nil, nil, nil)
	if trig != TriggerZoneChange {
		t.Errorf("trigger: got %q, want zone_change", trig)
	}
	if !strings.Contains(detail, "rest_area") || !strings.Contains(detail, "main_workshop") {
		t.Errorf("detail: got %q, want both zones", detail)
	}
}

// TestShouldTriggerReactive_SameZone verifies no trigger when zone unchanged.
func TestShouldTriggerReactive_SameZone(t *testing.T) {
	trig, _ := shouldTriggerReactive("main_workshop", "main_workshop", nil, nil, nil, nil)
	if trig != "" {
		t.Errorf("trigger: got %q, want empty (same zone)", trig)
	}
}

// TestShouldTriggerReactive_NewObject verifies new object detection.
func TestShouldTriggerReactive_NewObject(t *testing.T) {
	trig, detail := shouldTriggerReactive(
		"main_workshop", "main_workshop",
		[]string{"workbench_01"},
		[]string{"workbench_01", "charging_station_01"},
		nil, nil,
	)
	if trig != TriggerNewObject {
		t.Errorf("trigger: got %q, want new_object", trig)
	}
	if !strings.Contains(detail, "charging_station_01") {
		t.Errorf("detail should mention new object: %q", detail)
	}
}

// TestShouldTriggerReactive_EnergyAlert verifies energy threshold crossing.
func TestShouldTriggerReactive_EnergyAlert(t *testing.T) {
	prev := &protocol.PhysicalState{Energy: 45, Health: 90, Fatigue: 30}
	cur := &protocol.PhysicalState{Energy: 38, Health: 90, Fatigue: 30}
	trig, detail := shouldTriggerReactive("z", "z", nil, nil, prev, cur)
	if trig != TriggerPhysicalAlert {
		t.Errorf("trigger: got %q, want physical_alert", trig)
	}
	if !strings.Contains(detail, "energy") || !strings.Contains(detail, "40") {
		t.Errorf("detail should mention energy + 40: %q", detail)
	}
}

// TestShouldTriggerReactive_EnergyStaysLow verifies no trigger when energy
// stays below threshold (already in alert zone, no new crossing).
func TestShouldTriggerReactive_EnergyStaysLow(t *testing.T) {
	prev := &protocol.PhysicalState{Energy: 38, Health: 90, Fatigue: 30}
	cur := &protocol.PhysicalState{Energy: 35, Health: 90, Fatigue: 30}
	trig, _ := shouldTriggerReactive("z", "z", nil, nil, prev, cur)
	if trig != "" {
		t.Errorf("trigger: got %q, want empty (already in alert)", trig)
	}
}

// TestShouldTriggerReactive_HealthAlert verifies health threshold crossing.
func TestShouldTriggerReactive_HealthAlert(t *testing.T) {
	prev := &protocol.PhysicalState{Energy: 50, Health: 55, Fatigue: 30}
	cur := &protocol.PhysicalState{Energy: 50, Health: 48, Fatigue: 30}
	trig, detail := shouldTriggerReactive("z", "z", nil, nil, prev, cur)
	if trig != TriggerPhysicalAlert {
		t.Errorf("trigger: got %q, want physical_alert", trig)
	}
	if !strings.Contains(detail, "health") || !strings.Contains(detail, "50") {
		t.Errorf("detail should mention health + 50: %q", detail)
	}
}

// TestShouldTriggerReactive_FatigueAlert verifies fatigue threshold crossing.
func TestShouldTriggerReactive_FatigueAlert(t *testing.T) {
	prev := &protocol.PhysicalState{Energy: 50, Health: 90, Fatigue: 55}
	cur := &protocol.PhysicalState{Energy: 50, Health: 90, Fatigue: 62}
	trig, detail := shouldTriggerReactive("z", "z", nil, nil, prev, cur)
	if trig != TriggerPhysicalAlert {
		t.Errorf("trigger: got %q, want physical_alert", trig)
	}
	if !strings.Contains(detail, "fatigue") || !strings.Contains(detail, "60") {
		t.Errorf("detail should mention fatigue + 60: %q", detail)
	}
}

// TestShouldTriggerReactive_NoPhysical verifies no trigger when physical
// states are nil.
func TestShouldTriggerReactive_NoPhysical(t *testing.T) {
	trig, _ := shouldTriggerReactive("z", "z", nil, nil, nil, nil)
	if trig != "" {
		t.Errorf("trigger: got %q, want empty", trig)
	}
}

// TestShouldTriggerPeriodic verifies periodic trigger fires every
// periodicTriggerInterval perceptions.
func TestShouldTriggerPeriodic(t *testing.T) {
	// 第 0 次或负数：不触发
	trig, _ := shouldTriggerPeriodic(0)
	if trig != "" {
		t.Errorf("count=0: got %q, want empty", trig)
	}
	// 第 1/2/3 次：不触发（间隔为 4）
	for i := 1; i < periodicTriggerInterval; i++ {
		trig, _ := shouldTriggerPeriodic(i)
		if trig != "" {
			t.Errorf("count=%d: got %q, want empty", i, trig)
		}
	}
	// 第 4 次：触发
	trig, detail := shouldTriggerPeriodic(periodicTriggerInterval)
	if trig != TriggerPeriodic {
		t.Errorf("count=%d: got %q, want %q", periodicTriggerInterval, trig, TriggerPeriodic)
	}
	if !strings.Contains(detail, "周期性评估") {
		t.Errorf("detail should mention 周期性评估: %q", detail)
	}
	// 第 8 次：再次触发
	trig, _ = shouldTriggerPeriodic(periodicTriggerInterval * 2)
	if trig != TriggerPeriodic {
		t.Errorf("count=%d: got %q, want %q", periodicTriggerInterval*2, trig, TriggerPeriodic)
	}
	// 第 5/6/7 次：不触发
	for i := periodicTriggerInterval + 1; i < periodicTriggerInterval*2; i++ {
		trig, _ := shouldTriggerPeriodic(i)
		if trig != "" {
			t.Errorf("count=%d: got %q, want empty", i, trig)
		}
	}
}

// TestDescribeAction verifies action description formatting for the prompt.
func TestDescribeAction(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		params map[string]any
		want   string
	}{
		{"empty cmd", "", nil, ""},
		{"no params", protocol.CmdWait, nil, protocol.CmdWait},
		{"empty params map", protocol.CmdMoveToLocation, map[string]any{}, protocol.CmdMoveToLocation},
		{"target", protocol.CmdMoveToLocation, map[string]any{"target": "workbench_01"}, "MoveToLocation(target=workbench_01)"},
		{"multiple keys", protocol.CmdWorkAtWorkbench, map[string]any{"target_object_id": "workbench_01", "duration_sec": 3600}, "WorkAtWorkbench(target_object_id=workbench_01, duration_sec=3600)"},
		{"irrelevant keys ignored", protocol.CmdSpeak, map[string]any{"foo": "bar", "content": "hello"}, "Speak(content=hello)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeAction(tt.cmd, tt.params)
			if got != tt.want {
				t.Errorf("describeAction(%q, %v): got %q, want %q", tt.cmd, tt.params, got, tt.want)
			}
		})
	}
}

// TestDedupeKey verifies the dedupe key format.
func TestDedupeKey(t *testing.T) {
	key := dedupeKey("H-01", TriggerZoneChange, "zone A→B")
	want := "H-01|zone_change|zone A→B"
	if key != want {
		t.Errorf("dedupeKey: got %q, want %q", key, want)
	}
}

// TestDiffStrings verifies set difference.
func TestDiffStrings(t *testing.T) {
	got := diffStrings([]string{"a", "b", "c"}, []string{"a"})
	want := []string{"b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestStripCodeFence_NoFence verifies plain JSON passes through.
func TestStripCodeFence_NoFence(t *testing.T) {
	in := `{"reaction":"continue"}`
	if out := stripCodeFence(in); out != in {
		t.Errorf("got %q, want %q", out, in)
	}
}

// TestStripCodeFence_WithFence verifies fenced JSON is extracted.
func TestStripCodeFence_WithFence(t *testing.T) {
	in := "```json\n{\"reaction\":\"continue\"}\n```"
	want := `{"reaction":"continue"}`
	if out := stripCodeFence(in); out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// TestExtractObjectIDs verifies object id extraction from perception payload.
func TestExtractObjectIDs(t *testing.T) {
	cases := []struct {
		name string
		p    protocol.PerceptionPayload
		want []string
	}{
		{
			name: "empty",
			p:    protocol.PerceptionPayload{},
			want: []string{},
		},
		{
			name: "all valid",
			p: protocol.PerceptionPayload{
				NearbyObjects: []protocol.NearbyObject{
					{ID: "workbench_01", Name: "工作台一号"},
					{ID: "charging_station_01", Name: "充电站"},
				},
			},
			want: []string{"workbench_01", "charging_station_01"},
		},
		{
			name: "skip empty id",
			p: protocol.PerceptionPayload{
				NearbyObjects: []protocol.NearbyObject{
					{ID: "workbench_01"},
					{ID: ""}, // 防御性：应跳过
					{ID: "tool_rack_01"},
				},
			},
			want: []string{"workbench_01", "tool_rack_01"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractObjectIDs(c.p)
			if len(got) != len(c.want) {
				t.Fatalf("len: got %d, want %d (got=%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestReactiveRunner_NilSafe verifies nil receiver trigger doesn't panic.
// WS handler 调用路径需保证 reactiveRunnerRef 为 nil 时不崩溃。
func TestReactiveRunner_NilSafe(t *testing.T) {
	var r *reactiveRunner
	ac, _ := newAgentContext(context.Background())
	// 不应 panic
	r.trigger("H-01", ac, TriggerZoneChange, "zone A→B")
}

// TestReactiveRunner_EmptyTriggerNoOp verifies empty trigger is no-op.
func TestReactiveRunner_EmptyTriggerNoOp(t *testing.T) {
	r := &reactiveRunner{}
	ac, _ := newAgentContext(context.Background())
	// 不应 panic（虽然 r.ollama 为 nil，但 trigger=="" 应早返回）
	r.trigger("H-01", ac, "", "detail")
}

// TestReactiveRunner_BuildInput verifies buildInput reads agentContext state correctly.
func TestReactiveRunner_BuildInput(t *testing.T) {
	r := &reactiveRunner{}
	ac, _ := newAgentContext(context.Background())
	zone := "main_workshop"
	ac.mu.Lock()
	ac.latestPerception = mustMarshalPerception(t, zone, "14:30")
	ac.latestPhysical = &protocol.PhysicalState{Energy: 18, Fatigue: 85, Health: 75, JointWear: 20}
	ac.currentActionID = "act_001"
	ac.currentActionCmd = protocol.CmdWorkAtWorkbench
	ac.currentActionParams = map[string]any{"target_object_id": "workbench_01", "duration_sec": 3600}
	ac.mu.Unlock()

	in := r.buildInput("H-01", ac, TriggerPhysicalAlert, "energy 22→18")
	if in.AgentID != "H-01" {
		t.Errorf("AgentID: got %q", in.AgentID)
	}
	if in.Zone != zone {
		t.Errorf("Zone: got %q, want %q", in.Zone, zone)
	}
	if in.TimeOfDay != "14:30" {
		t.Errorf("TimeOfDay: got %q, want 14:30", in.TimeOfDay)
	}
	if in.Energy != 18 {
		t.Errorf("Energy: got %v, want 18", in.Energy)
	}
	if in.Fatigue != 85 {
		t.Errorf("Fatigue: got %v, want 85", in.Fatigue)
	}
	if in.Health != 75 {
		t.Errorf("Health: got %v, want 75", in.Health)
	}
	// CurrentAction 现在是可读描述（cmd + 关键 params），不再是 actionID
	if in.CurrentAction != "WorkAtWorkbench(target_object_id=workbench_01, duration_sec=3600)" {
		t.Errorf("CurrentAction: got %q, want readable description", in.CurrentAction)
	}
	if in.Trigger != TriggerPhysicalAlert {
		t.Errorf("Trigger: got %q", in.Trigger)
	}
	if in.TriggerDetail != "energy 22→18" {
		t.Errorf("TriggerDetail: got %q", in.TriggerDetail)
	}
}

// TestReactiveRunner_BuildInput_DefaultsPhysicalWhenNil verifies default physical
// values when state_report has not yet arrived (latestPhysical == nil).
func TestReactiveRunner_BuildInput_DefaultsPhysicalWhenNil(t *testing.T) {
	r := &reactiveRunner{}
	ac, _ := newAgentContext(context.Background())
	// 不设 latestPhysical —— 反应层不应把 0 误判为警戒带触发
	in := r.buildInput("H-01", ac, TriggerZoneChange, "zone A→B")
	if in.Energy != 100 {
		t.Errorf("Energy default: got %v, want 100", in.Energy)
	}
	if in.Health != 100 {
		t.Errorf("Health default: got %v, want 100", in.Health)
	}
	if in.Fatigue != 0 {
		t.Errorf("Fatigue default: got %v, want 0", in.Fatigue)
	}
}

// mustMarshalPerception constructs a minimal perception JSON for testing.
// tod 是 "HH:MM" 格式，按约定 19 转为 time_of_day_sec / game_time_sec 等字段。
func mustMarshalPerception(t *testing.T, zone, tod string) json.RawMessage {
	t.Helper()
	zonePtr := zone
	todSec := parseTodToSec(t, tod)
	p := protocol.PerceptionPayload{
		Location:    protocol.Location{CurrentZone: &zonePtr},
		Environment: protocol.Environment{GameTimeSec: todSec, TimeOfDaySec: todSec, DayCount: 0, TimeScale: 60},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal perception: %v", err)
	}
	return b
}

// parseTodToSec 把 "HH:MM" 转为当天秒数（测试辅助）。
func parseTodToSec(t *testing.T, tod string) float64 {
	t.Helper()
	parts := strings.Split(tod, ":")
	if len(parts) != 2 {
		t.Fatalf("parseTodToSec: invalid tod %q", tod)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("parseTodToSec: invalid hour %q: %v", tod, err)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("parseTodToSec: invalid minute %q: %v", tod, err)
	}
	return float64(h*3600 + m*60)
}

// ─── 动态 cmd 派生（Phase 2） ────────────────────────────────
//
// 反应层现已移除 act/interrupt，仅保留 continue/observe/replan。
// isValidReactionCmd / buildReactiveCmdList / mapReactionAction 及相关测试
// 已随之移除。

// ─── gameTimeDeltaMinutes（replan 游戏时间去抖） ───────────────

func TestGameTimeDeltaMinutes_Normal(t *testing.T) {
	got := gameTimeDeltaMinutes("06:00", "07:30")
	if got != 90 {
		t.Errorf("delta 06:00→07:30 = %d, want 90", got)
	}
}

func TestGameTimeDeltaMinutes_SameTime(t *testing.T) {
	got := gameTimeDeltaMinutes("11:00", "11:00")
	if got != 0 {
		t.Errorf("delta same time = %d, want 0", got)
	}
}

func TestGameTimeDeltaMinutes_BelowWindow(t *testing.T) {
	// 30 分钟差，应返回 30（< 60 分钟去抖窗口）
	got := gameTimeDeltaMinutes("11:00", "11:30")
	if got != 30 {
		t.Errorf("delta 11:00→11:30 = %d, want 30", got)
	}
}

func TestGameTimeDeltaMinutes_DayWrap(t *testing.T) {
	// 跨日：23:30 → 00:30 应为 60 分钟，不是 -1380
	got := gameTimeDeltaMinutes("23:30", "00:30")
	if got != 60 {
		t.Errorf("delta 23:30→00:30 = %d, want 60 (day wrap)", got)
	}
}

func TestGameTimeDeltaMinutes_EmptyArgs(t *testing.T) {
	// 任一为空返回 0（无去抖信息，允许触发）
	if got := gameTimeDeltaMinutes("", "12:00"); got != 0 {
		t.Errorf("delta empty prev = %d, want 0", got)
	}
	if got := gameTimeDeltaMinutes("12:00", ""); got != 0 {
		t.Errorf("delta empty cur = %d, want 0", got)
	}
}

func TestGameTimeDeltaMinutes_InvalidArgs(t *testing.T) {
	// 解析失败返回 0
	if got := gameTimeDeltaMinutes("abc", "12:00"); got != 0 {
		t.Errorf("delta invalid prev = %d, want 0", got)
	}
	if got := gameTimeDeltaMinutes("12:00", "xyz"); got != 0 {
		t.Errorf("delta invalid cur = %d, want 0", got)
	}
}
