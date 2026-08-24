package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// sysPrompt renders the strategic system prompt with a nil KB / nil profiles
// (fallback persona) — the pure-rules view used by rule-text guard tests.
func sysPrompt() string {
	return BuildStrategicSystemPrompt(nil, nil, "H-01")
}

// strategicDetailKB builds a KB with narrative, zone descriptions, and objects
// carrying both verb lists (AvailableInteractions) and rate declarations
// (Extra) — the in-memory shape after a world_kb push merge.
func strategicDetailKB() *worldkb.KB {
	mk := func(sg, display, zone string, verbs []string, itxs ...map[string]any) worldkb.Object {
		arr := make([]any, len(itxs))
		for i, m := range itxs {
			arr[i] = m
		}
		return worldkb.Object{
			ID: sg + "-1", DisplayName: display, SemanticGroup: sg, ZoneID: zone,
			AvailableInteractions: verbs,
			Extra:                 map[string]any{"available_interactions": arr},
		}
	}
	return &worldkb.KB{
		Version: "1.0",
		Narrative: worldkb.Narrative{
			Setting: "工业机器人小镇",
			Theme:   "一座由机器人居民维持生产和日常生活的封闭工业园区。",
		},
		Zones: []worldkb.Zone{
			{ID: "main_workshop", DisplayName: "主生产车间", Description: "小镇的生产核心，机器人居民在这里完成装配、质检和设备调试。"},
			{ID: "central_plaza", DisplayName: "中央广场", Description: "小镇的公共交通、社交和充能中心。"},
		},
		Objects: []worldkb.Object{
			mk("workbench", "工作台", "main_workshop", []string{"assemble"}, map[string]any{
				"name":                  "assemble",
				"description":           "在工作台上进行零件装配，产出成品",
				"energy_delta_per_hour": -4.0, "fatigue_delta_per_hour": 12.0,
				"joint_wear_delta_per_hour": 1.4, "money_delta_per_hour": 30.0,
				"min_energy_to_use": 10.0, "max_fatigue_to_use": 80.0,
			}),
			mk("sleep_pod", "睡眠舱", "residential_quarters", []string{"sleep", "meditate", "tidy_up"},
				map[string]any{"name": "sleep", "description": "进入休眠舱休息，恢复精力和状态", "fatigue_delta_per_hour": -25.0},
				map[string]any{"name": "meditate", "description": "坐在床上冥想，时间不宜太长", "fatigue_delta_per_hour": -10.0},
				map[string]any{"name": "tidy_up", "description": "整理内务：整理自己的私人物品和床铺，保持整洁"},
			),
			// 无速率声明（仅动词列表）：交互行只列动词。
			{ID: "legacy-1", DisplayName: "旧装置", SemanticGroup: "legacy", ZoneID: "main_workshop",
				AvailableInteractions: []string{"poke"}},
		},
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈"},
			{ID: "H-02", DisplayName: "老王"},
		},
	}
}

// TestBuildStrategicSystemPrompt_ThreeModules verifies the three-module
// structure: 【世界背景】(overview) → 【人物背景】(profile) →
// 【世界详细信息】(details). The seven rules live in the user message
// (StrategicRules + StrategicUserTemplate), not here.
func TestBuildStrategicSystemPrompt_ThreeModules(t *testing.T) {
	got := BuildStrategicSystemPrompt(strategicDetailKB(), nil, "H-01")
	overIdx := strings.Index(got, "【世界背景】")
	roleIdx := strings.Index(got, "【人物背景】")
	detailIdx := strings.Index(got, "【世界详细信息】")
	if overIdx < 0 || roleIdx < 0 || detailIdx < 0 {
		t.Fatalf("missing modules (bg=%d role=%d detail=%d):\n%s", overIdx, roleIdx, detailIdx, got)
	}
	if !(overIdx < roleIdx && roleIdx < detailIdx) {
		t.Errorf("module order wrong: bg=%d role=%d detail=%d", overIdx, roleIdx, detailIdx)
	}
	// 规则已迁至 user prompt，system prompt 不再包含。
	if strings.Contains(got, "规划要求：") {
		t.Errorf("system prompt should not contain the rules module (moved to user prompt):\n%s", got)
	}
	// 模块 1 世界背景：narrative + 三份名册。
	for _, want := range []string{
		"设定：工业机器人小镇",
		"主题：",
		"区域（2 个）：主生产车间（main_workshop）、中央广场（central_plaza）",
		"可交互设施类别",
		"居民（2 位）：老陈（H-01）、老王（H-02）",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("module 1 missing %q:\n%s", want, got)
		}
	}
	// 模块 2 人物背景：fallback persona。
	if !strings.Contains(got, "名字：老陈") {
		t.Errorf("module 2 missing fallback persona name:\n%s", got)
	}
	// 七条规则（user prompt 注入）+ 格式示例。
	if !strings.Contains(StrategicRules, "格式示例：[{\"time\":\"07:00-9:00\"") {
		t.Errorf("StrategicRules missing format example:\n%s", StrategicRules)
	}
	// user 模板第三个占位符注入规则。
	if !strings.Contains(StrategicUserTemplate, "规划要求：\n%s") {
		t.Errorf("user template should carry the rules placeholder:\n%s", StrategicUserTemplate)
	}
}

// TestBuildStrategicSystemPrompt_Module3Details verifies module 3 renders
// zone descriptions, per-group facilities with inline per-interaction
// description + per-hour effects + gates, and the composite cmd list.
func TestBuildStrategicSystemPrompt_Module3Details(t *testing.T) {
	got := BuildStrategicSystemPrompt(strategicDetailKB(), nil, "H-01")
	// zone 描述。
	if !strings.Contains(got, "主生产车间（main_workshop）：小镇的生产核心") {
		t.Errorf("module 3 missing zone description:\n%s", got)
	}
	// 各区域可交互设施映射表：按 zone 列出 semantic_group。
	if !strings.Contains(got, "各区域可交互设施：\n- 主生产车间（main_workshop）：旧装置（legacy）、工作台（workbench）") {
		t.Errorf("module 3 missing per-zone facility map for workbench:\n%s", got)
	}
	// 设施组 + 内联交互效果（不再带"位于"）。
	if !strings.Contains(got, "- 工作台（workbench）：\n  - assemble：") {
		t.Errorf("module 3 missing workbench group:\n%s", got)
	}
	if strings.Contains(got, "工作台（workbench），位于") {
		t.Errorf("module 3 facility detail should not carry per-object zone, got:\n%s", got)
	}
	if !strings.Contains(got, "  - assemble：在工作台上进行零件装配，产出成品。能量中等下降、疲劳度明显提升、关节磨损少量累积、余额快速增加。使用门槛：能量≥10、疲劳≤80") {
		t.Errorf("module 3 missing inline effect line for assemble:\n%s", got)
	}
	if !strings.Contains(got, "  - meditate：坐在床上冥想，时间不宜太长。疲劳度明显缓解") {
		t.Errorf("module 3 missing inline effect line for meditate:\n%s", got)
	}
	// 无速率声明的交互只列动词。
	if !strings.Contains(got, "  - poke") {
		t.Errorf("module 3 should still list rate-less interaction verb poke:\n%s", got)
	}
	// 复合动作清单不注入战略层（cmd 选择是战术层职责）。
	if strings.Contains(got, "复合动作（长时段活动用") {
		t.Errorf("module 3 should NOT contain the composite cmd section:\n%s", got)
	}
	if strings.Contains(got, "work_shift") {
		t.Errorf("module 3 should NOT list composite cmds:\n%s", got)
	}
}

// TestBuildStrategicUserContext_DynamicSegments 已移除：用户未提交的
// strategic.go 改动把战略层 preamble（引用模块名 + 属性权衡说明）从
// system prompt 移到 user context，本测试的"user context 不得含模块名"
// 断言随之过时。

func TestBuildStrategicUserContext_EmptyDayContext(t *testing.T) {
	got := BuildStrategicUserContext("H-01", nil, nil, "")
	if strings.Contains(got, "【今日日程】") {
		t.Errorf("empty dayContext should not produce 【今日日程】 segment in:\n%s", got)
	}
}

// strategicRosterKB builds a minimal KB with 3 agents for roster-segment tests.
func strategicRosterKB() *worldkb.KB {
	return &worldkb.KB{
		Version: "1.0",
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈", Profession: "装配工人"},
			{ID: "H-02", DisplayName: "老王", Profession: "物流分拣员"},
			{ID: "H-03", DisplayName: "老李", Profession: "精密装配技术员"},
		},
	}
}

func TestOtherAgentsLine_NilOrSparseKB(t *testing.T) {
	if got := OtherAgentsLine(nil, "H-01"); got != "" {
		t.Errorf("nil kb should return empty, got %q", got)
	}
	single := &worldkb.KB{Agents: []worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}}}
	if got := OtherAgentsLine(single, "H-01"); got != "" {
		t.Errorf("single-agent KB should return empty (no peers), got %q", got)
	}
}

func TestOtherAgentsLine_SkipsSelfAndFormats(t *testing.T) {
	kb := strategicRosterKB()
	got := OtherAgentsLine(kb, "H-01")
	if strings.Contains(got, "H-01") {
		t.Errorf("self H-01 should not appear in roster:\n%s", got)
	}
	if !strings.Contains(got, "老王（id=H-02）职业：物流分拣员") {
		t.Errorf("H-02 line missing or malformed in:\n%s", got)
	}
	if !strings.Contains(got, "老李（id=H-03）职业：精密装配技术员") {
		t.Errorf("H-03 line missing or malformed in:\n%s", got)
	}
}

func TestOtherAgentsLine_FallsBackToIDWhenNameEmpty(t *testing.T) {
	kb := &worldkb.KB{Agents: []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-02", Profession: "分拣员"}, // DisplayName 空，应回退到 id
	}}
	got := OtherAgentsLine(kb, "H-01")
	if !strings.Contains(got, "H-02（id=H-02）职业：分拣员") {
		t.Errorf("empty DisplayName should fall back to id as name in:\n%s", got)
	}
}

func TestOtherAgentsLine_OmitsProfessionWhenEmpty(t *testing.T) {
	kb := &worldkb.KB{Agents: []worldkb.Agent{
		{ID: "H-01", DisplayName: "老陈"},
		{ID: "H-02", DisplayName: "老王"}, // Profession 空，应省略职业段
	}}
	got := OtherAgentsLine(kb, "H-01")
	// 单 peer 时 TrimSuffix 去掉尾部换行，所以只检查名字行存在且无职业字段。
	if !strings.Contains(got, "老王（id=H-02）") {
		t.Errorf("peer line missing in:\n%s", got)
	}
	if strings.Contains(got, "职业：") {
		t.Errorf("empty Profession should omit 职业 field in:\n%s", got)
	}
}

// TestBuildStrategicSystemPrompt_OmitsOtherNPCsSegment 战略层暂不安排社交：
// 【其他NPC】段不出现（世界背景的居民名册仅为概况列表）。
// 恢复社交安排时删掉本测试并把段加回。
func TestBuildStrategicSystemPrompt_OmitsOtherNPCsSegment(t *testing.T) {
	kb := strategicRosterKB()
	got := BuildStrategicSystemPrompt(kb, nil, "H-01")
	if strings.Contains(got, "【其他NPC】\n") {
		t.Errorf("strategic prompt should not contain 【其他NPC】 segment while social planning is disabled:\n%s", got)
	}
}

func TestStrategicSystemPrompt_NoSocialWhileDisabled(t *testing.T) {
	// 社交描述暂时撤除：system prompt 不应引导 LLM 安排 social_chat 时段。
	// 注意：复合动作清单（模块 3）仍会列出 social_chat cmd 本身（能力事实），
	// 这里只守卫机制文本不出现社交引导。恢复社交安排时删掉本测试。
	for _, unwanted := range []string{"【社交】", "社交时段"} {
		if strings.Contains(sysPrompt(), unwanted) {
			t.Errorf("strategic system prompt should not mention %q while social planning is disabled", unwanted)
		}
	}
}

func TestStrategicSystemPrompt_GoalMustBeString(t *testing.T) {
	// L1（json_schema）之外的软约束：goal 字段类型显式声明。
	// 起因：实际仿真中 LLM 把 goal 写成 {"goal":"...","cmd":"..."} 导致整包解析失败。
	if !strings.Contains(StrategicRules, `"goal"（一句话，必须是纯文本字符串）`) {
		t.Error("system prompt should declare goal must be a plain string")
	}
}

func TestStrategicSystemPrompt_GoalStyleTerse(t *testing.T) {
	// goal 文案干练简洁（做什么+在哪），人设语气不得进入 schedule 文字
	// ——语气只属于战术层的 speak 动作。
	if !strings.Contains(StrategicRules, "goal 用干练简洁的客观描述") {
		t.Error("system prompt should require terse objective goal text")
	}
	if !strings.Contains(StrategicRules, "不带语气词、口头禅、内心独白或人设腔调") {
		t.Error("system prompt should ban persona tone in goal text")
	}
	if !strings.Contains(StrategicUserTemplate, "goal 文字一律干练简洁，不带人设语气") {
		t.Error("user template should reiterate terse goal style")
	}
}

func TestStrategicUserTemplate_EndsWithFormatReminder(t *testing.T) {
	// user 消息末尾的格式提醒（recency effect：越靠后的指令遵从率越高）。
	// 含 ≥120 分钟硬性约束的重复强调。
	if !strings.Contains(StrategicUserTemplate, `"goal" 必须是字符串`) {
		t.Error("user template should end with the JSON format reminder")
	}
	if !strings.Contains(StrategicUserTemplate, "每个时段必须 ≥120 分钟") {
		t.Error("user template should reiterate the ≥120-minute slot rule")
	}
}

func TestStrategicSystemPrompt_SlotDurationRuleEmphasized(t *testing.T) {
	// 规则 2 标记为硬性要求，且说明不足 60 分钟时的处理方式（并入/不安排）。
	if !strings.Contains(StrategicRules, "【硬性要求】每个时段的结束时间减去开始时间必须 ≥60 分钟") {
		t.Error("system prompt rule 2 should be marked as a hard ≥60-minute requirement")
	}
	if !strings.Contains(StrategicRules, "并入相邻时段") {
		t.Error("system prompt should say short activities merge into adjacent slots")
	}
}

func TestStrategicSystemPrompt_PlanStartsAtSeven(t *testing.T) {
	// 规则 5：第一个时段从 07:00 开始，任何时段开始不得早于 07:00，
	// 禁止 0:00-7:00 这类凌晨睡觉时段（凌晨睡眠由跨午夜末段覆盖）。
	for _, want := range []string{
		"第一个时段必须从 07:00 开始",
		"任何时段的开始时间不得早于 07:00",
		"禁止输出 0:00-7:00",
	} {
		if !strings.Contains(StrategicRules, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// user 模板收尾指令同样强调。
	if !strings.Contains(StrategicUserTemplate, "任何时段的开始时间不得早于 07:00") {
		t.Error("user template should reiterate the no-early-start rule")
	}
}

// TestStrategicSystemPrompt_NightSleepContinuous 验证规则 5 约束夜间睡眠
// 为单一连续跨午夜时段（不拆分、不提前结束）——防止 currentSlot 半夜到期
// 打断睡眠重新分解（"睡着睡着爬起来重睡"）。
func TestStrategicSystemPrompt_NightSleepContinuous(t *testing.T) {
	for _, want := range []string{
		"夜间睡眠必须是一个连续的跨午夜时段",
		"不得拆成多个睡眠时段",
		"不得在凌晨提前结束",
	} {
		if !strings.Contains(StrategicRules, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestStrategicSystemPrompt_InteractWorkGuidance 验证规则 3 告知
// goal 可映射到设施详情中的任意 (semantic_group, interaction) 组合
// （无复合动作工种的规划依据），且战术层负责分解。
func TestStrategicSystemPrompt_InteractWorkGuidance(t *testing.T) {
	for _, want := range []string{
		"设施详情中列出的某个 (semantic_group, interaction) 组合",
		"战术层会据此分解为对应的移动与长时段互动",
	} {
		if !strings.Contains(StrategicRules, want) {
			t.Errorf("rule 3 should mention %q:\n%s", want, StrategicRules)
		}
	}
}

// TestStrategicSystemPrompt_Rule3AtomicInteractionGate 验证规则 3 把
// (semantic_group, interaction) 组合列为合法映射——新动态互动（如
// meditate/tidy_up）由此获得准入，不再被"无 cmd 对应→换一个"拦下。
func TestStrategicSystemPrompt_Rule3AtomicInteractionGate(t *testing.T) {
	for _, want := range []string{
		"某个 (semantic_group, interaction) 组合",
		"睡眠舱的 sleep/meditate/tidy_up",
		"锻炼类活动（晨练拉伸等原地动作）不需要设施，属例外",
	} {
		if !strings.Contains(StrategicRules, want) {
			t.Errorf("rules missing %q", want)
		}
	}
	// 规则 5 早间露出。
	if !strings.Contains(StrategicRules, "冥想醒神、整理舱位") {
		t.Error("rules should expose 冥想醒神/整理舱位 as morning options")
	}
	if !strings.Contains(StrategicRules, "晨练拉伸") {
		t.Error("rules should expose 晨练拉伸 (exercise) as a morning option")
	}
}

// TestStrategicSystemPrompt_NoHardcodedEffects 已移除：用户未提交的
// strategic.go 改动把 preamble（含"属性变动"引用）移到 user context 并
// 改写 worldDetailCore 标题，本测试的"preamble 应引用属性变动"断言过时。
