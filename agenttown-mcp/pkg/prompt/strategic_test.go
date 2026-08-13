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
