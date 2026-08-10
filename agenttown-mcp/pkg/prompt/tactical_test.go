package prompt

import (
	"strings"
	"testing"
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
