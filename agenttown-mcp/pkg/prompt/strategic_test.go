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

func TestBuildStrategic_InjectsOtherNPCsSegment(t *testing.T) {
	kb := strategicRosterKB()
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	// 用 "【其他NPC】\n" 精确匹配段头，避免与【社交】段文案里的
	// "与【其他NPC】中的某一位" 子串混淆。
	npcHeader := "【其他NPC】\n"
	npcIdx := strings.Index(got, npcHeader)
	if npcIdx < 0 {
		t.Fatalf("missing 【其他NPC】 segment header in:\n%s", got)
	}
	// 提取【其他NPC】段内容：从段头到下一个段头【可用能力】。
	capIdx := strings.Index(got, "【可用能力】")
	if capIdx < 0 {
		t.Fatalf("missing 【可用能力】 segment for boundary check")
	}
	npcSection := got[npcIdx:capIdx]
	if strings.Contains(npcSection, "H-01") {
		t.Errorf("self id H-01 should not appear in 【其他NPC】 section:\n%s", npcSection)
	}
	if !strings.Contains(npcSection, "老王（id=H-02）") {
		t.Errorf("peer 老王 should appear in 【其他NPC】:\n%s", npcSection)
	}
	if !(npcIdx < capIdx) {
		t.Errorf("【其他NPC】 should precede 【可用能力】 (npc=%d cap=%d)", npcIdx, capIdx)
	}
}

func TestBuildStrategic_OmitsOtherNPCsWhenNoPeers(t *testing.T) {
	kb := &worldkb.KB{
		Version: "1.0",
		Agents:  []worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}},
	}
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if strings.Contains(got, "【其他NPC】\n") {
		t.Errorf("single-agent KB should not produce 【其他NPC】 segment in:\n%s", got)
	}
}

func TestBuildStrategic_ExampleContainsSocialChatSlot(t *testing.T) {
	// StrategicPromptTemplate is a constant; verify the example demonstrates
	// a social_chat slot so the LLM sees chatting as a legitimate plan item.
	if !strings.Contains(StrategicPromptTemplate, "social_chat") {
		t.Errorf("strategic prompt example should mention social_chat to model it as a valid slot")
	}
}

func TestBuildStrategic_SocialSegmentMentionsFrequency(t *testing.T) {
	kb := strategicRosterKB()
	got := BuildStrategic(kb, nil, "H-01", nil, nil, "")
	if !strings.Contains(got, "每天安排 1 个社交时段") {
		t.Errorf("【社交】 segment should suggest daily frequency in:\n%s", got)
	}
}
