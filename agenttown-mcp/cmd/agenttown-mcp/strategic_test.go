package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
	"log/slog"
)

// fakeStrategicCaller 实现 strategicCaller 接口，用于单测。
type fakeStrategicCaller struct {
	resp        *hermes.Response
	err         error
	resetCalled bool
}

func (f *fakeStrategicCaller) SendWithSummary(_ context.Context, _, _ string) (*hermes.Response, error) {
	return f.resp, f.err
}

func (f *fakeStrategicCaller) ResetSession() { f.resetCalled = true }

// makeStrategicResponse 构造一个 ExtractText 能提取出 text 的 Response。
func makeStrategicResponse(text string) *hermes.Response {
	return &hermes.Response{
		Status: "completed",
		Output: []hermes.Block{{
			Type: "message",
			Role: "assistant",
			Content: []hermes.Content{{
				Type: "output_text",
				Text: text,
			}},
		}},
	}
}

// ─── parseDailyPlan ──────────────────────────────────────────

func TestParseDailyPlan_ValidJSON(t *testing.T) {
	raw := `[{"time":"07:00-08:00","goal":"晨检"},{"time":"08:00-12:00","goal":"装配"}]`
	items, err := parseDailyPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Time != "07:00-08:00" || items[0].Goal != "晨检" {
		t.Errorf("item[0] = %+v", items[0])
	}
}

func TestParseDailyPlan_JSONFence(t *testing.T) {
	raw := "```json\n[{\"time\":\"06:00-07:00\",\"goal\":\"起床\"}]\n```"
	items, err := parseDailyPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].Goal != "起床" {
		t.Errorf("items = %+v", items)
	}
}

func TestParseDailyPlan_NarrativePrefix(t *testing.T) {
	raw := `好的，这是我今天的计划：` + "\n" +
		`[{"time":"07:00-12:00","goal":"车间装配"}]` + "\n" +
		`希望今天顺利。`
	items, err := parseDailyPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 || items[0].Goal != "车间装配" {
		t.Errorf("items = %+v", items)
	}
}

func TestParseDailyPlan_TruncatedMissingClosingBracket(t *testing.T) {
	// LLM 输出被上游截断，缺少末尾 ]。parseDailyPlan 应容错补 ] 后解析成功。
	raw := `[{"time":"06:00-07:00","goal":"起床晨检"},{"time":"07:00-12:00","goal":"车间装配"}`
	items, err := parseDailyPlan(raw)
	if err != nil {
		t.Fatalf("unexpected error for truncated output: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[1].Goal != "车间装配" {
		t.Errorf("items[1] = %+v", items[1])
	}
}

func TestParseDailyPlan_Malformed(t *testing.T) {
	raw := "今天我打算去车间看看，然后再去充电。"
	if _, err := parseDailyPlan(raw); err == nil {
		t.Fatal("expected error for narrative without JSON array")
	}
}

func TestParseDailyPlan_Empty(t *testing.T) {
	if _, err := parseDailyPlan(""); err == nil {
		t.Fatal("expected error for empty input")
	}
}

// ─── formatDailyPlan ─────────────────────────────────────────

func TestFormatDailyPlan_Empty(t *testing.T) {
	if got := formatDailyPlan(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestFormatDailyPlan_MultipleItems(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-08:00", Goal: "晨检"},
		{Time: "08:00-12:00", Goal: "装配"},
	}
	got := formatDailyPlan(items)
	want := "07:00-08:00: 晨检\n08:00-12:00: 装配"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ─── generateDailyPlan ───────────────────────────────────────

func TestGenerateDailyPlan_HTTPError(t *testing.T) {
	sc := &fakeStrategicCaller{err: errors.New("network down")}
	plan := generateDailyPlan(context.Background(), sc, "H-01", slog.Default())
	// HTTP 错误现在回退到 defaultDailyPlan 而不是空字符串，
	// 保证战术层有目标可分解、仿真不瘫痪。
	if plan != defaultDailyPlan {
		t.Errorf("got %q, want defaultDailyPlan on error", plan)
	}
	if sc.resetCalled {
		t.Error("ResetSession should not be called when SendWithSummary fails")
	}
}

func TestGenerateDailyPlan_ValidResponse(t *testing.T) {
	raw := `[{"time":"06:00-07:00","goal":"起床晨检"},{"time":"07:00-12:00","goal":"车间装配"}]`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	plan := generateDailyPlan(context.Background(), sc, "H-01", slog.Default())
	if plan == "" {
		t.Fatal("got empty plan, want non-empty")
	}
	if !strings.Contains(plan, "车间装配") {
		t.Errorf("plan missing expected goal: %q", plan)
	}
	if !sc.resetCalled {
		t.Error("ResetSession should be called after successful generation")
	}
}

func TestGenerateDailyPlan_ParseFail(t *testing.T) {
	sc := &fakeStrategicCaller{resp: makeStrategicResponse("今天天气不错，我打算去车间转转。")}
	plan := generateDailyPlan(context.Background(), sc, "H-01", slog.Default())
	// 解析失败现在回退到 defaultDailyPlan 而不是空字符串，
	// 避免整天 Wait(60s) 瘫痪。
	if plan != defaultDailyPlan {
		t.Errorf("got %q, want defaultDailyPlan on parse failure", plan)
	}
}

// ─── selectPlanInjection ─────────────────────────────────────

func TestSelectPlanInjection_EmptyPlan(t *testing.T) {
	inj, slot := selectPlanInjection("", "08:00", "")
	if inj != "" || slot != "" {
		t.Errorf("got inj=%q slot=%q, want empty", inj, slot)
	}
}

func TestSelectPlanInjection_BoundaryCross(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 装配\n12:00-13:00: 午餐"
	// 08:30 在 08:00-12:00 时段内，上次是 07:00-08:00 → 边界跨越，注入完整计划
	inj, slot := selectPlanInjection(plan, "08:30", "07:00-08:00")
	if slot != "08:00-12:00" {
		t.Errorf("slot=%q, want 08:00-12:00", slot)
	}
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("inj should start with [今日计划], got %q", inj)
	}
	if !strings.Contains(inj, "装配") {
		t.Errorf("inj should contain full plan, got %q", inj)
	}
}

func TestSelectPlanInjection_SameSlot(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 装配\n12:00-13:00: 午餐"
	// 09:00 还在 08:00-12:00，上次也是 08:00-12:00 → 只注入当前时段
	inj, slot := selectPlanInjection(plan, "09:00", "08:00-12:00")
	if slot != "08:00-12:00" {
		t.Errorf("slot=%q, want 08:00-12:00", slot)
	}
	want := "[当前时段] 08:00-12:00: 装配"
	if inj != want {
		t.Errorf("inj=%q, want %q", inj, want)
	}
}

func TestSelectPlanInjection_FirstDecision(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 装配"
	// lastSlot 为空 → 视为边界跨越，注入完整计划
	inj, slot := selectPlanInjection(plan, "07:30", "")
	if slot != "07:00-08:00" {
		t.Errorf("slot=%q, want 07:00-08:00", slot)
	}
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("first decision should inject full plan, got %q", inj)
	}
}

func TestSelectPlanInjection_UnparseableTime(t *testing.T) {
	plan := "07:00-08:00: 晨检"
	// timeOfDay 无法解析 → 回退全量注入
	inj, _ := selectPlanInjection(plan, "invalid", "07:00-08:00")
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("unparseable time should fall back to full plan, got %q", inj)
	}
}

func TestSelectPlanInjection_TimeBeforeFirstSlot(t *testing.T) {
	plan := "07:00-08:00: 晨检\n08:00-12:00: 装配"
	// 06:00 在所有时段之前 → matchPlanSlot 返回空 → 回退全量
	inj, slot := selectPlanInjection(plan, "06:00", "")
	if slot != "" {
		t.Errorf("slot=%q, want empty (no match)", slot)
	}
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("no matching slot should fall back to full plan, got %q", inj)
	}
}

func TestSelectPlanInjection_OvernightSlot(t *testing.T) {
	// 跨日时段 "17:30-06:00"：傍晚到次日清晨
	plan := "15:30-17:00: 收尾\n17:00-17:30: 日志\n17:30-06:00: 充电休息"
	// 19:30 在 17:30-06:00 内 → 应匹配跨日时段
	inj, slot := selectPlanInjection(plan, "19:30", "17:00-17:30")
	if slot != "17:30-06:00" {
		t.Errorf("slot=%q, want 17:30-06:00", slot)
	}
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("boundary cross should inject full plan, got %q", inj)
	}
	// 同一时段内第二次决策 → 只注入当前时段
	inj2, slot2 := selectPlanInjection(plan, "20:30", "17:30-06:00")
	if slot2 != "17:30-06:00" {
		t.Errorf("slot2=%q, want 17:30-06:00", slot2)
	}
	want := "[当前时段] 17:30-06:00: 充电休息"
	if inj2 != want {
		t.Errorf("inj2=%q, want %q", inj2, want)
	}
}

func TestSelectPlanInjection_OvernightSlotEarlyMorning(t *testing.T) {
	plan := "17:30-06:00: 充电休息"
	// 03:00 在 17:30-06:00 的跨日部分（[0,360)）内
	inj, slot := selectPlanInjection(plan, "03:00", "")
	if slot != "17:30-06:00" {
		t.Errorf("slot=%q, want 17:30-06:00", slot)
	}
	if !strings.HasPrefix(inj, "[今日计划]") {
		t.Errorf("first decision should inject full plan, got %q", inj)
	}
}
