package prompt

import (
	"strings"
	"testing"
)

// TestBuildReactive_QueueStateSegment verifies the 【排队状态】 segment
// appears when QueuedFor is non-empty and collapses to a blank line when
// the agent is not queued (约定21).
func TestBuildReactive_QueueStateSegment(t *testing.T) {
	base := ReactiveInput{
		AgentID:   "H-01",
		AgentName: "老陈",
		TimeOfDay: "09:30",
		Zone:      "main_workshop",
		Trigger:   TriggerPeriodic,
	}

	// Not queued — segment should be absent.
	promptNoQueue := BuildReactive(base)
	if strings.Contains(promptNoQueue, "【排队状态】") {
		t.Errorf("prompt should omit 排队状态 segment when QueuedFor empty:\n%s", promptNoQueue)
	}

	// Queued — segment should appear with the description.
	withQueue := base
	withQueue.QueuedFor = "正在排队等待 workbench（位置 2，预计等待 30 秒）"
	promptQueue := BuildReactive(withQueue)
	if !strings.Contains(promptQueue, "【排队状态】") {
		t.Errorf("prompt should include 排队状态 segment when QueuedFor set:\n%s", promptQueue)
	}
	if !strings.Contains(promptQueue, "正在排队等待 workbench") {
		t.Errorf("prompt missing QueuedFor content:\n%s", promptQueue)
	}
}

// TestBuildReactive_QueueStateTemplateStability ensures the existing
// prompt structure (other segments intact) is not broken by the
// additional %s slot for the queue segment.
func TestBuildReactive_QueueStateTemplateStability(t *testing.T) {
	in := ReactiveInput{
		AgentID:       "H-01",
		AgentName:     "老陈",
		AgentRole:     "车间主管",
		TimeOfDay:     "09:30",
		Zone:          "main_workshop",
		CurrentAction: "WorkShift(smart_object=workbench_01)",
		ActionSrc:     "tactical",
		CurrentSlot:   "07:00-11:00",
		DailyPlan:     "07:00-11:00 车间装配",
		Trigger:       TriggerActionDone,
		TriggerDetail: "result=failed reason=queue_timeout",
		QueuedFor:     "正在排队等待 workbench（位置 1，预计等待 10 秒）",
	}
	prompt := BuildReactive(in)

	mustContain := []string{
		"你是 NPC 老陈",
		"【你的角色】\n车间主管",
		"游戏时间：09:30",
		"位置：main_workshop",
		"【在途动作】\nWorkShift(smart_object=workbench_01)",
		"来源：tactical",
		"【排队状态】\n正在排队等待 workbench",
		"当前时段：07:00-11:00",
		"result=failed reason=queue_timeout",
	}
	for _, s := range mustContain {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt missing %q:\n%s", s, prompt)
		}
	}
}
