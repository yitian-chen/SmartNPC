package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
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

// TestShouldTriggerReactive_PerNPCThresholds verifies the physical alert
// crossing detection uses the per-NPC band thresholds: 老陈（疲劳 t3=90）
// 在疲劳 85 时不触发告警，默认 NPC（t3=80）触发。
func TestShouldTriggerReactive_PerNPCThresholds(t *testing.T) {
	prev := &protocol.PhysicalState{Energy: 90, Fatigue: 75}
	cur := &protocol.PhysicalState{Energy: 90, Fatigue: 85}

	laochen := DefaultBandThresholds()
	laochen.Fatigue = [3]float64{40, 70, 90}
	trig, _ := ShouldTriggerReactive("z", "z", nil, nil, prev, cur, laochen)
	if trig != "" {
		t.Errorf("老陈 fatigue 75→85 should NOT trigger (t3=90), got %q", trig)
	}

	trig, detail := ShouldTriggerReactive("z", "z", nil, nil, prev, cur, DefaultBandThresholds())
	if trig != TriggerPhysicalAlert {
		t.Errorf("默认 NPC fatigue 75→85 should trigger (t3=80), got %q", trig)
	}
	if !strings.Contains(detail, "80") {
		t.Errorf("detail should reference the effective threshold 80, got %q", detail)
	}
}

// TestUpgradeIfPhysicalAlert_PerNPCThresholds verifies the code-level
// forced replan respects per-NPC thresholds: 老陈 fatigue 85 → no upgrade;
// 默认 NPC fatigue 85 → upgrade to replan.
func TestUpgradeIfPhysicalAlert_PerNPCThresholds(t *testing.T) {
	base := ReactiveInput{
		AgentID:           "H-01",
		Fatigue:           85,
		Energy:            90,
		PhysicalAvailable: true,
	}
	dec := ReactiveDecision{Reaction: ReactionContinue, Reason: "ok"}

	laochen := base
	laochen.Bands = DefaultBandThresholds()
	laochen.Bands.Fatigue = [3]float64{40, 70, 90}
	if got := UpgradeIfPhysicalAlert(laochen, dec); got.Reaction != ReactionContinue {
		t.Errorf("老陈 fatigue 85 should NOT upgrade (t3=90), got %q", got.Reaction)
	}

	other := base
	other.Bands = DefaultBandThresholds()
	if got := UpgradeIfPhysicalAlert(other, dec); got.Reaction != ReactionReplan {
		t.Errorf("默认 NPC fatigue 85 should upgrade to replan (t3=80), got %q", got.Reaction)
	}
}

// TestUpgradeIfPhysicalAlert_ZeroBandsFallBack verifies a zero Bands value
// (caller did not resolve per-NPC thresholds) falls back to defaults
// instead of alerting on every state.
func TestUpgradeIfPhysicalAlert_ZeroBandsFallBack(t *testing.T) {
	in := ReactiveInput{
		AgentID:           "H-01",
		Fatigue:           50, // below default 80 — must not alert
		Energy:            90,
		PhysicalAvailable: true,
		// Bands zero value
	}
	dec := ReactiveDecision{Reaction: ReactionContinue, Reason: "ok"}
	if got := UpgradeIfPhysicalAlert(in, dec); got.Reaction != ReactionContinue {
		t.Errorf("zero Bands should fall back to defaults (no alert at fatigue 50), got %q", got.Reaction)
	}
}
