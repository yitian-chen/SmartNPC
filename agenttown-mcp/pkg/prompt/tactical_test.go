package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// TestBuildTactical_MemoriesInjected verifies a non-empty Memories field
// produces a 【过往经验】 segment in the prompt.
func TestBuildTactical_MemoriesInjected(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:      "车间装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Slot:      "06:00-11:00",
		Memories:  "- 完成车间装配（event）\n- 学会使用新工具（skill）",
		AgentID:   "H-01",
	})
	if !strings.Contains(out, "【过往经验】") {
		t.Error("missing 【过往经验】 segment when Memories non-empty")
	}
	if !strings.Contains(out, "完成车间装配") {
		t.Error("memory content '完成车间装配' not found in prompt")
	}
	if !strings.Contains(out, "学会使用新工具") {
		t.Error("memory content '学会使用新工具' not found in prompt")
	}
}

// TestBuildTactical_MemoriesEmpty verifies an empty Memories field produces
// no 【过往经验】 line (the segment is skipped entirely).
func TestBuildTactical_MemoriesEmpty(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:      "车间装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Slot:      "06:00-11:00",
		Memories:  "",
		AgentID:   "H-01",
	})
	if strings.Contains(out, "【过往经验】") {
		t.Error("【过往经验】 segment present when Memories empty — should be skipped")
	}
}

// TestBuildTactical_PlaceholderCount verifies all 13 %s + 1 %d placeholders
// in tacticalPromptBody are filled — no leftover %s or %!(EXTRA ...) from
// a mismatched Sprintf arity.
func TestBuildTactical_PlaceholderCount(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:          "车间装配",
		Zone:          "main_workshop",
		TimeOfDay:     "08:00",
		Slot:          "06:00-11:00",
		Hint:          "物理状态告警",
		Memories:      "- 某条记忆（event）",
		Relationships: "- 与 H-02：熟悉度 3、好感 1（互动 2 次）",
		AgentID:       "H-01",
	})
	if strings.Contains(out, "%!") {
		t.Errorf("found %%!(EXTRA ...) or similar in output, indicating placeholder arity mismatch:\n%s", out)
	}
	if strings.Contains(out, "%s") {
		t.Errorf("found leftover %%s in output, indicating unfilled placeholder:\n%s", out)
	}
	if strings.Contains(out, "%d") {
		t.Errorf("found leftover %%d in output, indicating unfilled placeholder:\n%s", out)
	}
}

// TestBuildTactical_RelationshipsInjected verifies a non-empty Relationships
// field surfaces as a【人际关系】segment in the prompt.
func TestBuildTactical_RelationshipsInjected(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:          "车间装配",
		Zone:          "main_workshop",
		TimeOfDay:     "08:00",
		Relationships: "- 与 H-02：熟悉度 5、好感 2（互动 3 次）",
		AgentID:       "H-01",
	})
	if !strings.Contains(out, "【人际关系】") {
		t.Error("missing 【人际关系】 segment when Relationships non-empty")
	}
	if !strings.Contains(out, "与 H-02：熟悉度 5") {
		t.Error("relationship content not injected into prompt")
	}
}

// TestBuildTactical_RelationshipsEmpty verifies an empty Relationships field
// produces no【人际关系】segment (single-NPC scenario should not pollute prompt).
func TestBuildTactical_RelationshipsEmpty(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:          "车间装配",
		Zone:          "main_workshop",
		TimeOfDay:     "08:00",
		Relationships: "",
		AgentID:       "H-01",
	})
	if strings.Contains(out, "【人际关系】") {
		t.Error("【人际关系】 segment present when Relationships empty — should be skipped")
	}
}

// TestNearbyAgentsLine verifies the 【附近NPC】 segment formatting:
// empty → "", non-empty → header + one bullet per agent with name/id/distance.
func TestNearbyAgentsLine(t *testing.T) {
	// Empty → "" (caller skips segment).
	if got := NearbyAgentsLine(nil); got != "" {
		t.Errorf("NearbyAgentsLine(nil) = %q, want empty", got)
	}
	if got := NearbyAgentsLine([]protocol.VisibleAgent{}); got != "" {
		t.Errorf("NearbyAgentsLine(empty) = %q, want empty", got)
	}

	// Non-empty → header + bullets.
	agents := []protocol.VisibleAgent{
		{ID: "H-02", Name: "老王", Distance: 4.2, CurrentAction: "idle"},
		{ID: "H-05", Name: "老张", Distance: 9.8, CurrentAction: ""},
	}
	got := NearbyAgentsLine(agents)
	if !strings.Contains(got, "【附近NPC】") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "老王（id=H-02）") {
		t.Errorf("missing H-02 entry: %q", got)
	}
	if !strings.Contains(got, "距离 4 米") {
		t.Errorf("missing distance for H-02: %q", got)
	}
	if !strings.Contains(got, "当前：idle") {
		t.Errorf("missing current_action for H-02: %q", got)
	}
	// Empty current_action → no "当前：" suffix.
	if strings.Contains(got, "老张（id=H-05）距离 10 米，当前：") {
		t.Errorf("H-05 should omit 当前 when action empty: %q", got)
	}
	if !strings.Contains(got, "老张（id=H-05）距离 10 米") {
		t.Errorf("missing H-05 entry: %q", got)
	}
}

// TestBuildTactical_VisibleAgentsInjected verifies that a non-empty
// VisibleAgents field produces a 【附近NPC】 segment in the prompt, and
// an empty one omits it. Uses "【附近NPC】\n" as the marker to distinguish
// the segment header from the inline mention in requirement #7.
func TestBuildTactical_VisibleAgentsInjected(t *testing.T) {
	withAgents := BuildTactical(TacticalInput{
		Goal:      "车间装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Slot:      "06:00-11:00",
		AgentID:   "H-01",
		VisibleAgents: []protocol.VisibleAgent{
			{ID: "H-02", Name: "老王", Distance: 5.0, CurrentAction: "sort_cargo"},
		},
	})
	if !strings.Contains(withAgents, "【附近NPC】\n") {
		t.Error("missing 【附近NPC】 segment when VisibleAgents non-empty")
	}
	if !strings.Contains(withAgents, "老王（id=H-02）") {
		t.Error("visible agent H-02 not injected into prompt")
	}

	withoutAgents := BuildTactical(TacticalInput{
		Goal:      "车间装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Slot:      "06:00-11:00",
		AgentID:   "H-01",
	})
	if strings.Contains(withoutAgents, "【附近NPC】\n") {
		t.Error("【附近NPC】 segment present when VisibleAgents empty — should be skipped")
	}
}

// TestTacticalExample_SocialChatGoal verifies the chat/social goal branch
// now emits a social_chat composite action (not move_to+speak) and picks
// the peer via pickChatPeer (falls back to first non-self agent when no
// KB relationships exist).
func TestTacticalExample_SocialChatGoal(t *testing.T) {
	kb := &worldkb.KB{
		Version: "1.0",
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈"},
			{ID: "H-02", DisplayName: "老王"},
			{ID: "H-05", DisplayName: "老张"},
		},
	}
	got := TacticalExample(kb, "去找同事聊天社交", "H-01")
	if !strings.Contains(got, `"action":"social_chat"`) {
		t.Errorf("chat goal should emit social_chat action, got:\n%s", got)
	}
	if !strings.Contains(got, `"target_agent_id":"H-02"`) {
		t.Errorf("pickChatPeer fallback should pick first non-self agent H-02, got:\n%s", got)
	}
	if strings.Contains(got, `"action":"move_to"`) {
		t.Errorf("chat goal should NOT emit move_to (social_chat is composite, auto-moves), got:\n%s", got)
	}
}

// TestTacticalExample_MeditateGoal verifies a meditate goal (even when the
// text mentions 睡眠舱, which would otherwise be routed to the sleep branch)
// emits InteractSmartObject(sleep_pod/meditate) instead of rest_at_residence.
func TestTacticalExample_MeditateGoal(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "residential_quarters", DisplayName: "休眠舱居住区"}},
		[]worldkb.Object{{ID: "SleepPod-1", DisplayName: "睡眠舱",
			SemanticGroup: "sleep_pod", ZoneID: "residential_quarters",
			AvailableInteractions: []string{"sleep", "meditate", "tidy_up"}}},
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "休眠舱居住区睡眠舱冥想放松", "H-01")
	if !strings.Contains(got, `"action":"InteractSmartObject"`) {
		t.Errorf("meditate goal should emit InteractSmartObject, got:\n%s", got)
	}
	if !strings.Contains(got, `"semantic_group":"sleep_pod"`) || !strings.Contains(got, `"interaction":"meditate"`) {
		t.Errorf("meditate goal should use sleep_pod/meditate, got:\n%s", got)
	}
	if strings.Contains(got, `"action":"rest_at_residence"`) || strings.Contains(got, `"interaction":"sleep"`) {
		t.Errorf("meditate goal must NOT be routed to rest_at_residence/sleep, got:\n%s", got)
	}
}

// TestTacticalExample_TidyUpGoal verifies a tidy-up goal emits
// InteractSmartObject(sleep_pod/tidy_up).
func TestTacticalExample_TidyUpGoal(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "residential_quarters", DisplayName: "休眠舱居住区"}},
		[]worldkb.Object{{ID: "SleepPod-1", DisplayName: "睡眠舱",
			SemanticGroup: "sleep_pod", ZoneID: "residential_quarters",
			AvailableInteractions: []string{"sleep", "meditate", "tidy_up"}}},
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "早起整理床铺和舱位", "H-01")
	if !strings.Contains(got, `"interaction":"tidy_up"`) {
		t.Errorf("tidy-up goal should emit tidy_up interaction, got:\n%s", got)
	}
	if strings.Contains(got, `"action":"rest_at_residence"`) {
		t.Errorf("tidy-up goal must NOT be routed to rest_at_residence, got:\n%s", got)
	}
}

// TestTacticalExample_ExerciseGoal verifies an exercise goal (晨练/拉伸)
// emits the exercise composite with a concrete exercise_type param.
func TestTacticalExample_ExerciseGoal(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "central_plaza", DisplayName: "中央广场"}},
		[]worldkb.Object{{ID: "Bench-1", DisplayName: "长椅",
			SemanticGroup: "bench", ZoneID: "central_plaza",
			AvailableInteractions: []string{"rest"}}},
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "中央广场晨练拉伸放松", "H-01")
	if !strings.Contains(got, `"action":"exercise"`) {
		t.Errorf("exercise goal should emit exercise composite, got:\n%s", got)
	}
	if !strings.Contains(got, `"exercise_type":"stretch"`) {
		t.Errorf("exercise example should show concrete exercise_type param, got:\n%s", got)
	}
}

// TestTacticalExample_InternetGoal verifies an internet goal emits the
// two-action form speak + surf_internet WITHOUT a preceding move_to.
// Rationale: the three-action form (speak+move_to+surf_internet) makes UE
// complete the composite almost instantly (0.1-1.7s, observed in logs),
// triggering re-decompose loops + idle standing; the two-action form lets
// the composite walk itself and run until the slot switch.
func TestTacticalExample_InternetGoal(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "archive_station", DisplayName: "档案馆"}},
		[]worldkb.Object{{ID: "Computer-1", DisplayName: "电脑",
			SemanticGroup: "computer", ZoneID: "archive_station",
			AvailableInteractions: []string{"surf_internet"}}},
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "档案馆电脑上网浏览", "H-01")
	if !strings.Contains(got, `"action":"surf_internet"`) {
		t.Errorf("internet goal should emit surf_internet composite, got:\n%s", got)
	}
	if !strings.Contains(got, `"semantic_group":"computer"`) {
		t.Errorf("internet example should use semantic_group=computer, got:\n%s", got)
	}
	if strings.Contains(got, `"action":"move_to"`) {
		t.Errorf("internet example must NOT include move_to before the composite, got:\n%s", got)
	}
}

// TestTacticalExample_SleepGoalStillRoutesToResidence verifies the plain
// sleep goal is unaffected by the new branches (regression guard).
func TestTacticalExample_SleepGoalStillRoutesToResidence(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "residential_quarters", DisplayName: "休眠舱居住区"}},
		[]worldkb.Object{{ID: "SleepPod-1", DisplayName: "睡眠舱",
			SemanticGroup: "sleep_pod", ZoneID: "residential_quarters",
			AvailableInteractions: []string{"sleep", "meditate", "tidy_up"}}},
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "回休眠舱睡觉", "H-01")
	if !strings.Contains(got, `"action":"rest_at_residence"`) || !strings.Contains(got, `"interaction":"sleep"`) {
		t.Errorf("plain sleep goal should still route to rest_at_residence/sleep, got:\n%s", got)
	}
}

// TestPickChatPeer_PrefersRelationship verifies pickChatPeer prefers the
// KB-declared relationship peer with the highest familiarity over the
// declaration-order fallback.
func TestPickChatPeer_PrefersRelationship(t *testing.T) {
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈"},
			{ID: "H-02", DisplayName: "老王"},
			{ID: "H-03", DisplayName: "老李"},
		},
		Relationships: []worldkb.Relationship{
			{From: "H-01", To: "H-02", Familiarity: 3, Affection: 1, Type: "colleague"},
			{From: "H-01", To: "H-03", Familiarity: 7, Affection: 2, Type: "colleague"},
		},
	}
	// H-03 has higher familiarity → preferred over H-02 (declaration order).
	peer := pickChatPeer(kb, "H-01")
	if peer != "H-03" {
		t.Errorf("pickChatPeer = %q, want H-03 (highest familiarity)", peer)
	}
}

func TestPickChatPeer_FallbackNoRelationships(t *testing.T) {
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈"},
			{ID: "H-04", DisplayName: "老刘"},
		},
	}
	// No relationships → fallback to first non-self agent.
	peer := pickChatPeer(kb, "H-01")
	if peer != "H-04" {
		t.Errorf("pickChatPeer fallback = %q, want H-04", peer)
	}
}

func TestPickChatPeer_SingleAgent(t *testing.T) {
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	}
	if peer := pickChatPeer(kb, "H-01"); peer != "" {
		t.Errorf("pickChatPeer single agent = %q, want empty", peer)
	}
}

// TestBuildTacticalSystemPrompt_Structure verifies the tactical system prompt
// shares the strategic layer's KB modules (世界背景/人物背景/世界详细信息)
// plus the full tool list — and does NOT carry the decomposition rules
// (they live in the user message).
func TestBuildTacticalSystemPrompt_Structure(t *testing.T) {
	got := BuildTacticalSystemPrompt(strategicDetailKB(), nil, "H-01", nil)
	overIdx := strings.Index(got, "【世界背景】")
	roleIdx := strings.Index(got, "【人物背景】")
	detailIdx := strings.Index(got, "【世界详细信息】")
	if overIdx < 0 || roleIdx < 0 || detailIdx < 0 {
		t.Fatalf("missing shared modules (bg=%d role=%d detail=%d):\n%s", overIdx, roleIdx, detailIdx, got)
	}
	if !(overIdx < roleIdx && roleIdx < detailIdx) {
		t.Errorf("module order wrong: bg=%d role=%d detail=%d", overIdx, roleIdx, detailIdx)
	}
	// 战术层特有：完整工具清单（带 params 与 [复合]/[原子] 标签）。
	if !strings.Contains(got, "可用工具（仅限以下") {
		t.Errorf("system prompt missing the tool list:\n%s", got)
	}
	if !strings.Contains(got, "[复合]") || !strings.Contains(got, "[原子]") {
		t.Errorf("tool list should carry kind labels:\n%s", got)
	}
	// 分解规则已迁至 user prompt。
	if strings.Contains(got, "队列首个动作必须是 speak") {
		t.Errorf("system prompt should not contain the rules (moved to user prompt):\n%s", got)
	}
}

// TestBuildTactical_FourParts verifies the user message's four-part layout:
// 全天任务与当前时段任务 / NPC与环境实时状态 / 分解规则 / 任务.
func TestBuildTactical_FourParts(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal:      "主生产车间工作台装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Slot:      "07:00-12:00",
		DailyPlan: "07:00-12:00: 主生产车间工作台装配\n12:00-14:00: 中央广场长椅休息",
		AgentID:   "H-01",
	})
	p1 := strings.Index(out, "一、全天任务与当前时段任务")
	p2 := strings.Index(out, "二、NPC与环境实时状态")
	p3 := strings.Index(out, "三、分解规则")
	p4 := strings.Index(out, "四、任务")
	if p1 < 0 || p2 < 0 || p3 < 0 || p4 < 0 {
		t.Fatalf("missing four parts (t=%d s=%d r=%d a=%d):\n%s", p1, p2, p3, p4, out)
	}
	if !(p1 < p2 && p2 < p3 && p3 < p4) {
		t.Errorf("part order wrong: %d %d %d %d", p1, p2, p3, p4)
	}
	// 第一部分：全天日程 + 当前时段目标。
	if !strings.Contains(out, "【全天日程】") || !strings.Contains(out, "中央广场长椅休息") {
		t.Errorf("part 1 missing full-day schedule:\n%s", out)
	}
	if !strings.Contains(out, "【当前时段目标】主生产车间工作台装配") {
		t.Errorf("part 1 missing current goal:\n%s", out)
	}
	// 第二部分：实时状态。
	if !strings.Contains(out, "你目前在：main_workshop，游戏时间 08:00。") {
		t.Errorf("part 2 missing realtime state line:\n%s", out)
	}
	// 第三部分：分解规则。
	if !strings.Contains(out, "队列首个动作必须是 speak") {
		t.Errorf("part 3 missing tactical rules:\n%s", out)
	}
	// 第四部分：任务 + 示例。
	if !strings.Contains(out, "请把【当前时段目标】分解为一个或多个 action") {
		t.Errorf("part 4 missing the ask:\n%s", out)
	}
	// KB/工具清单不重复出现在 user prompt（规则文本引用模块名不算）。
	if strings.Contains(out, "可用工具（仅限以下") {
		t.Errorf("user prompt should not carry the tool list (system prompt's job):\n%s", out)
	}
}

// TestBuildTactical_EmptyDailyPlanSkipped verifies an empty DailyPlan skips
// the 【全天日程】 segment.
func TestBuildTactical_EmptyDailyPlanSkipped(t *testing.T) {
	out := BuildTactical(TacticalInput{
		Goal: "车间装配", Zone: "main_workshop", TimeOfDay: "08:00",
		Slot: "07:00-12:00", AgentID: "H-01",
	})
	if strings.Contains(out, "【全天日程】") {
		t.Errorf("empty DailyPlan should skip the segment:\n%s", out)
	}
}
