package main

// P4-12（旧反应层退役）事件合成器：把 Agent 侧检测到的本地状态变化
// （物理警戒带突破 / 动作异常完成 / Director event_notification）合成为
// world_event 走统一事件管道（入队 → 路由器裁决 → 打断或等安全点 drain）。
// 这是 UE 侧 world_event 推送未实现前的过渡层——UE 就绪后本文件整体删除。
//
// 合成的事件 category 按 detail 内容推断（简化：物理告警 → physical_threshold，
// 动作异常 → action_anomaly，其余 → world）。event_id 用 synth_ 前缀与 UE 的
// evt_ 前缀区分，去重互不干扰。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// nextSynthEventSeq numbers synthesized event ids within the process.
var synthEventSeq int64

// dispatchSynthesizedEvent enqueues a locally detected state change as a
// world_event, then routes it through the standard pipeline (P2-5 router
// decides interrupt-or-enqueue; P3-7 drains it into the tactical prompt at
// the next safe point).
func (rt *Runtime) dispatchSynthesizedEvent(ac *agentContext, agentID, detail string) {
	ev := synthesizeEvent(ac, agentID, detail)
	ac.as.EnqueueWorldEvent(ev)
	rt.logger.Info("[事件合成] 本地状态变化已合成 world_event 入队",
		"agent_id", agentID, "event_id", ev.EventID,
		"category", ev.Category, "event_type", ev.EventType, "detail", detail)
	// 路由器裁决（与 UE 事件同一管道）。
	if rt.eventRouter != nil {
		go rt.eventRouter.route(rt.ctx, agentID, ev)
	}
}

// synthesizeEvent builds a WorldEventPayload from a locally detected detail.
// Category is inferred from the detail text: 物理告警 → physical_threshold,
// 动作异常（result=failed/...）→ action_anomaly, 其余 → world.
func synthesizeEvent(ac *agentContext, agentID, detail string) protocol.WorldEventPayload {
	category := protocol.CategoryWorld
	eventType := "environment_change"
	data := map[string]any{"description": detail}

	switch {
	case strings.Contains(detail, "警戒带"):
		category = protocol.CategoryPhysicalThreshold
		eventType = classifyPhysicalEvent(detail)
		// 附加当前值（detail 形如 "energy 45→39 跌破警戒带 40"）。
		data = map[string]any{"description": detail}
	case strings.Contains(detail, "result="):
		category = protocol.CategoryActionAnomaly
		eventType = protocol.EventTypeActionFailed
	case strings.Contains(detail, "event_notification"):
		category = protocol.CategoryWorld
		eventType = protocol.EventTypeEnvironmentChange
	}

	synthEventSeq++
	ev := protocol.WorldEventPayload{
		EventID:    fmt.Sprintf("synth_%d", synthEventSeq),
		Category:   category,
		EventType:  eventType,
		Force:      false, // 本地检测一律非 force——紧急与否由路由器判
		Severity:   4,
		Subject:    agentID,
		GameTime:   ac.as.LatestTimeOfDay(),
		OccurredAt: 0, // 由 EnqueueWorldEvent 侧不需要；日志时间戳足够
		Data:       mustMarshal(data),
	}
	return ev
}

// classifyPhysicalEvent maps the physical-alert detail text to the protocol
// event_type ("energy 45→39 跌破警戒带 40" → energy_below).
func classifyPhysicalEvent(detail string) string {
	switch {
	case strings.Contains(detail, "energy"):
		return protocol.EventTypeEnergyBelow
	case strings.Contains(detail, "fatigue"):
		return protocol.EventTypeFatigueAbove
	case strings.Contains(detail, "joint_wear"):
		return protocol.EventTypeJointWearAbove
	default:
		return protocol.EventTypeEnergyBelow
	}
}

// mustMarshal is a no-fail JSON marshal for internal maps.
func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
