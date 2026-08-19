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
		Goal:      "车间装配",
		Zone:      "main_workshop",
		TimeOfDay: "08:00",
		Relationships: "",
		AgentID:   "H-01",
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

// TestTacticalExample_ExerciseGoal verifies the exercise/stretch goal branch
// emits move_to BEFORE exercise (exercise is in-place, UE does not auto-move),
// and prefers a plaza-like zone.
func TestTacticalExample_ExerciseGoal(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{
			{ID: "main_workshop", DisplayName: "主车间"},
			{ID: "central_plaza", DisplayName: "中央广场"},
		},
		nil,
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "前往中央广场拉伸放松一下", "H-01")
	if !strings.Contains(got, `"action":"move_to"`) {
		t.Errorf("exercise goal must emit move_to first, got:\n%s", got)
	}
	if !strings.Contains(got, `"action":"exercise"`) {
		t.Errorf("exercise goal should emit exercise action, got:\n%s", got)
	}
	if strings.Index(got, `"action":"move_to"`) > strings.Index(got, `"action":"exercise"`) {
		t.Errorf("move_to must precede exercise (in-place action), got:\n%s", got)
	}
	if !strings.Contains(got, `"exercise_type":"stretch"`) {
		t.Errorf("exercise action must fill required exercise_type param, got:\n%s", got)
	}
}

// TestTacticalExample_ExerciseZoneFallback verifies exerciseZone falls back
// to the first zone when no plaza-like zone exists.
func TestTacticalExample_ExerciseZoneFallback(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{
			{ID: "main_workshop", DisplayName: "主车间"},
			{ID: "archive_room", DisplayName: "档案馆"},
		},
		nil,
		[]worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	)
	got := TacticalExample(kb, "去锻炼身体", "H-01")
	if !strings.Contains(got, `"target_id":"main_workshop"`) {
		t.Errorf("exercise example should fall back to first zone, got:\n%s", got)
	}
}

// TestTacticalSystemPrompt_ExerciseInPlaceRule verifies the system prompt
// tells the LLM exercise is in-place and requires a preceding move_to when
// the goal mentions going somewhere to exercise.
func TestTacticalSystemPrompt_ExerciseInPlaceRule(t *testing.T) {
	if !strings.Contains(TacticalSystemPrompt, "exercise（原地锻炼/拉伸）是原地动作") {
		t.Error("missing exercise in-place rule in TacticalSystemPrompt")
	}
	if !strings.Contains(TacticalSystemPrompt, "必须先 move_to 到目标 zone，再输出 exercise") {
		t.Error("missing move_to-before-exercise requirement in TacticalSystemPrompt")
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
