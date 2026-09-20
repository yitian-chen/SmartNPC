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

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// RouterDecision is the JSON verdict expected from the router LLM.
// Motive names the reason for the interrupt — one of the RouterMotive*
// constants — so the runtime can phrase the tactical hint accordingly
// (emergency treatment vs resume-schedule vs brief-social-response).
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

// RouterSystemPrompt is the router's system message: mechanism only (the
// single question, the cost asymmetry, JSON shape). Static across calls →
// cacheable. BuildRouterSystem appends the per-agent judgment identity
// (persona + relationships), which changes rarely within a day.
const RouterSystemPrompt = `你是小镇居民 NPC 的事件路由模块。世界发生了一件事，正在推送给该 NPC。你回答一个问题：要不要打断 NPC 手上正在做的事？

【打断的三种理由——满足任一即 interrupt=true，motive 填对应值】

1. urgent（紧急处理）：事件本身对该 NPC 足够紧急或危险，值得立即放下手头的事去应对。
   典型：被攻击、被瞄准、亲近的伙伴发生严重故障。

2. situation_resolved（情境解除）：事件解除了当前持续情境，使 NPC 正在做的动作不再有必要。
   例如，当NPC被玩家攻击后正在逃跑，此时收到了脱离战斗的信息，则逃跑动作不再有必要，可以打断。

3. social（社交回应）：事件是社交性质的（有人打招呼、搭话、点名），且该 NPC 与事件主体关系不差，
   社交礼节上值得停下简短回应一声，然后继续原工作。
   关系恶劣或敌对时不用为此打断。

【不打断】
以上三种都不满足 → interrupt=false，入队等当前动作完成后的下一个决策点一并处理。

【判断要点】
- 同一条事件对不同 NPC 结论可以不同（性格、关系、当前处境不同）
- 事件描述里的 severity 是客观严重度（世界视角的量级），仅供参考——你评的 severity 是主观紧急度（0-10），但打断决策看上述三种理由，不只看 severity
- situation_resolved 型打断的 severity 通常不高（事件本身不危险），但打断依然合理
- social 型打断的 severity 应很低（1-3），且关系恶劣时不触发

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"interrupt": true|false, "severity": 0-10, "reason": "简短理由", "motive": "urgent|situation_resolved|social"}`

// BuildRouterSystem constructs the router's system message: the static
// mechanism text, the shared world setting/theme (【世界背景】+【生产工作流】，
// the same modules the other three layers inject — 路由器与整套心智共用
// 同一份世界模型), and the per-agent judgment identity (【你的角色】 +
// 【人际关系】). Identity lives in the system message (not the user
// message) so the user message carries only what changes per event —
// state, action, event — keeping the per-call prefix minimal.
//
// The world overview is passed through worldOverviewWithoutZones: the zone
// roster line is dropped for the router only (zones are planning context —
// the tactical layer needs them; for an urgency verdict they are noise,
// and the event itself already carries its location). The shared
// WorldOverview used by the strategic/tactical/dialogue layers keeps the
// zone roster.
func BuildRouterSystem(in RouterInput) string {
	agentName := in.AgentName
	if agentName == "" {
		agentName = in.AgentID
	}
	agentRole := in.AgentRole
	if agentRole == "（无角色信息）" || agentRole == "" {
		agentRole = "（无角色信息）"
	}
	var sb strings.Builder
	sb.WriteString(RouterSystemPrompt)
	if in.WorldOverview != "" {
		sb.WriteString("\n\n【世界背景】\n")
		sb.WriteString(worldOverviewWithoutZones(in.WorldOverview))
	}
	sb.WriteString("\n\n【生产工作流】\n")
	sb.WriteString(ProductionWorkflowText)
	fmt.Fprintf(&sb, "\n\n你是 NPC %s 的事件路由模块。判断时的角色与关系背景如下：\n", agentName)
	sb.WriteString("\n【你的角色】\n")
	sb.WriteString(agentRole)
	sb.WriteString("\n")
	if in.Relationships != "" {
		sb.WriteString("\n【人际关系】\n")
		sb.WriteString(in.Relationships)
		sb.WriteString("\n")
	}
	return sb.String()
}

// worldOverviewWithoutZones strips the zone-roster line ("区域（N 个）：…")
// from a rendered WorldOverview. Line-based: the zone roster is exactly one
// line, so dropping every line with that prefix keeps 设定/主题/设施/居民
// intact regardless of KB shape.
func worldOverviewWithoutZones(overview string) string {
	lines := strings.Split(overview, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, "区域（") {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}

// RouterUserTemplate is the router's user message template. Per-call data
// only: realtime state, in-flight action, the event.
const RouterUserTemplate = `NPC %s 收到一条世界事件，请裁决是否打断当前行动。

【当前状态】
游戏时间：%s
位置：%s
%s
【当前动作】
%s
%s
【收到的事件】
%s

请给出你的裁决。`

// BuildRouterPrompt constructs the router's user message. Pure function.
// Per-call data only (state / action / event); the per-agent identity
// (persona + relationships) goes into the system message — see
// BuildRouterSystem.
func BuildRouterPrompt(in RouterInput) string {
	agentName := in.AgentName
	if agentName == "" {
		agentName = in.AgentID
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
	return fmt.Sprintf(RouterUserTemplate,
		agentName,
		in.TimeOfDay,
		in.Zone,
		physicalSeg,
		action,
		situationsSeg,
		FormatWorldEvent(in.Event),
	)
}

// ParseRouterDecision parses the router LLM's output.
//
// Fault tolerance (判不动的时候倾向入队): JSON parse failure, a missing
// interrupt field, and any malformed input all degrade to interrupt=false —
// the conservative enqueue. A well-formed interrupt=true must survive.
func ParseRouterDecision(raw string) RouterDecision {
	fallback := RouterDecision{Interrupt: false, Severity: 0, Reason: "parse_failed: " + truncate(raw, 80), Motive: RouterMotiveUrgent}
	cleaned := StripCodeFence(raw)
	// Tolerate trailing prose / fenced blocks: locate the first { to last }.
	start := strings.IndexByte(cleaned, '{')
	end := strings.LastIndexByte(cleaned, '}')
	if start < 0 || end <= start {
		return fallback
	}
	var dec RouterDecision
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &dec); err != nil {
		return fallback
	}
	if dec.Severity < 0 {
		dec.Severity = 0
	}
	if dec.Severity > 10 {
		dec.Severity = 10
	}
	if dec.Reason == "" {
		dec.Reason = "（模型未给出理由）"
	}
	// Motive 降级：空或非法值 → urgent（向后兼容，已有中断语义不变）。
	switch dec.Motive {
	case RouterMotiveUrgent, RouterMotiveSituationResolved, RouterMotiveSocial:
		// 合法值保留。
	default:
		dec.Motive = RouterMotiveUrgent
	}
	return dec
}
