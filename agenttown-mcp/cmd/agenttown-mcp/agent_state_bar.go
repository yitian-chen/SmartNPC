// Package main — <agent_state> 状态栏（事件驱动设计 §6.1）。
//
// 状态栏是每次 LLM 调用注入 messages 数组末尾的瞬态 user 消息：当前游戏
// 时间、物理状态（分档自然语言）、当前日程与剩余时间、当前动作与剩余
// 时间四行。设计约束：
//   - 前缀保持稳定，每轮都变的内容放尾部——KV cache 失效的只有尾巴；
//   - 不入会话历史：历史里留存旧状态栏只会误导后续轮次，每轮请求从
//     AgentState 现查现拼，下一轮自然刷新；
//   - 物理状态用分段后的自然语言（PhysicalLine 分档），不喂裸数值。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
)

// agentStateBarOpen / agentStateBarClose 是状态栏消息的定界标签。
const (
	agentStateBarOpen  = "<agent_state>"
	agentStateBarClose = "</agent_state>"
)

// buildAgentStateBar 构造 <agent_state> 状态栏内容。无感知数据（UE 尚未
// 推送首条 perception_update，游戏时间/物理状态均未知）时返回空串，调用
// 方跳过注入——空栏没有信息量，反而白占尾部 token。
func (a *agentContext) buildAgentStateBar(agentID string, profiles map[string]*profile.Profile) string {
	snap := a.as.Snapshot()

	var env protocol.Environment
	hasPerception := false
	if len(snap.LatestPerception) > 0 {
		var p protocol.PerceptionPayload
		if err := json.Unmarshal(snap.LatestPerception, &p); err == nil {
			env = p.Environment
			hasPerception = true
		}
	}
	// 无感知（UE 尚未推送首条 perception_update）→ 游戏时间/物理状态均
	// 未知，状态栏无从谈起。TimeOfDaySec=0（午夜）是合法值，不能用它
	// 区分"无感知"，必须用解析是否成功判定。
	if !hasPerception {
		return ""
	}
	todSec := env.TimeOfDaySec
	if todSec < 0 || todSec >= 86400 {
		if env.GameTimeSec > 0 {
			todSec = math.Mod(env.GameTimeSec, 86400)
		} else {
			todSec = 0
		}
	}

	var b strings.Builder
	b.WriteString(agentStateBarOpen + "\n")
	b.WriteString(barGameTimeLine(env, todSec) + "\n")
	b.WriteString(prompt.PhysicalLine(snap.LatestPhysical, prompt.BandThresholdsFor(profiles, agentID)) + "\n")
	b.WriteString(barScheduleLine(&snap, todSec) + "\n")
	b.WriteString(a.barActionLine(&snap, env.GameTimeSec, todSec) + "\n")
	// P4-10（§6.1）补全字段：未完成任务槽 + 上次动作结束原因。缺失的
	// 行整行省略——状态栏保持紧凑，有事实才占行。
	if snap.UnfinishedTask != "" {
		b.WriteString("未完成：" + snap.UnfinishedTask + "\n")
	}
	if line := barLastEndLine(&snap); line != "" {
		b.WriteString(line + "\n")
	}
	b.WriteString(agentStateBarClose)
	return b.String()
}

// interruptedTaskDesc builds the unfinished-task description captured at a
// stop point: tool name + key params + elapsed game time. Call BEFORE the
// in-flight tracking is cleared.
func (a *agentContext) interruptedTaskDesc(snap *agentstate.Snapshot, gameSec float64) string {
	desc := tools.CmdToToolName(snap.CurrentActionCmd)
	if extra := barActionParams(snap.CurrentActionParams); extra != "" {
		desc += "(" + extra + ")"
	}
	if snap.CurrentActionStartGame > 0 && gameSec > snap.CurrentActionStartGame {
		desc += fmt.Sprintf("，已执行约 %d 分钟", int((gameSec-snap.CurrentActionStartGame)/60))
	}
	return desc
}

// recordInterrupted captures the in-flight action as the unfinished-task
// slot with the interrupt reason (P4-10). No-op when nothing is in flight.
// Call BEFORE the in-flight tracking is cleared at any stop point.
func (a *agentContext) recordInterrupted(reason string) {
	snap := a.as.Snapshot()
	if snap.CurrentActionCmd == "" {
		return
	}
	a.as.RecordActionInterrupted(a.interruptedTaskDesc(&snap, a.as.LatestGameTimeSec()), reason)
}

// barLastEndLine renders the 上次动作结束原因 line; "" when no action has
// ended yet. §3.4：三种结束方式必须区分——把失败/打断当成功继续走是
// 最典型的幻觉来源。
func barLastEndLine(snap *agentstate.Snapshot) string {
	if snap.LastEndResult == "" {
		return ""
	}
	label := map[string]string{
		"success":     "正常完成",
		"failed":      "失败",
		"interrupted": "被中断",
		"error":       "异常结束",
	}[snap.LastEndResult]
	if label == "" {
		label = snap.LastEndResult
	}
	line := "上次动作结束：" + label
	if snap.LastEndWhy != "" {
		line += "（" + snap.LastEndWhy + "）"
	}
	return line
}

// barGameTimeLine 渲染当前游戏时间行。DayCount 从 0 起（约定 19），展示为
// D<天数>（DayCount=11 → D12）；秒级精度取 TimeOfDaySec（UE 派生字段）。
func barGameTimeLine(env protocol.Environment, todSec float64) string {
	day := env.DayCount
	if day < 0 {
		day = int(env.GameTimeSec / 86400)
		if day < 0 {
			day = 0
		}
	}
	total := int(todSec)
	return fmt.Sprintf("当前游戏时间：D%d %02d:%02d:%02d", day+1, total/3600, (total%3600)/60, total%60)
}

// barScheduleLine 渲染当前日程行：[序号/总数] 时段 goal（剩余时间）。
// 时段与 goal 取自 dailyPlan/currentSlot/currentPlanIndex（与 /debug/tactical
// 同一查法：先按 index、再按时段文本回退）；剩余时间按权威游戏时间推算，
// 跨午夜时段经 NormalizeTodToSlot 归一。无计划时明确说"无"，而不是省略——
// 让 LLM 知道此刻没有日程约束。
func barScheduleLine(snap *agentstate.Snapshot, todSec float64) string {
	slot := snap.CurrentSlot
	if slot == "" {
		return "当前日程：无（尚未生成当日计划）"
	}
	items := parseFormattedPlan(snap.DailyPlan)
	goal := ""
	if snap.CurrentPlanIndex >= 0 && snap.CurrentPlanIndex < len(items) {
		goal = items[snap.CurrentPlanIndex].Goal
	} else {
		for _, it := range items {
			if it.Time == slot {
				goal = it.Goal
				break
			}
		}
	}

	line := "当前日程："
	if len(items) > 0 && snap.CurrentPlanIndex >= 0 && snap.CurrentPlanIndex < len(items) {
		line += fmt.Sprintf("[%d/%d] ", snap.CurrentPlanIndex+1, len(items))
	}
	line += slot
	if goal != "" {
		line += " " + goal
	}
	if start, end := prompt.SlotRangeMinute(slot); start >= 0 {
		curMin := prompt.NormalizeTodToSlot(int(todSec)/60, start, end)
		if remain := end - curMin; remain > 0 {
			line += "（剩余" + barDurMinute(remain) + "）"
		} else {
			line += "（已到时段末尾，即将切换）"
		}
	}
	return line
}

// barActionLine 渲染当前动作行：工具名(关键参数)（剩余时间，预计结束时刻）。
// 在途动作取自 RecordActionStarted 记录；剩余时间仅当 time_to_stop 已武装
// 且跟踪的正是当前动作时给出（与 checkTimeToStop 同一口径）。未设
// time_to_stop 的长复合动作持续到时段切换，不展示剩余。
func (a *agentContext) barActionLine(snap *agentstate.Snapshot, gameSec, todSec float64) string {
	if snap.CurrentActionCmd == "" {
		return "当前动作：无（空闲）"
	}
	desc := tools.CmdToToolName(snap.CurrentActionCmd)
	if extra := barActionParams(snap.CurrentActionParams); extra != "" {
		desc += "(" + extra + ")"
	}
	// P4-10：已执行时长（§6.1 防反复重启同一动作）。游戏时间口径，无
	// 感知起点时省略。
	if snap.CurrentActionStartGame > 0 && gameSec > snap.CurrentActionStartGame {
		desc += fmt.Sprintf("，已执行约 %d 分钟", int((gameSec-snap.CurrentActionStartGame)/60))
	}
	if target, _, tsActionID, armed := a.as.TimeStop(); armed && tsActionID == snap.CurrentActionID && gameSec > 0 {
		if remain := target - gameSec; remain > 0 {
			endSec := math.Mod(todSec+remain, 86400)
			desc += fmt.Sprintf("（剩余%s，预计 %02d:%02d 结束）",
				barDurMinute(int(remain/60)), int(endSec)/3600, (int(endSec)%3600)/60)
		} else {
			desc += "（time_to_stop 已到点，即将结束）"
		}
	}
	return "当前动作：" + desc
}

// barActionParams 从在途动作参数里挑最有辨识度的一项渲染：设施交互的
// semantic_group/interaction 优先，其次 behavior / 目标 agent / 目标 id，
// 文本类（speak 的 content、generic_act 的 thought）截断展示。
func barActionParams(params map[string]any) string {
	if sg, _ := params["semantic_group"].(string); sg != "" {
		if itx, _ := params["interaction"].(string); itx != "" {
			return sg + "/" + itx
		}
		return sg
	}
	if b, _ := params["behavior"].(string); b != "" {
		return b
	}
	if t, _ := params["target_agent_id"].(string); t != "" {
		return t
	}
	if tid, _ := params["target_id"].(string); tid != "" {
		return tid
	}
	if c, _ := params["content"].(string); c != "" {
		return "“" + truncateRunes(c, 16) + "”"
	}
	if th, _ := params["thought"].(string); th != "" {
		return "“" + truncateRunes(th, 16) + "”"
	}
	return ""
}

// barDurMinute 把分钟数格式化为自然语言时长：<60 → "约 N 分钟"；
// ≥60 → "约 X 小时[Y 分钟]"。
func barDurMinute(m int) string {
	if m < 60 {
		return fmt.Sprintf("约 %d 分钟", m)
	}
	h, r := m/60, m%60
	switch r {
	case 0:
		return fmt.Sprintf("约 %d 小时", h)
	default:
		return fmt.Sprintf("约 %d 小时 %d 分钟", h, r)
	}
}

// truncateRunes 按 rune 截断字符串，超长补省略号。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
