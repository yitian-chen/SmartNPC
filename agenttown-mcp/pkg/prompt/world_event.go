package prompt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// World-event rendering for LLM prompts (docs/AgentTown_WorldEvent_Protocol.md).
//
// FormatWorldEvent is the single rendering source shared by the force
// interrupt hint (P1-3) and the tactical layer's 【发生的事件】 segment (P3-7),
// so an event reads identically wherever the LLM meets it. Unknown
// categories / event types degrade to their raw ids rather than erroring —
// the protocol may grow new types (forward compatibility, §三).

// FormatWorldEvent renders one world event as a compact natural-language
// line: 类别：描述（主体…，客观严重度 N，地点…，游戏时间…）. Optional
// fields are omitted; unknown enums degrade to raw ids.
func FormatWorldEvent(ev protocol.WorldEventPayload) string {
	parts := make([]string, 0, 4)
	if ev.Subject != "" {
		parts = append(parts, "主体 "+ev.Subject)
	}
	if ev.Severity > 0 {
		parts = append(parts, fmt.Sprintf("客观严重度 %d", ev.Severity))
	}
	if ev.Location != "" {
		parts = append(parts, "地点 "+ev.Location)
	}
	if ev.GameTime != "" {
		parts = append(parts, "游戏时间 "+ev.GameTime)
	}
	out := fmt.Sprintf("%s：%s", worldEventCategoryLabel(ev.Category), worldEventDescription(ev))
	if len(parts) > 0 {
		out += "（" + strings.Join(parts, "，") + "）"
	}
	return out
}

// FormatWorldEventList renders queued world events as a bullet list for the
// tactical prompt's 【发生的事件】 segment (P3-7 safe-point drain). FIFO
// order (oldest first — chronological narrative); empty → "" (caller omits
// the segment).
func FormatWorldEventList(events []protocol.WorldEventPayload) string {
	if len(events) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, ev := range events {
		sb.WriteString("- " + FormatWorldEvent(ev) + "\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// worldEventCategoryLabel maps a category constant to its Chinese label.
func worldEventCategoryLabel(category string) string {
	switch category {
	case protocol.CategoryPhysicalThreshold:
		return "物理状态跨阈值"
	case protocol.CategorySpatial:
		return "空间变化"
	case protocol.CategorySocial:
		return "社交信号"
	case protocol.CategoryActionAnomaly:
		return "动作异常"
	case protocol.CategoryWorld:
		return "世界事件"
	case protocol.CategoryPlayerInteraction:
		return "玩家互动"
	default:
		return "事件（未知类别 " + category + "）"
	}
}

// worldEventDescription renders the category-specific body from the typed
// data block. Parse failures and unknown event types fall back to the raw
// event_type id.
func worldEventDescription(ev protocol.WorldEventPayload) string {
	switch ev.Category {
	case protocol.CategoryPhysicalThreshold:
		var d protocol.PhysicalThresholdData
		if err := json.Unmarshal(ev.Data, &d); err == nil && d.Attribute != "" {
			return fmt.Sprintf("%s%s阈值 %.0f，当前 %.1f",
				thresholdAttributeLabel(d.Attribute), thresholdDirectionLabel(d.Direction), d.Threshold, d.Value)
		}
	case protocol.CategorySpatial:
		var d protocol.SpatialData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			switch ev.EventType {
			case protocol.EventTypeZoneEnter:
				if d.Zone != "" {
					return fmt.Sprintf("进入 %s（来自 %s）", d.Zone, d.From)
				}
			case protocol.EventTypeZoneExit:
				if d.Zone != "" {
					return fmt.Sprintf("离开 %s（前往 %s）", d.Zone, d.To)
				}
			case protocol.EventTypeAgentNearby:
				if d.OtherAgent != "" {
					return fmt.Sprintf("%s 进入对话距离，相距约 %.1f 米", d.OtherAgent, d.DistanceCm/100)
				}
			case protocol.EventTypeAgentLeaveNearby:
				if d.OtherAgent != "" {
					return fmt.Sprintf("%s 离开对话距离", d.OtherAgent)
				}
			}
		}
	case protocol.CategorySocial:
		var d protocol.SocialData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			switch ev.EventType {
			case protocol.EventTypeChatInviteIncoming:
				if d.From != "" {
					return fmt.Sprintf("%s 向你发起对话：%s", d.From, d.Content)
				}
			case protocol.EventTypeBroadcastHeard:
				if d.Source != "" {
					return fmt.Sprintf("听到 %s 的广播：%s", d.Source, d.Content)
				}
			case protocol.EventTypeMentioned:
				if d.Source != "" {
					return fmt.Sprintf("被 %s 提及：%s", d.Source, d.Context)
				}
			}
		}
	case protocol.CategoryActionAnomaly:
		var d protocol.ActionAnomalyData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			switch ev.EventType {
			case protocol.EventTypeActionFailed:
				return fmt.Sprintf("动作 %s 执行失败（原因 %s）", d.Cmd, d.Reason)
			case protocol.EventTypeSmartObjectOccupied:
				return fmt.Sprintf("目标设施 %s 被 %s 占用", d.SemanticGroup, d.OccupiedBy)
			}
		}
	case protocol.CategoryWorld:
		var d protocol.WorldEventData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			switch ev.EventType {
			case protocol.EventTypeMalfunction:
				return "发生故障：" + d.Description
			case protocol.EventTypeEnvironmentChange:
				return "环境变化：" + d.Description
			case protocol.EventTypeDirectorDirective:
				return "剧情指令：" + d.Description
			}
		}
	case protocol.CategoryPlayerInteraction:
		var d protocol.PlayerInteractionData
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			switch ev.EventType {
			case protocol.EventTypePlayerAttacked:
				return fmt.Sprintf("被玩家 %s 攻击（伤害 %.0f，类型 %s）", d.Attacker, d.Damage, d.DamageType)
			case protocol.EventTypePlayerTargeted:
				return fmt.Sprintf("被玩家 %s 瞄准/锁定", d.Attacker)
			case protocol.EventTypeCombatExit:
				return fmt.Sprintf("与玩家 %s 的战斗结束（%s）", d.Attacker, d.Outcome)
			case protocol.EventTypePlayerInteract:
				return fmt.Sprintf("玩家 %s 向你发起交互（%s）：%s", d.Player, d.Action, d.Detail)
			}
		}
	}
	// Fallback: unknown type or unparseable data — keep the raw id so the
	// LLM at least knows something of this kind happened.
	return "发生了 " + ev.EventType + " 事件"
}

// thresholdAttributeLabel maps a physical attribute id to its Chinese name.
func thresholdAttributeLabel(attr string) string {
	switch attr {
	case protocol.ThresholdAttrEnergy:
		return "能量"
	case protocol.ThresholdAttrFatigue:
		return "疲劳"
	case protocol.ThresholdAttrJointWear:
		return "关节磨损"
	case protocol.ThresholdAttrMoney:
		return "余额"
	default:
		return attr
	}
}

// thresholdDirectionLabel maps a crossing direction to its Chinese verb.
func thresholdDirectionLabel(direction string) string {
	switch direction {
	case protocol.ThresholdDirectionBelow:
		return "向下跌破"
	case protocol.ThresholdDirectionAbove:
		return "向上升破"
	default:
		return "跨过"
	}
}
