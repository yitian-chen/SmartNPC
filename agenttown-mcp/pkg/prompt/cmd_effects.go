// Package prompt — cmd 属性影响的自然语言转换。
//
// UE 推送 world_kb 时，每个 object 的 available_interactions 以对象数组
// 形态声明各互动的属性影响速率（energy/fatigue/joint_wear/money 的
// *_delta_per_hour 与 money_one_shot）。合并器把这些原始 map 保留在
// Object.Extra["available_interactions"]（持久化 yaml 只保留动词名）。
// 本文件在运行时从合并后的 KB 提取速率，按 (semantic_group, interaction)
// 去重，分档转成自然语言（如"疲劳度明显提升、余额快速增加"），经
// SetCmdEffects 注入战略层与战术层 prompt 的【动作对属性的影响】段。
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// InteractionEffect 是一个 (semantic_group, interaction) 的属性影响声明。
type InteractionEffect struct {
	SemanticGroup string
	DisplayName   string // 物体显示名（如"睡眠舱"），缺省用 semantic_group
	Interaction   string
	// 每游戏小时变化率（正=上升，负=下降）
	Energy    float64
	Fatigue   float64
	JointWear float64
	Money     float64
	// MoneyOneShot 是使用时的一次性余额变动（充电 -30、维修 -50）。
	MoneyOneShot float64
}

// magnitude 分档（每游戏小时 |速率|）：<1 忽略；1-4 少量；4-10 中等；
// 10-20 明显；≥20 快速。阈值与 UE 侧数值量纲对齐（工作疲劳 +8~20/h、
// 充电能量 +80/h、睡眠疲劳 -25/h 等）。
var effectMagnitudes = []struct {
	lo   float64
	word string
}{
	{20, "快速"},
	{10, "明显"},
	{4, "中等"},
	{1, "少量"},
}

// effectAttrWord 属性 → (显示名, 上升动词, 下降动词)。
var effectAttrWord = map[string][3]string{
	"energy":    {"能量", "恢复", "下降"},
	"fatigue":   {"疲劳度", "提升", "缓解"},
	"joint_wear": {"关节磨损", "累积", "修复"},
	"money":     {"余额", "增加", "消耗"},
}

// InteractionEffectsFromKB 从合并后的 KB 提取所有带速率声明的互动。
// 数据源是 Object.Extra["available_interactions"]（对象数组形态，由
// world_kb 推送时的合并器写入）；字符串形态（无速率）的对象跳过。
// 同一 (semantic_group, interaction) 的多个实例只保留首个。
func InteractionEffectsFromKB(kb *worldkb.KB) []InteractionEffect {
	if kb == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []InteractionEffect
	for _, obj := range kb.Objects {
		raw, ok := obj.Extra["available_interactions"].([]any)
		if !ok {
			continue
		}
		sg := obj.SemanticGroup
		if sg == "" {
			sg = obj.ID
		}
		display := obj.DisplayName
		if display == "" {
			display = sg
		}
		for _, e := range raw {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			name, _ := m["name"].(string)
			if name == "" {
				continue
			}
			key := sg + "/" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, InteractionEffect{
				SemanticGroup: sg,
				DisplayName:   display,
				Interaction:   name,
				Energy:        effectFloat(m, "energy_delta_per_hour"),
				Fatigue:       effectFloat(m, "fatigue_delta_per_hour"),
				JointWear:     effectFloat(m, "joint_wear_delta_per_hour"),
				Money:         effectFloat(m, "money_delta_per_hour"),
				MoneyOneShot:  effectFloat(m, "money_one_shot"),
			})
		}
	}
	return out
}

// effectFloat 从原始 map 取数值字段，缺省/类型不符返回 0。
func effectFloat(m map[string]any, key string) float64 {
	v, ok := m[key].(float64)
	if !ok {
		return 0
	}
	return v
}

// BuildCmdEffectsText 把互动影响声明转成自然语言多行文本（每行一条，
// 按语义组+动词排序）。空列表返回空串（调用方跳过注入）。
func BuildCmdEffectsText(effects []InteractionEffect) string {
	if len(effects) == 0 {
		return ""
	}
	sorted := make([]InteractionEffect, len(effects))
	copy(sorted, effects)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SemanticGroup != sorted[j].SemanticGroup {
			return sorted[i].SemanticGroup < sorted[j].SemanticGroup
		}
		return sorted[i].Interaction < sorted[j].Interaction
	})
	var sb strings.Builder
	sb.WriteString("各活动对属性的影响（每游戏小时变化率，来自 world KB 声明）：\n")
	for _, e := range sorted {
		sb.WriteString(fmt.Sprintf("- %s（%s/%s）：%s\n", e.DisplayName, e.SemanticGroup, e.Interaction, describeEffect(e)))
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// describeEffect 把单个互动的速率转成自然语言短语（顿号分隔）。
// 无显著变化（所有 |速率| <1 且无一次性变动）时返回"无属性影响"。
func describeEffect(e InteractionEffect) string {
	var parts []string
	for _, attr := range []string{"energy", "fatigue", "joint_wear", "money"} {
		var r float64
		switch attr {
		case "energy":
			r = e.Energy
		case "fatigue":
			r = e.Fatigue
		case "joint_wear":
			r = e.JointWear
		case "money":
			r = e.Money
		}
		if r > -1 && r < 1 {
			continue
		}
		word := ""
		for _, mg := range effectMagnitudes {
			if r >= mg.lo || r <= -mg.lo {
				word = mg.word
				break
			}
		}
		if word == "" {
			continue // 防御：<1 已被上面过滤
		}
		name, up, down := effectAttrWord[attr][0], effectAttrWord[attr][1], effectAttrWord[attr][2]
		if r > 0 {
			parts = append(parts, name+word+up)
		} else {
			parts = append(parts, name+word+down)
		}
	}
	if e.MoneyOneShot > 0 {
		parts = append(parts, fmt.Sprintf("一次性增加余额 %g 点", e.MoneyOneShot))
	} else if e.MoneyOneShot < 0 {
		parts = append(parts, fmt.Sprintf("一次性消耗余额 %g 点", -e.MoneyOneShot))
	}
	if len(parts) == 0 {
		return "无属性影响"
	}
	return strings.Join(parts, "、")
}
