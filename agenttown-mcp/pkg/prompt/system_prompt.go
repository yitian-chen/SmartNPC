// Package prompt — the shared system prompt for the unified per-NPC agentic
// loop. Injected verbatim and identically as the system role of every LLM
// call (strategic / tactical / dialogue layers); all layer-specific content
// lives in each layer's user message.
package prompt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// ProductionWorkflowText summarizes the town's production workflow (from
// docs/AgentTown_Workflow.md) as a short natural-language paragraph. Injected
// into the shared system prompt so every layer shares the same mental model
// of the production loop and job trade-offs.
const ProductionWorkflowText = "小镇生产是一条概念流水线：物流转运站加工原料、分拣货物，主生产车间装配半成品、调试设备、质检成品，废料回收与再制造场拆解报废设备回收零件（当前阶段无物料/库存依赖，各工序彼此独立）。六类工种强度与收入成正比：质检轻松低薪；装配、加工、分拣强度与收入中等；调试、拆解高薪但更耗电量、更积磨损。机器人靠电量、疲劳、关节磨损、余额四项状态维持“打工循环”：工作挣余额，同时耗电量、涨疲劳、积磨损，又需花钱充电、休息、维修才能延续工作。规划时应按自身状态取舍：累了或电量低选轻松工种，缺钱时选高强度工种。"

// BuildSharedSystemPrompt constructs THE system prompt, injected verbatim and
// identically into the strategic, tactical, and dialogue layers:
//  1. 【世界背景】 — world overview (WorldOverview).
//  2. 【人物背景】 — the current agent's profile (AgentRole).
//  3. 【世界详细信息】 — per-zone descriptions + facility groups with inline
//     per-interaction effects (worldDetailCore).
//  4. 【生产工作流】 — the production workflow overview.
//
// The output is byte-identical across the three layers and static for the
// whole simulation (kb/profiles load once at startup, never hot-reload), so
// it stays cache-friendly. ALL layer-specific content — planning rules,
// decomposition rules, the strategic 【其他NPC】 roster, the dialogue
// mechanism and JSON output formats — lives in each layer's user message.
//
// kb == nil → modules degrade to empty; profiles == nil → persona falls back
// to the hardcoded fallback fields.
func BuildSharedSystemPrompt(kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	var sb strings.Builder

	if m1 := WorldOverview(kb); m1 != "" {
		sb.WriteString("【世界背景】\n")
		sb.WriteString(m1)
	}
	if role := AgentRole(kb, profiles, agentID); role != "" {
		sb.WriteString("\n【人物背景】\n")
		sb.WriteString(role)
	}
	if m3 := worldDetailCore(kb); m3 != "" {
		sb.WriteString("\n【世界详细信息】\n")
		sb.WriteString(m3)
	}
	sb.WriteString("\n【生产工作流】\n")
	sb.WriteString(ProductionWorkflowText)
	return sb.String()
}

// WorldOverview renders the shared module 1: the world's basic situation —
// narrative setting/theme, zone roster, smart-object group roster, and NPC
// roster (compact inventories only; details live in module 3).
func WorldOverview(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	var lines []string
	if kb.Narrative.Setting != "" {
		lines = append(lines, "设定："+kb.Narrative.Setting)
	}
	if kb.Narrative.Theme != "" {
		lines = append(lines, "主题："+kb.Narrative.Theme)
	}
	if zs := kb.ListZones(); len(zs) > 0 {
		parts := make([]string, 0, len(zs))
		for _, z := range zs {
			if z.DisplayName != "" && z.DisplayName != z.ID {
				parts = append(parts, fmt.Sprintf("%s（%s）", z.DisplayName, z.ID))
			} else {
				parts = append(parts, z.ID)
			}
		}
		lines = append(lines, fmt.Sprintf("区域（%d 个）：%s。", len(zs), strings.Join(parts, "、")))
	}
	if os := kb.ListObjects(); len(os) > 0 {
		parts := make([]string, 0)
		for _, g := range groupObjectsBySemantic(os) {
			label := g.SemanticGroup
			if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
				label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
			}
			if g.InstanceCount > 1 {
				label += fmt.Sprintf("，%d 个实例", g.InstanceCount)
			}
			parts = append(parts, label)
		}
		lines = append(lines, fmt.Sprintf("可交互设施类别（%d 类）：%s。", len(parts), strings.Join(parts, "、")))
	}
	if ags := kb.Agents; len(ags) > 0 {
		parts := make([]string, 0, len(ags))
		for _, a := range ags {
			if a.DisplayName != "" && a.DisplayName != a.ID {
				parts = append(parts, fmt.Sprintf("%s（%s）", a.DisplayName, a.ID))
			} else {
				parts = append(parts, a.ID)
			}
		}
		lines = append(lines, fmt.Sprintf("居民（%d 位）：%s。", len(ags), strings.Join(parts, "、")))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// worldDetailCore renders the KB-derived world detail shared by the strategic
// and tactical system prompts: per-zone descriptions (each inlined with its
// interactive facilities) + smart objects grouped by semantic_group with
// per-interaction description, per-hour attribute effects and usage gates
// (from the KB's declared rates).
func worldDetailCore(kb *worldkb.KB) string {
	var sb strings.Builder
	wroteZone := false
	if kb != nil {
		if zs := kb.ListZones(); len(zs) > 0 {
			// 先聚合 zone → semantic_group 真实分布 + semantic_group → 显示名，
			// 供区域详情行内联附上"可交互设施"，避免单独再列一段、重复 zone 标签。
			os := kb.ListObjects()
			zoneGroups := make(map[string]map[string]bool, len(zs))
			for _, o := range os {
				if o.ZoneID == "" || o.SemanticGroup == "" {
					continue
				}
				if zoneGroups[o.ZoneID] == nil {
					zoneGroups[o.ZoneID] = make(map[string]bool)
				}
				zoneGroups[o.ZoneID][o.SemanticGroup] = true
			}
			display := make(map[string]string, len(os))
			for _, g := range groupObjectsBySemantic(os) {
				label := g.SemanticGroup
				if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
					label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
				}
				display[g.SemanticGroup] = label
			}
			sb.WriteString("各区域详情：\n")
			wroteZone = true
			for _, z := range zs {
				label := z.ID
				if z.DisplayName != "" && z.DisplayName != z.ID {
					label = fmt.Sprintf("%s（%s）", z.DisplayName, z.ID)
				}
				line := "- " + label
				if d := strings.TrimSpace(z.Description); d != "" {
					line += "：" + d
				}
				// 该区域的可交互设施（按实例真实分布聚合）附在详情行后。
				if gs := zoneGroups[z.ID]; len(gs) > 0 {
					keys := make([]string, 0, len(gs))
					for k := range gs {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					names := make([]string, 0, len(keys))
					for _, k := range keys {
						if n, ok := display[k]; ok {
							names = append(names, n)
						} else {
							names = append(names, k)
						}
					}
					line += "；可交互设施：" + strings.Join(names, "、")
				}
				sb.WriteString(line + "\n")
			}
		}
	}
	// 设施详情：按 semantic_group 分组，交互行内联 KB 声明的描述与属性变动。
	if kb != nil {
		if os := kb.ListObjects(); len(os) > 0 {
			if wroteZone {
				sb.WriteString("\n")
			}
			sb.WriteString("设施详情（这些设施均为SmartObject，均附带有可交互的一个或多个interaction列在后面，可进行调用）：\n")
			effects := effectLookup(kb)
			for _, g := range groupObjectsBySemantic(os) {
				label := g.SemanticGroup
				if g.DisplayName != "" && g.DisplayName != g.SemanticGroup {
					label = fmt.Sprintf("%s（%s）", g.DisplayName, g.SemanticGroup)
				}
				meta := ""
				if g.InstanceCount > 1 {
					meta += fmt.Sprintf("，%d 个实例", g.InstanceCount)
				}
				sb.WriteString("- " + label + meta + "：\n")
				// 无速率声明的交互只列动词；有声明的带描述+属性变动+门槛。
				if len(g.AvailableInteractions) == 0 {
					if d := strings.TrimRight(strings.TrimSpace(g.Description), "。"); d != "" {
						sb.WriteString("  " + d + "。\n")
					}
					continue
				}
				for _, itx := range g.AvailableInteractions {
					if e, ok := effects[g.SemanticGroup+"/"+itx]; ok {
						sb.WriteString("  - " + itx + "：" + describeEffect(e) + "\n")
					} else {
						sb.WriteString("  - " + itx + "\n")
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// effectLookup builds a (semantic_group, interaction) → InteractionEffect map
// from the merged KB's declared interaction rates.
func effectLookup(kb *worldkb.KB) map[string]InteractionEffect {
	effects := InteractionEffectsFromKB(kb)
	if len(effects) == 0 {
		return nil
	}
	m := make(map[string]InteractionEffect, len(effects))
	for _, e := range effects {
		m[e.SemanticGroup+"/"+e.Interaction] = e
	}
	return m
}
