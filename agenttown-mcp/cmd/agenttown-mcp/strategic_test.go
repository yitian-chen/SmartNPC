package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// fakeStrategicCaller 实现 strategicCaller / llmClient 接口，用于单测。
type fakeStrategicCaller struct {
	resp               *llmtypes.Response
	err                error
	capturedInput      string
	capturedSystem     string
	capturedSchemaName string
	capturedTools      []venus.Tool
	resetCalled        bool
}

func (f *fakeStrategicCaller) SendWithSummary(_ context.Context, _, user string, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	f.capturedInput = user
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendWithSummaryTools(_ context.Context, _, user string, tools []venus.Tool) (*llmtypes.Response, error) {
	f.capturedInput = user
	f.capturedTools = tools
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendMessagesTools(_ context.Context, messages []llmtypes.Message, _ []venus.Tool) (*llmtypes.Response, error) {
	if len(messages) > 0 {
		f.capturedInput = messages[len(messages)-1].Content
	}
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendStreaming(_ context.Context, _, _ string, _ func(string)) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendStreamingTools(_ context.Context, _, _ string, _ []venus.Tool, _ func(string), _ func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendWithSchema(_ context.Context, system, user, schemaName string, _ []byte, _ ...[]venus.Tool) (*llmtypes.Response, error) {
	f.capturedSystem = system
	f.capturedInput = user
	f.capturedSchemaName = schemaName
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendLoop(_ context.Context, messages []llmtypes.Message, _ []venus.Tool, _, schemaName string, _ []byte) (*llmtypes.Response, error) {
	if len(messages) > 0 {
		f.capturedSystem = messages[0].Content
		f.capturedInput = messages[len(messages)-1].Content
	}
	f.capturedSchemaName = schemaName
	return f.resp, f.err
}

func (f *fakeStrategicCaller) SendLoopStreaming(_ context.Context, messages []llmtypes.Message, _ []venus.Tool, _, schemaName string, _ []byte, _ func(string), _ func(llmtypes.ToolCall)) (*llmtypes.Response, error) {
	return f.SendLoop(context.Background(), messages, nil, "", schemaName, nil)
}

func (f *fakeStrategicCaller) ResetSession() { f.resetCalled = true }

// makeStrategicResponse 构造一个 ExtractText 能提取出 text 的 Response。
func makeStrategicResponse(text string) *llmtypes.Response {
	return &llmtypes.Response{
		Status: "completed",
		Output: []llmtypes.Block{{
			Type: "message",
			Role: "assistant",
			Content: []llmtypes.Content{{
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

// strategicCtxForTest 构造挂 fake 战略客户端的 agentContext（统一 agentic loop
// 测试用）：as 为全新 AgentState（历史从空开始），strategicHc 接 fake。
func strategicCtxForTest(sc *fakeStrategicCaller) *agentContext {
	return &agentContext{as: agentstate.New(), strategicHc: sc}
}

// ─── generateDailyPlan ───────────────────────────────────────

func TestGenerateDailyPlan_HTTPError(t *testing.T) {
	sc := &fakeStrategicCaller{err: errors.New("network down")}
	plan := strategicCtxForTest(sc).triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")
	// HTTP 错误现在回退到 prompt.DefaultDailyPlan(nil) 的扰动版本
	// （时间节点 ±planJitterMinutes 错峰），而不是空字符串，
	// 保证战术层有目标可分解、仿真不瘫痪。
	if !isJitteredDefaultPlan(t, plan) {
		t.Errorf("got %q, want jittered defaultDailyPlan on error", plan)
	}
	if sc.resetCalled {
		t.Error("ResetSession should not be called when SendWithSummary fails")
	}
}

func TestGenerateDailyPlan_ValidResponse(t *testing.T) {
	raw := `[{"time":"06:00-07:00","goal":"起床晨检"},{"time":"07:00-12:00","goal":"车间装配"}]`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	plan := strategicCtxForTest(sc).triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")
	if plan == "" {
		t.Fatal("got empty plan, want non-empty")
	}
	if !strings.Contains(plan, "车间装配") {
		t.Errorf("plan missing expected goal: %q", plan)
	}
	if !sc.resetCalled {
		t.Error("ResetSession should be called after successful generation")
	}
	// 战略层必须走 Structured Outputs（json_schema strict）约束输出格式。
	if sc.capturedSchemaName != "daily_plan" {
		t.Errorf("generateDailyPlan should call SendWithSchema with schema name daily_plan, got %q", sc.capturedSchemaName)
	}
}

func TestGenerateDailyPlan_ParseFail(t *testing.T) {
	sc := &fakeStrategicCaller{resp: makeStrategicResponse("今天天气不错，我打算去车间转转。")}
	plan := strategicCtxForTest(sc).triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")
	// 解析失败现在回退到 prompt.DefaultDailyPlan(nil) 的扰动版本，
	// 避免整天 Wait(60s) 瘫痪。
	if !isJitteredDefaultPlan(t, plan) {
		t.Errorf("got %q, want jittered defaultDailyPlan on parse failure", plan)
	}
}

// isJitteredDefaultPlan 校验 plan 是 DefaultDailyPlan(nil) 的合法扰动结果：
// 行数一致、每条 goal 一致、时间可解析且为 "HH:MM-HH:MM" 格式。
func isJitteredDefaultPlan(t *testing.T, plan string) bool {
	t.Helper()
	want := parseFormattedPlan(prompt.DefaultDailyPlan(nil, "H-01"))
	got := parseFormattedPlan(plan)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Goal != want[i].Goal {
			return false
		}
		if _, _, ok := prompt.SplitPlanRange(got[i].Time); !ok {
			return false
		}
	}
	return true
}

// ─── buildSharedSystemPrompt ───────────────────────────────

func TestBuildSharedSystemPrompt_WithKB(t *testing.T) {
	kb := loadTestKB(t)
	got := prompt.BuildSharedSystemPrompt(kb, nil, "H-01")
	if got == "" {
		t.Fatal("got empty system prompt, want non-empty for valid KB")
	}
	// 人物背景段：包含 agent 显示名和职业
	if !strings.Contains(got, "老陈") {
		t.Errorf("system prompt missing agent display name '老陈': %q", got)
	}
	// 新 fallback 用 "装配工人（专做工作台装配作业）"
	if !strings.Contains(got, "装配工人（专做工作台装配作业）") {
		t.Errorf("system prompt missing agent profession '装配工人（专做工作台装配作业）': %q", got)
	}
	if !strings.Contains(got, "【人物背景】") {
		t.Errorf("system prompt missing '【人物背景】' header: %q", got)
	}
	// 世界信息：包含 zone id 和 object semantic_group
	if !strings.Contains(got, "main_workshop") {
		t.Errorf("system prompt missing zone id 'main_workshop': %q", got)
	}
	if !strings.Contains(got, "workbench") {
		t.Errorf("system prompt missing object semantic_group 'workbench': %q", got)
	}
	if !strings.Contains(got, "【世界背景】") || !strings.Contains(got, "【世界详细信息】") {
		t.Errorf("system prompt missing world module headers: %q", got)
	}
}

func TestBuildSharedSystemPrompt_NilKB(t *testing.T) {
	// kb == nil 且 actions == nil：复合动作段为空（可用工具仅从 cmd 派生，
	// 无内置兜底——UE 未连接时 LLM 不获知任何工具）。
	got := prompt.BuildSharedSystemPrompt(nil, nil, "H-01")
	if strings.Contains(got, "复合动作（长时段活动用") {
		t.Errorf("nil actions should not produce composite capability section: %q", got)
	}
	if strings.Contains(got, "work_shift") {
		t.Errorf("nil actions should not list any tool: %q", got)
	}
	// kb == nil 时世界背景模块跳过（前言提及模块名不算）；规则已迁至
	// user prompt，system prompt 不包含。
	if strings.Contains(got, "【世界背景】\n") {
		t.Errorf("nil KB should not produce 世界背景 module: %q", got)
	}
	if strings.Contains(got, "规划要求：") {
		t.Errorf("system prompt should not contain rules module (moved to user prompt): %q", got)
	}
}

func TestBuildSharedSystemPrompt_AgentNotFound(t *testing.T) {
	// KB 存在但 agentID 不在 KB 中：跳过人物背景段，仍注入世界模块。
	kb := loadTestKB(t)
	got := prompt.BuildSharedSystemPrompt(kb, nil, "NONEXISTENT-99")
	if strings.Contains(got, "【人物背景】\n") {
		t.Errorf("should not include persona module for unknown agent: %q", got)
	}
	if !strings.Contains(got, "【世界背景】\n") {
		t.Errorf("should still include world overview module even if agent unknown: %q", got)
	}
}

// ─── buildAgentRoleContext ────────────────────────────────────

// TestBuildAgentRoleContext_WithKB 验证从真实 KB 取 H-01 的角色画像：
// 新 UE5 authored 提供 display_name/role[shim→profession]/personality.traits，
// 但未提供 description 和 personality.speech_style，故输出仅含名字/职业/性格特质
// 三项（helper 跳过空字段）。helper 不带【你的角色】标题——标题由调用方决定
// （战略层 buildStrategicContext 加标题、战术层 buildTacticalPrompt 加标题，
// helper 本身只输出裸字段）。
func TestBuildAgentRoleContext_WithKB(t *testing.T) {
	kb := loadTestKB(t)
	got := prompt.AgentRole(kb, nil, "H-01")
	if got == "" {
		t.Fatal("got empty role, want non-empty for valid KB + agent")
	}
	for _, want := range []string{
		"名字：老陈",
		"职业：装配工人（专做工作台装配作业）",
		"性格特质：沉稳、念旧、耐久省电、磨损慢",
		"背景：资深装配工人，常驻主生产车间工作台，只做装配（assemble）",
		"说话风格：简短有力，多用行业术语",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("role missing %q, got: %q", want, got)
		}
	}
	// helper 不应包含段落标题——标题由调用方添加
	if strings.Contains(got, "【你的角色】") {
		t.Errorf("helper should not include section header, got: %q", got)
	}
}

// TestBuildAgentRoleContext_NilKB 验证 nil KB 降级到硬编码兜底（H-01），
// 返回老陈的简短角色画像，不 panic。三层决策 prompt 仍能注入角色风格。
// fallback 内容与 assets/world_kb.yaml 中 H-01 的字段保持一致，确保
// KB 加载和 fallback 路径产出相同文本。
func TestBuildAgentRoleContext_NilKB(t *testing.T) {
	got := prompt.AgentRole(nil, nil, "H-01")
	if got == "" {
		t.Fatal("got empty role for H-01 with nil KB, want fallback content")
	}
	for _, want := range []string{"名字：老陈", "职业：装配工人（专做工作台装配作业）", "沉稳、念旧、耐久省电、磨损慢"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback role missing %q, got: %q", want, got)
		}
	}
}

// TestBuildAgentRoleContext_AgentNotFound 验证 KB 存在但 agentID
// 不在 KB 中时返回空串（降级路径，三层决策共用此 helper）。
func TestBuildAgentRoleContext_AgentNotFound(t *testing.T) {
	kb := loadTestKB(t)
	got := prompt.AgentRole(kb, nil, "NONEXISTENT-99")
	if got != "" {
		t.Errorf("got %q, want empty for unknown agent", got)
	}
}

// ─── generateDailyPlan KB injection ──────────────────────────

func TestGenerateDailyPlan_KBInjectedIntoPrompt(t *testing.T) {
	// 验证 generateDailyPlan 的 system/user 拆分：KB 世界信息 + 人物背景
	// 进入 system prompt；动态段（物理状态兜底 + 昨日总结）进入 user prompt。
	kb := loadTestKB(t)
	raw := `[{"time":"06:00-07:00","goal":"起床晨检"}]`
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(raw)}
	_ = strategicCtxForTest(sc).triggerStrategicPlanning(context.Background(), "H-01", kb, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")

	sys := sc.capturedSystem
	if sys == "" {
		t.Fatal("captured system prompt is empty")
	}
	if !strings.Contains(sys, "老陈") {
		t.Errorf("system prompt missing agent display name '老陈': %q", sys)
	}
	if !strings.Contains(sys, "main_workshop") {
		t.Errorf("system prompt missing zone id 'main_workshop': %q", sys)
	}
	if !strings.Contains(sys, "【人物背景】") {
		t.Errorf("system prompt missing '【人物背景】' header: %q", sys)
	}
	if !strings.Contains(sys, "【世界背景】") || !strings.Contains(sys, "【世界详细信息】") {
		t.Errorf("system prompt missing world module headers: %q", sys)
	}

	user := sc.capturedInput
	if user == "" {
		t.Fatal("captured user prompt is empty")
	}
	if !strings.Contains(user, "昨日总结：") {
		t.Errorf("user prompt missing yesterday summary: %q", user)
	}
	if !strings.Contains(user, "规划要求：") {
		t.Errorf("user prompt missing the rules block: %q", user)
	}
	if !strings.Contains(user, "连续两个时段不得任务完全相同") {
		t.Errorf("user prompt missing rule 2 text: %q", user)
	}
	if !strings.Contains(user, "【物理状态】") {
		t.Errorf("user prompt missing physical state segment: %q", user)
	}
	// KB 内容不应重复出现在 user prompt（规则文本中引用模块名不算，
	// 模块头以换行结尾）。
	if strings.Contains(user, "【世界详细信息】\n") {
		t.Errorf("user prompt should not contain world detail module: %q", user)
	}
}

// ─── buildDefaultDailyPlan ───────────────────────────────────

func TestBuildDefaultDailyPlan_NilKB(t *testing.T) {
	// kb == nil 返回 defaultDailyPlan 常量（中性表述，无 KB 专属词）。
	got := prompt.DefaultDailyPlan(nil, "H-01")
	want := prompt.DefaultDailyPlan(nil, "H-01")
	if got != want {
		t.Errorf("got %q, want defaultDailyPlan %q", got, want)
	}
	// 中性表述不应包含旧 KB 专属词
	for _, w := range []string{"车间", "装配", "充电"} {
		if strings.Contains(got, w) {
			t.Errorf("nil-KB fallback should not contain KB-specific word %q: %q", w, got)
		}
	}
}

func TestBuildDefaultDailyPlan_WithKB(t *testing.T) {
	// 有 KB 时：工作时段从工作类设施（category=work）中按 agentID 稳定
	// 选择——不再机械取"首个 zone + 首个 object"（KB objects 按字典序
	// 首个是 bench-1 长椅，会拼出"在档案馆进行长椅作业"的荒谬组合，
	// 2026-09-03 venus 429 全员兜底时实测出现）。
	kb := loadTestKB(t)
	got := prompt.DefaultDailyPlan(kb, "H-01")
	// 兜底必须落在工作类设施里（工作台/调试台/加工机/分拣传送带等），
	// 绝不能是休息类设施"长椅"。
	workNames := map[string]bool{
		"工作台": true, "调试台": true, "质检台": true,
		"加工机": true, "分拣传送带": true, "拆解台": true,
	}
	if !strings.Contains(got, "作业") {
		t.Errorf("KB-derived plan should have work slots: %q", got)
	}
	foundWork := false
	for name := range workNames {
		if strings.Contains(got, name) {
			foundWork = true
		}
	}
	if !foundWork {
		t.Errorf("plan should mention a work-category facility, got: %q", got)
	}
	if strings.Contains(got, "长椅作业") {
		t.Errorf("plan must not pair rest facility with 作业: %q", got)
	}
	// 跨日仿真：兜底计划含 5 个时段。
	items := parseFormattedPlan(got)
	if len(items) != 5 {
		t.Errorf("got %d plan items, want 5", len(items))
	}
	// 同一 agentID 多次调用稳定（跨重试不漂移）。
	if again := prompt.DefaultDailyPlan(kb, "H-01"); again != got {
		t.Errorf("DefaultDailyPlan not stable for same agentID:\nfirst:  %q\nsecond: %q", got, again)
	}
}

func TestBuildDefaultDailyPlan_PerAgentDiffers(t *testing.T) {
	// 5 个 NPC 并发兜底时计划应尽量错开（按 agentID 稳定哈希选择不同
	// 工作设施），不再全员同一份"长椅作业"。
	kb := loadTestKB(t)
	plans := map[string]string{}
	for _, id := range []string{"H-01", "H-02", "H-03", "H-04", "H-05"} {
		plans[id] = prompt.DefaultDailyPlan(kb, id)
	}
	distinct := map[string]bool{}
	for _, p := range plans {
		distinct[p] = true
	}
	if len(distinct) < 2 {
		t.Errorf("expected per-agent plans to differ, got %d distinct of 5:\n%s",
			len(distinct), strings.Join(mapValues(plans), "\n---\n"))
	}
}

func TestBuildDefaultDailyPlan_NoWorkCategory(t *testing.T) {
	// KB 无工作类设施（category=work 为空）时退化为中性"主要工作"，
	// 不引用休息设施。
	kb := &worldkb.KB{
		Zones: []worldkb.Zone{{ID: "plaza", DisplayName: "中央广场"}},
		Objects: []worldkb.Object{
			{ID: "bench-1", DisplayName: "长椅", Category: "rest", ZoneID: "plaza"},
		},
	}
	got := prompt.DefaultDailyPlan(kb, "H-01")
	if !strings.Contains(got, "主要工作") {
		t.Errorf("no-work KB should fall back to neutral wording, got: %q", got)
	}
	if strings.Contains(got, "长椅") {
		t.Errorf("no-work KB plan must not mention rest facility: %q", got)
	}
}

// mapValues 提取 map 的值切片（测试辅助）。
func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// ─── normalizeDailyPlan ─────────────────────────────────────

func TestNormalizeDailyPlan_KeepsShortSlots(t *testing.T) {
	// 短时段裁剪已停用：30-59 分钟的短时段（晨练/午休等）应被保留，
	// 否则 LLM 生成的优质日程会被裁成"工作+睡眠"的简略形态。
	items := []dailyPlanItem{
		{Time: "06:00-06:30", Goal: "短时段"}, // 30min, 不再被丢弃
		{Time: "06:30-12:00", Goal: "上午工作"},
		{Time: "12:00-22:00", Goal: "下午到晚上"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (short slot kept)", len(got))
	}
	found := false
	for _, it := range got {
		if it.Goal == "短时段" {
			found = true
		}
	}
	if !found {
		t.Errorf("short slot should be kept: %+v", got)
	}
}

func TestNormalizeDailyPlan_FillsGap(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "06:00-10:00", Goal: "上午"},
		{Time: "14:00-22:00", Goal: "下午"}, // 10:00-14:00 是空白
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].Time != "06:00-14:00" {
		t.Errorf("first slot should be extended to fill gap: got %q, want 06:00-14:00", got[0].Time)
	}
	// 末段 14:00-22:00，end=22:00 < dayEndMinute（次日 06:00），后延到次日 06:00。
	if got[1].Time != "14:00-06:00" {
		t.Errorf("second slot extended to next-day 06:00: got %q, want 14:00-06:00", got[1].Time)
	}
}

func TestNormalizeDailyPlan_ExtendsFirstSlot(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "08:00-12:00", Goal: "上午"}, // 07:00-08:00 空白（06:00-07:00 是规划时间不覆盖）
		{Time: "12:00-22:00", Goal: "下午"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if got[0].Time != "07:00-12:00" {
		t.Errorf("first slot should start at 07:00: got %q, want 07:00-12:00", got[0].Time)
	}
}

func TestNormalizeDailyPlan_MidDayStartNotStretchedToMorning(t *testing.T) {
	// 中途触发的战略规划：首段前伸到触发时刻（14:00），不回拉到 07:00
	// 覆盖已流逝的上午。
	items := []dailyPlanItem{
		{Time: "15:00-18:00", Goal: "下午工作"},
		{Time: "18:00-22:00", Goal: "广场休息"},
	}
	got := normalizeDailyPlan(items, 14*60)
	if got[0].Time != "14:00-18:00" {
		t.Errorf("first slot should stretch to trigger time 14:00, got %q", got[0].Time)
	}
}

func TestNormalizeDailyPlan_ExtendsLastSlot(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-12:00", Goal: "上午"},
		{Time: "12:00-18:00", Goal: "下午"}, // 18:00-22:00 空白
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if got[len(got)-1].Time != "12:00-22:00" {
		t.Errorf("last slot should end at 22:00: got %q, want 12:00-22:00", got[len(got)-1].Time)
	}
}

func TestNormalizeDailyPlan_KeepsAllShortSlots(t *testing.T) {
	// 短时段裁剪停用后，全短时段计划不再返回 nil（交由后续步骤填补空白）。
	items := []dailyPlanItem{
		{Time: "06:00-06:30", Goal: "短1"},
		{Time: "07:00-07:15", Goal: "短2"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if got == nil || len(got) != 2 {
		t.Fatalf("short slots should be kept, got %+v", got)
	}
}

func TestNormalizeDailyPlan_AlreadyValid(t *testing.T) {
	// 跨日仿真：计划覆盖 06:00 到次日 06:00，末段为跨午夜夜间 slot。
	items := []dailyPlanItem{
		{Time: "06:00-12:00", Goal: "上午"},
		{Time: "12:00-22:00", Goal: "下午"},
		{Time: "22:00-06:00", Goal: "夜间休息"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	if got[0].Time != "06:00-12:00" || got[1].Time != "12:00-22:00" || got[2].Time != "22:00-06:00" {
		t.Errorf("already-valid plan should be unchanged: got %+v", got)
	}
}

// TestNormalizeDailyPlan_ExtendsCrossMidnightLastSlot 验证跨午夜末段结束
// 早于 06:00（如实测的 23:29-00:54）时被后延到 06:00，避免凌晨空洞导致
// 睡眠段半夜到期被打断重睡。
func TestNormalizeDailyPlan_ExtendsCrossMidnightLastSlot(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-12:00", Goal: "上午"},
		{Time: "12:00-22:00", Goal: "傍晚"},
		{Time: "23:29-00:54", Goal: "休眠"}, // 跨午夜，只睡到 00:54
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	if got[2].Time != "23:29-06:00" {
		t.Errorf("cross-midnight last slot should extend to 06:00: got %q, want 23:29-06:00", got[2].Time)
	}
}

// TestNormalizeDailyPlan_MergesAdjacentSameGoal 验证相邻同时段被合并
// （实测 LLM 输出连续两段睡眠 20:30-22:58 + 22:58-07:16，边界到期会打断
// 睡眠重新分解）。
func TestNormalizeDailyPlan_MergesAdjacentSameGoal(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-20:30", Goal: "白天活动"},
		{Time: "20:30-22:58", Goal: "睡觉"},
		{Time: "22:58-07:16", Goal: "睡觉"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2 (same-goal slots merged): %+v", len(got), got)
	}
	if got[1].Time != "20:30-07:16" {
		t.Errorf("merged sleep slot should be 20:30-07:16: got %q", got[1].Time)
	}
	if got[1].Goal != "睡觉" {
		t.Errorf("merged slot goal = %q, want 睡觉", got[1].Goal)
	}
}

// TestNormalizeDailyPlan_NoMergeDifferentGoals 验证相邻时段 goal 不同时
// 不合并。
func TestNormalizeDailyPlan_NoMergeDifferentGoals(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-12:00", Goal: "上午工作"},
		{Time: "12:00-18:00", Goal: "下午工作"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2 (different goals must not merge)", len(got))
	}
}

// TestNormalizeDailyPlan_NoMergeSleepSynonyms 验证措辞不同的相邻睡眠时段
// 不再被自动合并（睡眠语义合并已移除，计划保留 LLM 原样输出）。
func TestNormalizeDailyPlan_NoMergeSleepSynonyms(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-20:49", Goal: "白天活动"},
		{Time: "20:49-23:25", Goal: "休眠舱居住区睡眠舱休息"},
		{Time: "23:25-06:35", Goal: "休眠舱居住区睡眠舱睡眠"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (different-goal sleep slots must not merge): %+v", len(got), got)
	}
}

// TestNormalizeDailyPlan_NoMergeRunBeforeSleep 验证跑步锻炼与后续睡眠不会
// 被合并（睡眠语义合并移除后天然不合并；此测试锁定该行为不被重新引入）。
func TestNormalizeDailyPlan_NoMergeRunBeforeSleep(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:00-20:00", Goal: "白天活动"},
		{Time: "20:00-22:00", Goal: "休眠舱居住区跑步机跑步锻炼"},
		{Time: "22:00-07:00", Goal: "休眠舱睡眠"},
	}
	got := normalizeDailyPlan(items, dayStartMinute)
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (run and sleep must not merge): %+v", len(got), got)
	}
	if got[1].Goal != "休眠舱居住区跑步机跑步锻炼" {
		t.Errorf("run slot goal lost: %q", got[1].Goal)
	}
	if got[1].Time != "20:00-22:00" {
		t.Errorf("run slot = %q, want 20:00-22:00", got[1].Time)
	}
	if got[2].Goal != "休眠舱睡眠" {
		t.Errorf("sleep slot goal lost: %q", got[2].Goal)
	}
	if got[2].Time != "22:00-07:00" {
		t.Errorf("sleep slot = %q, want 22:00-07:00", got[2].Time)
	}
}

// TestClampNightEnd 验证跨午夜末段结束时间的 06:00 钳位：
// 早于 06:00 → 钳到 06:00；已达 06:00 及以后 / 非跨午夜 → 不变。
func TestClampNightEnd(t *testing.T) {
	cases := []struct {
		last, want string
	}{
		{"22:00-05:50", "22:00-06:00"}, // 被 jitter 拉早的夜间结束 → 钳回
		{"23:29-00:54", "23:29-06:00"}, // 凌晨短睡眠 → 钳回
		{"22:00-06:00", "22:00-06:00"}, // 恰好 06:00 → 不变
		{"22:38-06:58", "22:38-06:58"}, // 覆盖过 06:00 → 不变
		{"12:00-22:00", "12:00-22:00"}, // 非跨午夜 → 不变（由规则 5 处理）
	}
	for _, c := range cases {
		items := []dailyPlanItem{
			{Time: "07:00-12:00", Goal: "白天"},
			{Time: c.last, Goal: "休眠"},
		}
		got := clampNightEnd(items)
		if got[len(got)-1].Time != c.want {
			t.Errorf("clampNightEnd(last=%q) = %q, want %q", c.last, got[len(got)-1].Time, c.want)
		}
	}
}

// TestJitterPlanNodes_NightEndNotBeforeSix 验证 jitter + clampNightEnd 后
// 跨午夜末段的结束时间始终 ≥ 06:00（随机扰动多轮迭代验证不变量）。
func TestJitterPlanNodes_NightEndNotBeforeSix(t *testing.T) {
	base := []dailyPlanItem{
		{Time: "07:00-12:00", Goal: "上午"},
		{Time: "12:00-22:00", Goal: "下午"},
		{Time: "22:00-06:20", Goal: "夜间休息"},
	}
	for i := 0; i < 200; i++ {
		got := clampNightEnd(jitterPlanNodes(base, planJitterMinutes, dayStartMinute))
		_, e, ok := prompt.SplitPlanRange(got[len(got)-1].Time)
		if !ok {
			t.Fatalf("iteration %d: unparseable last slot %q", i, got[len(got)-1].Time)
		}
		if e < 6*60 {
			t.Fatalf("iteration %d: night end %q before 06:00 (e=%d)", i, got[len(got)-1].Time, e)
		}
	}
}

// jitterPlanNodes 的性质是随机的，单测用多轮迭代验证不变量而非精确值。
func jitterTestPlan() []dailyPlanItem {
	return []dailyPlanItem{
		{Time: "07:00-12:00", Goal: "上午装配"},
		{Time: "12:00-14:00", Goal: "午间休息"},
		{Time: "14:00-18:00", Goal: "下午装配"},
		{Time: "18:00-22:00", Goal: "傍晚充电"},
		{Time: "22:00-06:00", Goal: "夜间休眠"},
	}
}

func TestJitterPlanNodes_ZeroJitterNoop(t *testing.T) {
	items := jitterTestPlan()
	got := jitterPlanNodes(items, 0, dayStartMinute)
	for i := range items {
		if got[i].Time != items[i].Time {
			t.Errorf("maxJitter=0 should be a no-op: got[%d]=%q want %q", i, got[i].Time, items[i].Time)
		}
	}
}

func TestJitterPlanNodes_OffsetsWithinRange(t *testing.T) {
	orig := jitterTestPlan()
	for round := 0; round < 100; round++ {
		got := jitterPlanNodes(orig, planJitterMinutes, dayStartMinute)
		for i := range orig {
			os, oe, _ := prompt.SplitPlanRange(orig[i].Time)
			gs, ge, _ := prompt.SplitPlanRange(got[i].Time)
			// 跨午夜段 end 用归一化坐标比较。
			if oe <= os {
				oe += 1440
			}
			if ge <= gs {
				ge += 1440
			}
			for _, d := range []int{gs - os, ge - oe} {
				if d < -planJitterMinutes || d > planJitterMinutes {
					t.Fatalf("round %d slot %d: offset %d out of ±%d (orig %q got %q)",
						round, i, d, planJitterMinutes, orig[i].Time, got[i].Time)
				}
			}
		}
	}
}

func TestJitterPlanNodes_ContiguityAndMinDuration(t *testing.T) {
	orig := jitterTestPlan()
	for round := 0; round < 100; round++ {
		got := jitterPlanNodes(orig, planJitterMinutes, dayStartMinute)
		for i := range got {
			if i < len(got)-1 {
				// 共享边界：前段（非跨午夜）扰动后的 end 必须等于后段 start。
				es := strings.SplitN(got[i].Time, "-", 2)[1]
				ss := strings.SplitN(got[i+1].Time, "-", 2)[0]
				if es != ss {
					t.Fatalf("round %d: shared boundary broken between %q and %q",
						round, got[i].Time, got[i+1].Time)
				}
			}
			if d := prompt.SlotDurationMinute(got[i].Time); d < planJitterMinGap {
				t.Fatalf("round %d slot %d: duration %d < min gap %d (%q)",
					round, i, d, planJitterMinGap, got[i].Time)
			}
		}
	}
}

func TestJitterPlanNodes_OvernightSlotStaysOvernight(t *testing.T) {
	orig := jitterTestPlan()
	for round := 0; round < 100; round++ {
		got := jitterPlanNodes(orig, planJitterMinutes, dayStartMinute)
		last := got[len(got)-1]
		s, e, ok := prompt.SplitPlanRange(last.Time)
		if !ok || e > s {
			t.Fatalf("round %d: overnight slot should stay cross-midnight, got %q", round, last.Time)
		}
	}
}

func TestJitterPlanNodes_GoalsPreserved(t *testing.T) {
	orig := jitterTestPlan()
	got := jitterPlanNodes(orig, planJitterMinutes, dayStartMinute)
	for i := range orig {
		if got[i].Goal != orig[i].Goal {
			t.Errorf("slot %d goal changed: got %q want %q", i, got[i].Goal, orig[i].Goal)
		}
	}
}

func TestJitterPlanString_UnparseableNoop(t *testing.T) {
	in := "not a plan"
	if got := jitterPlanString(in); got != in {
		t.Errorf("unparseable plan should be returned unchanged, got %q", got)
	}
}

func TestFmtMinute(t *testing.T) {
	cases := []struct {
		min  int
		want string
	}{
		{0, "00:00"},
		{360, "06:00"},
		{1320, "22:00"},
		{901, "15:01"},
		// 跨午夜归一化（m >= 1440 取模）
		{1440, "00:00"},
		{1800, "06:00"}, // 次日 06:00 = 1440+360
		{1500, "01:00"}, // 次日 01:00 = 1440+60
	}
	for _, c := range cases {
		if got := prompt.FmtMinute(c.min); got != c.want {
			t.Errorf("prompt.FmtMinute(%d) = %q, want %q", c.min, got, c.want)
		}
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

// TestMatchPlanSlot_OvernightMorningHalfAfterDayStart 验证跨午夜时段的
// 凌晨半段在 07:00（dayStartMinute）之后不再命中：07:00 后生效的必为当天
// 新生成的计划，其中的跨午夜时段属于今晚——清晨命中它会把今晚的睡觉当作
// 当前时段分解下发（实测：末段 22:21-07:25 在 07:02 命中，NPC 一早回舱睡觉）。
// 07:00 前的凌晨时间仍应命中（此时生效的是前一天的旧计划，夜间时段在进行中）。
func TestMatchPlanSlot_OvernightMorningHalfAfterDayStart(t *testing.T) {
	items := []dailyPlanItem{
		{Time: "07:22-09:45", Goal: "装配"},
		{Time: "09:45-11:48", Goal: "拆解"},
		{Time: "22:21-07:25", Goal: "休眠"},
	}
	cases := []struct {
		tod  string
		want string
	}{
		{"03:00", "22:21-07:25"}, // 凌晨：旧计划夜间时段进行中 → 命中
		{"06:30", "22:21-07:25"}, // 06:00-07:00 规划窗口（matchPlanSlot 层面仍命中，selectCurrentGoal 另有屏蔽）
		{"07:02", ""},            // 07:00 后：今晚的睡觉段不得命中（首段 07:22 尚未开始）
		{"07:23", "07:22-09:45"}, // 首段开始后正常命中
		{"23:00", "22:21-07:25"}, // 晚间半段正常命中
	}
	for _, c := range cases {
		if got := matchPlanSlot(items, c.tod); got != c.want {
			t.Errorf("matchPlanSlot(items, %q) = %q, want %q", c.tod, got, c.want)
		}
	}
}

// TestSelectCurrentGoal_OvernightSleepNotDispatchedMorning 验证 07:00 后
// selectCurrentGoal 不会选中当天新计划的跨午夜睡觉段（不下发凌晨睡眠）。
func TestSelectCurrentGoal_OvernightSleepNotDispatchedMorning(t *testing.T) {
	plan := "07:22-09:45: 装配\n09:45-11:48: 拆解\n22:21-07:25: 休眠"
	// 07:02：首段未开始、夜间段不得命中 → 无当前 goal，本轮 idle 等待
	goal, slot, idx := selectCurrentGoal(plan, "07:02")
	if goal != "" || slot != "" || idx != -1 {
		t.Errorf("selectCurrentGoal(plan, 07:02) = (%q, %q, %d), want empty", goal, slot, idx)
	}
	// 凌晨 03:00（旧计划语义）：命中夜间段（selectCurrentGoal 不屏蔽 06:00 前）
	goal, slot, _ = selectCurrentGoal(plan, "03:00")
	if goal != "休眠" || slot != "22:21-07:25" {
		t.Errorf("selectCurrentGoal(plan, 03:00) = (%q, %q), want 休眠/22:21-07:25", goal, slot)
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

// TestBuildSharedSystemPrompt_NoZoneObjectMapHeader 【区域设施映射】段
// 已移除（模块 3 设施详情逐组标注所在 zone，信息不冗余）。
// 日后若 LLM 又出现 zone-object 错配可重新启用并恢复此断言。
func TestBuildSharedSystemPrompt_NoZoneObjectMapHeader(t *testing.T) {
	kb := loadTestKB(t)
	got := prompt.BuildSharedSystemPrompt(kb, nil, "H-01")
	if strings.Contains(got, "【区域设施映射】") {
		t.Errorf("system prompt should NOT include '【区域设施映射】' section (superseded by module 3): %q", got)
	}
}

// TestTriggerStrategicPlanning_ReadsTimeAndZoneFromSnapshot 验证通用战略
// 触发钩子从快照读当前游戏时间与所在区域拼 user prompt，并携带 hint。
func TestTriggerStrategicPlanning_ReadsTimeAndZoneFromSnapshot(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	// 预置感知：time_of_day_sec 14:30（52200 秒），zone=main_workshop。
	raw := []byte(`{"environment":{"game_time_sec":52200,"time_of_day_sec":52200,"day_count":1},"location":{"current_zone":"main_workshop"}}`)
	if _, err := as.SetPerception(raw); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`[{"time":"14:30-18:00","goal":"下午工作"}]`)}
	ac := &agentContext{as: as, strategicHc: sc}

	_ = ac.triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "中午状态变化重规划", "")

	if !strings.Contains(sc.capturedInput, "现在是仿真时间 14:30") {
		t.Errorf("user prompt should carry snapshot time 14:30:\n%s", sc.capturedInput)
	}
	if !strings.Contains(sc.capturedInput, "你当前位于main_workshop") {
		t.Errorf("user prompt should carry snapshot zone main_workshop:\n%s", sc.capturedInput)
	}
	if !strings.Contains(sc.capturedInput, "中午状态变化重规划") {
		t.Errorf("user prompt should carry hint:\n%s", sc.capturedInput)
	}
}

// TestTriggerStrategicPlanning_MorningFallbackNoPerception 验证冷启动（首条
// 感知未到）时时间回落 07:00 且省略位置段。
func TestTriggerStrategicPlanning_MorningFallbackNoPerception(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`[{"time":"07:00-08:00","goal":"晨练"}]`)}
	ac := &agentContext{as: as, strategicHc: sc}

	_ = ac.triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")

	if !strings.Contains(sc.capturedInput, "现在是仿真时间 07:00") {
		t.Errorf("cold start should fall back to 07:00:\n%s", sc.capturedInput)
	}
	if strings.Contains(sc.capturedInput, "你当前位于") {
		t.Errorf("cold start without perception should omit location clause:\n%s", sc.capturedInput)
	}
	if !strings.Contains(sc.capturedInput, "早晨例行制定每日日程安排") {
		t.Errorf("morning hint missing:\n%s", sc.capturedInput)
	}
}

// TestTriggerStrategicPlanning_CrossDayUsesFixedStart 验证跨日触发（planningStart
// 传 "07:00"）时，即使快照当前游戏时间是深夜，prompt 仍用 07:00 作为规划
// 起点，避免 LLM 只规划"睡觉到清晨"的简略计划。
func TestTriggerStrategicPlanning_CrossDayUsesFixedStart(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	// 预置感知：深夜 00:09（day_count 已递增，模拟 UE 在 00:00 即推进游戏日）。
	raw := []byte(`{"environment":{"game_time_sec":540,"time_of_day_sec":540,"day_count":2},"location":{"current_zone":"residential_quarters"}}`)
	if _, err := as.SetPerception(raw); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`[{"time":"07:00-08:00","goal":"晨练"}]`)}
	ac := &agentContext{as: as, strategicHc: sc}

	_ = ac.triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "07:00")

	// 规划起点应为 07:00（而非快照的深夜 00:09）。
	if !strings.Contains(sc.capturedInput, "现在是仿真时间 07:00") {
		t.Errorf("cross-day planning should use 07:00 start, got:\n%s", sc.capturedInput)
	}
	if strings.Contains(sc.capturedInput, "现在是仿真时间 00:09") {
		t.Errorf("cross-day planning should NOT use the late-night snapshot time 00:09:\n%s", sc.capturedInput)
	}
}

// TestTriggerStrategicPlanning_PlanningWindowClampsToStart 验证冷启动落在
// 规划窗口（06:00-07:00）时被钳位到 07:00：UE 每次启动 game_time 从 06:00
// 起，若直接以 06:00 为规划起点，会生成"05:59 起的晨间冥想"这类规划窗口
// 内活动段（2026-09-03 实测 H-01 计划 05:59-06:51），而该窗口是战略层
// 规划时间、不应安排 schedule。
func TestTriggerStrategicPlanning_PlanningWindowClampsToStart(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	// 预置感知：UE 刚启动的 06:30（规划窗口内）。
	raw := []byte(`{"environment":{"game_time_sec":23400,"time_of_day_sec":23400,"day_count":0},"location":{"current_zone":"residential_quarters"}}`)
	if _, err := as.SetPerception(raw); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`[{"time":"07:00-08:00","goal":"晨练"}]`)}
	ac := &agentContext{as: as, strategicHc: sc}

	plan := ac.triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "早晨例行制定每日日程安排", "")

	if !strings.Contains(sc.capturedInput, "现在是仿真时间 07:00") {
		t.Errorf("cold start inside planning window should clamp to 07:00, got:\n%s", sc.capturedInput)
	}
	// 产出的计划首段不早于 07:00（LLM 从 07:00 规划 + jitter 下界钳位）。
	items := parseFormattedPlan(plan)
	if len(items) == 0 {
		t.Fatalf("plan should have items, got %q", plan)
	}
	if s, _, _ := prompt.SplitPlanRange(items[0].Time); s < dayStartMinute {
		t.Errorf("plan first slot starts at %d (before 07:00): %q", s, items[0].Time)
	}
}

// TestTriggerStrategicPlanning_EarlyMorningNotClamped 验证凌晨（<06:00）
// 不被钳位——旧计划的跨午夜末段仍在进行，中途触发按当前时间规划。
func TestTriggerStrategicPlanning_EarlyMorningNotClamped(t *testing.T) {
	as := agentstate.New()
	as.SetIdentity("H-01", nil)
	raw := []byte(`{"environment":{"game_time_sec":9000,"time_of_day_sec":9000,"day_count":1},"location":{"current_zone":"residential_quarters"}}`)
	if _, err := as.SetPerception(raw); err != nil {
		t.Fatalf("SetPerception: %v", err)
	}
	sc := &fakeStrategicCaller{resp: makeStrategicResponse(`[{"time":"02:30-07:00","goal":"继续休眠"}]`)}
	ac := &agentContext{as: as, strategicHc: sc}

	_ = ac.triggerStrategicPlanning(context.Background(), "H-01", nil, nil, slog.Default(), "", "", "测试触发", "")

	if !strings.Contains(sc.capturedInput, "现在是仿真时间 02:30") {
		t.Errorf("early-morning mid-day trigger should keep current time, got:\n%s", sc.capturedInput)
	}
}

// TestJitterPlanNodes_FirstNodeNotBeforeStart 验证扰动不把首段起点抖早于
// 规划语义起点（minStart）——多轮随机迭代验证不变量。
func TestJitterPlanNodes_FirstNodeNotBeforeStart(t *testing.T) {
	orig := jitterTestPlan() // 首段 07:00-12:00
	for round := 0; round < 200; round++ {
		got := jitterPlanNodes(orig, planJitterMinutes, dayStartMinute)
		s, _, ok := prompt.SplitPlanRange(got[0].Time)
		if !ok {
			t.Fatalf("round %d: unparseable first slot %q", round, got[0].Time)
		}
		if s < dayStartMinute {
			t.Fatalf("round %d: first slot %q starts %d before minStart %d",
				round, got[0].Time, s, dayStartMinute)
		}
	}
}
