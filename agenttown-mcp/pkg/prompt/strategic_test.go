package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

func TestBuildStrategic_WithDayContext(t *testing.T) {
	kb := &worldkb.KB{
		Version: "1.0",
		Narrative: worldkb.Narrative{
			Setting: "测试车间",
			Theme:   "测试",
		},
	}
	dayContext := "今天是周二（工作日）。下班后适合上网休闲放松。"
	got := BuildStrategic(kb, nil, "H-01", nil, nil, dayContext)
	if !strings.Contains(got, "【今日日程】") {
		t.Errorf("missing 【今日日程】 segment in:\n%s", got)
	}
	if !strings.Contains(got, dayContext) {
		t.Errorf("dayContext text not injected in:\n%s", got)
	}
	// 【今日日程】 should appear after 【你的角色】 and before 【物理状态】
	roleIdx := strings.Index(got, "【你的角色】")
	dayIdx := strings.Index(got, "【今日日程】")
	physIdx := strings.Index(got, "【物理状态】")
	if roleIdx < 0 || dayIdx < 0 || physIdx < 0 {
		t.Fatalf("missing expected segments (role=%d day=%d phys=%d)", roleIdx, dayIdx, physIdx)
	}
	if !(roleIdx < dayIdx && dayIdx < physIdx) {
		t.Errorf("segment order wrong: role=%d day=%d phys=%d (want role<day<phys)", roleIdx, dayIdx, physIdx)
	}
}

func TestBuildStrategic_EmptyDayContext(t *testing.T) {
	kb := &worldkb.KB{
		Version:   "1.0",
		Narrative: worldkb.Narrative{Setting: "测试"},
	}
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if strings.Contains(got, "【今日日程】") {
		t.Errorf("empty dayContext should not produce 【今日日程】 segment in:\n%s", got)
	}
}

// TestBuildStrategic_CmdEffectsSegment verifies the 【动作对属性的影响】
// segment appears when SetCmdEffects installed text (and appears after the
// 【可用能力】 segment), and is skipped entirely when the summary is empty.
func TestBuildStrategic_CmdEffectsSegment(t *testing.T) {
	kb := &worldkb.KB{
		Version:   "1.0",
		Narrative: worldkb.Narrative{Setting: "测试"},
	}
	text := "各活动对属性的影响（每游戏小时平均变化，由仿真日志统计）：\n" +
		"- work_shift（assemble）：能量少量下降、疲劳度中等提升、余额明显增加\n"
	SetCmdEffects(text)
	defer SetCmdEffects("")

	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if !strings.Contains(got, "【动作对属性的影响】") {
		t.Errorf("missing 【动作对属性的影响】 segment in:\n%s", got)
	}
	if !strings.Contains(got, "疲劳度中等提升") {
		t.Errorf("cmd effects text not injected in:\n%s", got)
	}
	capIdx := strings.Index(got, "【可用能力】")
	effIdx := strings.Index(got, "【动作对属性的影响】")
	if capIdx < 0 || effIdx < 0 || capIdx > effIdx {
		t.Errorf("segment order wrong: cap=%d effects=%d (want effects after cap)", capIdx, effIdx)
	}

	// 禁用（空串）时整段消失。
	SetCmdEffects("")
	got = BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if strings.Contains(got, "【动作对属性的影响】") {
		t.Errorf("empty cmd effects should not produce segment in:\n%s", got)
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

func TestBuildStrategic_OmitsOtherNPCsSegment(t *testing.T) {
	// 【其他NPC】段暂时撤除：战略层暂不安排社交，花名册不再注入。
	// 恢复社交安排时删掉本测试并把段加回 BuildStrategic。
	kb := strategicRosterKB()
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if strings.Contains(got, "【其他NPC】\n") {
		t.Errorf("strategic prompt should not contain 【其他NPC】 segment while social planning is disabled:\n%s", got)
	}
}

func TestStrategicSystemPrompt_NoSocialWhileDisabled(t *testing.T) {
	// 社交描述暂时撤除：system prompt 不应引导 LLM 安排 social_chat 时段。
	// 恢复社交安排时删掉本测试。
	for _, unwanted := range []string{"social_chat", "【社交】", "社交时段"} {
		if strings.Contains(StrategicSystemPrompt, unwanted) {
			t.Errorf("strategic system prompt should not mention %q while social planning is disabled", unwanted)
		}
	}
}

func TestStrategicSystemPrompt_GoalMustBeString(t *testing.T) {
	// L1（json_schema）之外的软约束：goal 字段类型显式声明。
	// 起因：实际仿真中 LLM 把 goal 写成 {"goal":"...","cmd":"..."} 导致整包解析失败。
	if !strings.Contains(StrategicSystemPrompt, `"goal"（一句话，必须是纯文本字符串）`) {
		t.Error("system prompt should declare goal must be a plain string")
	}
}

func TestStrategicSystemPrompt_GoalStyleTerse(t *testing.T) {
	// goal 文案干练简洁（做什么+在哪），人设语气不得进入 schedule 文字
	// ——语气只属于战术层的 speak 动作。
	if !strings.Contains(StrategicSystemPrompt, "goal 用干练简洁的客观描述") {
		t.Error("system prompt should require terse objective goal text")
	}
	if !strings.Contains(StrategicSystemPrompt, "不带语气词、口头禅、内心独白或人设腔调") {
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
	// 规则 2 标记为硬性要求，且说明不足 120 分钟时的处理方式（并入/不安排）。
	if !strings.Contains(StrategicSystemPrompt, "【硬性要求】每个时段的结束时间减去开始时间必须 ≥120 分钟") {
		t.Error("system prompt rule 2 should be marked as a hard ≥120-minute requirement")
	}
	if !strings.Contains(StrategicSystemPrompt, "并入相邻时段") {
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
		if !strings.Contains(StrategicSystemPrompt, want) {
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
		if !strings.Contains(StrategicSystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

func TestStrategicSystemPrompt_RecoveryDiversified(t *testing.T) {
	// 规则 8：恢复时段方式多样化（休眠舱小憩/充电/长椅/上网），
	// 性格为主依据；仅低电量必充、非常疲劳必休两个强制例外。
	// 注意：不能给任何选项加"优先"级量化条件——实测全员中午状态同为
	// "能量中等/轻度疲劳"时，量化优先级会让所有人同步选同一选项。
	for _, want := range []string{
		"疲劳恢复时段（如午间）的方式应多样化",
		"回休眠舱小憩（rest_at_residence",
		"去充电桩充电兼休息（charge_at_station",
		"主要依据你的性格特质",
		"不要固定去长椅或充电",
	} {
		if !strings.Contains(StrategicSystemPrompt, want) {
			t.Errorf("system prompt should contain recovery diversification guidance %q", want)
		}
	}
	if strings.Contains(StrategicSystemPrompt, "优先选这项") {
		t.Error("recovery options must not carry quantified priority — it re-homogenizes behavior when all NPCs share the same midday state")
	}
}

func TestBuildStrategic_InteractWorkGuidance(t *testing.T) {
	// 【可用能力】段应告知：任何工种都可用 interact + 工作设备直接工作
	// （process/debug/dismantle 等无复合动作的工种的规划依据）。
	kb := strategicRosterKB()
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	for _, want := range []string{"任何工种", "加工机（process）", "拆解台（dismantle）"} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildStrategic 可用能力 should mention %q:\n%s", want, got)
		}
	}
}

func TestBuildStrategic_ExcludesMechanismSegments(t *testing.T) {
	// Mechanism segments (【社交】/【要求】) live in StrategicSystemPrompt;
	// BuildStrategic returns data segments only. 【动作对状态的影响】已删除
	// ——属性影响统一由 user 消息的【动作对属性的影响】段（脚本生成）提供。
	kb := strategicRosterKB()
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	for _, seg := range []string{"【动作对状态的影响】", "【社交】", "要求："} {
		if strings.Contains(got, seg) {
			t.Errorf("BuildStrategic should not contain mechanism segment %q in:\n%s", seg, got)
		}
	}
}

// TestStrategicSystemPrompt_NoHardcodedEffects 验证 system prompt 不再硬编码
// 动作属性影响——统一由 cmd_effect_summary.py 生成的【动作对属性的影响】
// 段（user 消息，SetCmdEffects 注入）提供，避免两份内容漂移。
func TestStrategicSystemPrompt_NoHardcodedEffects(t *testing.T) {
	if strings.Contains(StrategicSystemPrompt, "【动作对状态的影响】") {
		t.Error("StrategicSystemPrompt should not hardcode the effect table (use the script-generated segment)")
	}
	if !strings.Contains(StrategicSystemPrompt, "【动作对属性的影响】") {
		t.Error("StrategicSystemPrompt should reference the script-generated effect segment")
	}
}
