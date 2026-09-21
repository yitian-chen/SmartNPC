package prompt

// Lightweight event router (事件驱动设计 §4.3, docs/
// AgentTown_WorldEvent_Protocol.md §六-2).
//
// The router answers exactly one question per non-force world event:
// interrupt what the NPC is doing, or enqueue for the next safe point?
// It never plans — planning is the tactical layer's job; the router only
// stamps the event. Judgment inputs: persona (性格), relationships (与事件
// 主体的关系——同一条"K-03 故障"对阿静是急事、对老陈不是), current action
// progress, and the event's objective facts (who/when/where/severity).
// Cost asymmetry drives the tie-break: a missed interrupt makes the NPC
// look dull for minutes; a wrong interrupt drags out a full replan and
// wrecks the schedule — so when unsure, enqueue (§4.3 判不动的时候倾向入队).
//
// The verdict comes from the Venus judgment model (jev, POST /v1/systemone):
// this layer renders the decision context into a single state string
// (BuildRouterState); the judgment criteria and the answer mapping live in
// cmd/agenttown-mcp/event_router.go (routerQuestions / routerDecisionFromJev).

import (
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// RouterDecision is the router's verdict. Motive names the reason for the
// interrupt — one of the RouterMotive* constants — so the runtime can phrase
// the tactical hint accordingly (emergency treatment vs resume-schedule vs
// brief-social-response).
type RouterDecision struct {
	Interrupt bool   `json:"interrupt"`
	Severity  int    `json:"severity"`
	Reason    string `json:"reason"`
	Motive    string `json:"motive"`
}

// Router motive constants: WHY the router chose to interrupt (or not).
const (
	// RouterMotiveUrgent: the event itself is urgent/dangerous for this
	// NPC — full emergency treatment (撤离/避险/支援).
	RouterMotiveUrgent = "urgent"
	// RouterMotiveSituationResolved: the event resolves the ongoing
	// situation that motivated the current action — the action is moot,
	// stop it and resume the schedule (e.g. combat_exit while fleeing).
	RouterMotiveSituationResolved = "situation_resolved"
	// RouterMotiveSocial: social etiquette warrants a brief response,
	// then resume the original work (e.g. greeting from a non-hostile NPC).
	RouterMotiveSocial = "social"
)

// RouterInput aggregates everything the router sees. All fields are
// pre-rendered strings (the caller composes them from AgentState / KB /
// relationships), keeping this layer pure and testable.
type RouterInput struct {
	AgentID       string
	AgentName     string
	AgentRole     string // 【你的角色】段（AgentRole），空 → 降级占位
	TimeOfDay     string
	Zone          string
	PhysicalLine  string // 分档渲染后的物理状态行，空 = 省略段
	Relationships string // 【人际关系】bullet 列表，空 = 省略段
	// CurrentAction describes the in-flight action (e.g. "InteractSmartObject
	// (workbench/assemble)，已执行约 47 分钟"); empty = idle.
	CurrentAction string
	// Situations is the pre-rendered active-situation list（P3-9 修复 A）：
	// an ongoing threat colors how urgent a NEW event is.
	Situations string
	// WorldOverview is the shared world setting/theme (prompt.WorldOverview,
	// the same module 1 the strategic/tactical/dialogue layers inject), so
	// the router judges with the same world model as the rest of the mind.
	// Empty → 省略段.
	WorldOverview string
	Event         protocol.WorldEventPayload
}

// BuildRouterState renders the router's decision context into the single
// state string the judgment model (jev) sees. It merges what used to be the
// system message (world setting + persona + relationships) and the user
// message (realtime state / action / event) — the judgment API has no
// system/user split, one state carries everything. Segment wording follows
// the previous prompt pair so verdicts stay anchored to the same context;
// the judgment criteria (three interrupt motives) moved into the questions
// themselves (routerQuestions, cmd layer).
//
// Planning context is deliberately absent: no 生产工作流 module, and the
// world overview goes through worldOverviewForRouter, which drops the zone
// roster and the facility-category roster lines (the tactical layer needs
// them; for an urgency verdict they are noise, and the event itself already
// carries its location). The shared WorldOverview / ProductionWorkflowText
// used by the strategic/tactical/dialogue layers keep everything.
func BuildRouterState(in RouterInput) string {
	agentName := in.AgentName
	if agentName == "" {
		agentName = in.AgentID
	}
	agentRole := in.AgentRole
	if agentRole == "" {
		agentRole = "（无角色信息）"
	}
	action := in.CurrentAction
	if action == "" {
		action = "无（空闲）"
	}
	physicalSeg := ""
	if in.PhysicalLine != "" {
		physicalSeg = in.PhysicalLine + "\n"
	}
	situationsSeg := ""
	if in.Situations != "" {
		situationsSeg = "【当前处境】仍在持续、尚未解除：\n" + in.Situations + "\n"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "小镇居民 NPC %s 收到一条世界事件，需要判断是否打断其当前正在做的事。以下是该 NPC 的背景与当前情况。\n", agentName)
	if in.WorldOverview != "" {
		sb.WriteString("\n【世界背景】\n")
		sb.WriteString(worldOverviewForRouter(in.WorldOverview))
		sb.WriteString("\n")
	}
	sb.WriteString("\n【你的角色】\n")
	sb.WriteString(agentRole)
	sb.WriteString("\n")
	if in.Relationships != "" {
		sb.WriteString("\n【人际关系】\n")
		sb.WriteString(in.Relationships)
		sb.WriteString("\n")
	}
	sb.WriteString("\n【当前状态】\n")
	fmt.Fprintf(&sb, "游戏时间：%s\n位置：%s\n", in.TimeOfDay, in.Zone)
	sb.WriteString(physicalSeg)
	sb.WriteString("\n【当前动作】\n")
	sb.WriteString(action)
	sb.WriteString("\n")
	sb.WriteString(situationsSeg)
	sb.WriteString("\n【收到的事件】\n")
	sb.WriteString(FormatWorldEvent(in.Event))
	sb.WriteString("\n")
	return sb.String()
}

// worldOverviewForRouter strips planning-context lines from a rendered
// WorldOverview for the router state: the zone roster ("区域（N 个）：…")
// and the facility-category roster ("可交互设施类别（N 类）：…") are both
// tactical-layer planning context — for an urgency verdict they are noise,
// and the event itself already carries its location. Line-based: each roster
// is exactly one line, so dropping every line with those prefixes keeps
// 设定/主题/居民 intact regardless of KB shape.
func worldOverviewForRouter(overview string) string {
	lines := strings.Split(overview, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, "区域（") || strings.HasPrefix(l, "可交互设施类别") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
