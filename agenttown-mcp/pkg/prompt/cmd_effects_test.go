package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// effectKB builds a KB whose objects carry rate-declared interactions in
// Extra["available_interactions"]，模拟 world_kb 推送合并后的内存形态。
func effectKB() *worldkb.KB {
	mk := func(sg, display string, itxs ...map[string]any) worldkb.Object {
		arr := make([]any, len(itxs))
		for i, m := range itxs {
			arr[i] = m
		}
		return worldkb.Object{
			ID: sg + "-1", DisplayName: display, SemanticGroup: sg,
			Extra: map[string]any{"available_interactions": arr},
		}
	}
	return &worldkb.KB{Objects: []worldkb.Object{
		mk("workbench", "工作台", map[string]any{
			"name": "assemble",
			"description":              "在工作台上进行零件装配，产出成品",
			"energy_delta_per_hour":    -4.0, "fatigue_delta_per_hour": 12.0,
			"joint_wear_delta_per_hour": 1.4, "money_delta_per_hour": 30.0,
			"min_energy_to_use": 10.0, "max_fatigue_to_use": 80.0, "max_joint_wear_to_use": 80.0,
		}),
		mk("charger", "充电桩", map[string]any{
			"name": "charge",
			"description":           "连接到充电桩补充能量。能量低时前往，可恢复能量值",
			"energy_delta_per_hour": 80.0, "fatigue_delta_per_hour": -5.0,
			"money_delta_per_hour": -20.0, "money_one_shot": -30.0,
			"min_money_to_use": 40.0,
		}),
		mk("sleep_pod", "睡眠舱",
			map[string]any{"name": "sleep", "description": "进入休眠舱休息，恢复精力和状态", "fatigue_delta_per_hour": -25.0},
			map[string]any{"name": "tidy_up", "description": "整理内务：整理自己的私人物品和床铺，保持整洁"},
		),
		mk("bench", "长椅", map[string]any{
			"name": "rest", "fatigue_delta_per_hour": -10.0,
		}),
		// 同组第二个实例：应被去重。
		mk("bench", "长椅", map[string]any{
			"name": "rest", "fatigue_delta_per_hour": -10.0,
		}),
		// 字符串形态（无速率声明）：跳过。
		{ID: "legacy-1", DisplayName: "旧物体", SemanticGroup: "legacy",
			AvailableInteractions: []string{"poke"}},
	}}
}

func TestInteractionEffectsFromKB(t *testing.T) {
	effects := InteractionEffectsFromKB(effectKB())
	if len(effects) != 5 { // workbench/assemble, charger/charge, sleep/sleep, sleep/tidy_up, bench/rest
		t.Fatalf("got %d effects, want 5 (dup bench + string-form legacy skipped): %+v", len(effects), effects)
	}
	byKey := map[string]InteractionEffect{}
	for _, e := range effects {
		byKey[e.SemanticGroup+"/"+e.Interaction] = e
	}
	wb := byKey["workbench/assemble"]
	if wb.Energy != -4 || wb.Fatigue != 12 || wb.JointWear != 1.4 || wb.Money != 30 {
		t.Errorf("workbench/assemble rates not extracted: %+v", wb)
	}
	if wb.DisplayName != "工作台" {
		t.Errorf("DisplayName = %q, want 工作台", wb.DisplayName)
	}
	ch := byKey["charger/charge"]
	if ch.MoneyOneShot != -30 {
		t.Errorf("charger/charge one_shot = %v, want -30", ch.MoneyOneShot)
	}
	if _, ok := byKey["legacy/poke"]; ok {
		t.Error("string-form interaction should be skipped")
	}
	if InteractionEffectsFromKB(nil) != nil {
		t.Error("nil KB should return nil")
	}
}

func TestBuildCmdEffectsText(t *testing.T) {
	text := BuildCmdEffectsText(InteractionEffectsFromKB(effectKB()))
	for _, want := range []string{
		"各活动对属性的影响（每游戏小时变化率，来自 world KB 声明）：",
		// 描述在前、属性效果居中、使用门槛收尾。
		"工作台（workbench/assemble）：在工作台上进行零件装配，产出成品。能量中等下降、疲劳度明显提升、关节磨损少量累积、余额快速增加。使用门槛：能量≥10、疲劳≤80、磨损≤80",
		"充电桩（charger/charge）：连接到充电桩补充能量。能量低时前往，可恢复能量值。能量快速恢复、疲劳度中等缓解、余额快速消耗、一次性消耗余额 30 点。使用门槛：余额≥40",
		"睡眠舱（sleep_pod/sleep）：进入休眠舱休息，恢复精力和状态。疲劳度快速缓解",
		"睡眠舱（sleep_pod/tidy_up）：整理内务：整理自己的私人物品和床铺，保持整洁。无属性影响",
		// 无描述、无门槛的条目保持原样。
		"长椅（bench/rest）：疲劳度明显缓解",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("effects text missing %q:\n%s", want, text)
		}
	}
	// 排序：按 semantic_group + interaction 字典序。
	iWork := strings.Index(text, "workbench/assemble")
	iBench := strings.Index(text, "bench/rest")
	if iBench > iWork {
		t.Errorf("effects should sort by semantic_group (bench before workbench):\n%s", text)
	}
	if BuildCmdEffectsText(nil) != "" {
		t.Error("empty effects should produce empty text")
	}
}

// TestBuildTactical_CmdEffectsInjected verifies the tactical prompt carries
// the same effects segment when installed (and skips it when empty).
func TestBuildTactical_CmdEffectsInjected(t *testing.T) {
	SetCmdEffects("- 长椅（bench/rest）：疲劳度明显缓解")
	defer SetCmdEffects("")
	out := BuildTactical(TacticalInput{
		Goal: "恢复", Zone: "central_plaza", TimeOfDay: "12:00",
		Slot: "12:00-14:00", AgentID: "H-01",
	})
	if !strings.Contains(out, "【动作对属性的影响】") {
		t.Errorf("tactical prompt missing effects segment:\n%s", out)
	}
	if !strings.Contains(out, "疲劳度明显缓解") {
		t.Errorf("tactical prompt missing effects content:\n%s", out)
	}
	// 空串时整段消失。
	SetCmdEffects("")
	out = BuildTactical(TacticalInput{
		Goal: "恢复", Zone: "central_plaza", TimeOfDay: "12:00",
		Slot: "12:00-14:00", AgentID: "H-01",
	})
	if strings.Contains(out, "【动作对属性的影响】") {
		t.Errorf("empty cmd effects should not produce segment:\n%s", out)
	}
}
