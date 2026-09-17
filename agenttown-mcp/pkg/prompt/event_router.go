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
type RouterDecision struct {
	Interrupt bool   `json:"interrupt"`
	Severity  int    `json:"severity"`
	Reason    string `json:"reason"`
}

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
	Event         protocol.WorldEventPayload
}

// RouterSystemPrompt is the router's system message: mechanism only (the
// single question, the cost asymmetry, JSON shape). Static across calls →
// cacheable. BuildRouterSystem appends the per-agent judgment identity
// (persona + relationships), which changes rarely within a day.
const RouterSystemPrompt = `你是小镇居民 NPC 的事件路由模块。世界发生了一件事，正在推送给该 NPC。你只回答一个问题：要不要打断 NPC 手上正在做的事？

【只有两种结论】
- interrupt=true：这件事对该 NPC 足够紧急，值得立即打断当前行动去处理
- interrupt=false：不紧急，入队，等当前动作完成后的下一个决策点一并处理

【判断要点】
- 紧急与否由该 NPC 结合自己的性格、与事件主体的关系、当前处境判断——同一条事件对不同 NPC 结论可以不同
- 代价不对称：误打断会拖出一整轮重规划、搅乱日程；漏打断只是反应慢几分钟。拿不准时一律 interrupt=false
- 你只裁决紧急度，不规划具体做什么——做什么、做多久由后续的战术规划决定
- 事件描述里的 severity 是客观严重度（世界视角的量级），不是"对该 NPC 紧不紧急"，仅供参考

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"interrupt": true|false, "severity": 0-10, "reason": "简短理由（一句话）"}

其中 severity 是你评估的"对该 NPC 的主观紧急度"（0-10），reason 说明关键依据（如关系、距离、性格）。`

// BuildRouterSystem constructs the router's system message: the static
// mechanism text plus the per-agent judgment identity (【你的角色】 +
// 【人际关系】). Identity lives in the system message (not the user
// message) so the user message carries only what changes per event —
// state, action, event — keeping the per-call prefix minimal.
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
	sb.WriteString("\n\n")
	fmt.Fprintf(&sb, "你是 NPC %s 的事件路由模块。判断时的角色与关系背景如下：\n\n", agentName)
	sb.WriteString("【你的角色】\n")
	sb.WriteString(agentRole)
	sb.WriteString("\n")
	if in.Relationships != "" {
		sb.WriteString("\n【人际关系】\n")
		sb.WriteString(in.Relationships)
		sb.WriteString("\n")
	}
	return sb.String()
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
	return fmt.Sprintf(RouterUserTemplate,
		agentName,
		in.TimeOfDay,
		in.Zone,
		physicalSeg,
		action,
		FormatWorldEvent(in.Event),
	)
}

// ParseRouterDecision parses the router LLM's output.
//
// Fault tolerance (判不动的时候倾向入队): JSON parse failure, a missing
// interrupt field, and any malformed input all degrade to interrupt=false —
// the conservative enqueue. A well-formed interrupt=true must survive.
func ParseRouterDecision(raw string) RouterDecision {
	fallback := RouterDecision{Interrupt: false, Severity: 0, Reason: "parse_failed: " + truncate(raw, 80)}
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
	return dec
}
