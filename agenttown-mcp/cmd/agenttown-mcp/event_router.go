package main

// Lightweight event router (事件驱动设计 §4.3, P2-5).
//
// Every non-force world event goes through one judgment-model call that
// answers exactly one question: interrupt or enqueue? The verdict's
// interrupt branch reuses the force machinery minus the hard guarantee
// (stop with progress + replan with the event injected); the enqueue branch
// is the default and the failure fallback (判不动的时候倾向入队 — the cost
// asymmetry).
//
// Router calls go through the Venus judgment API (pkg/jev, POST
// /v1/systemone): a state string (prompt.BuildRouterState merges what used
// to be the system+user prompts) plus three typed questions — noul
// (should_interrupt 概率), choice (motive), score (severity). ~1s latency,
// no JSON-in-prose parsing, judgment criteria travel inside the questions.
// Unlike the old flash-LLM path there is no conversation history involved:
// each verdict is a stateless one-shot. A cancelled/failed verdict leaves
// no trace (the event is already enqueued at receipt).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/jev"
	"github.com/AgentTown/agenttown-mcp/pkg/llmmetrics"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// routerJudge is the router's slice of the judgment client: one call plus
// the dump hook. Narrow so tests can script it without dragging the full
// llmClient surface around.
type routerJudge interface {
	Judge(ctx context.Context, req *jev.Request) (*jev.Response, error)
	LastRequestBody() []byte
}

// routerInterruptNoulThreshold is the noul probability above which
// should_interrupt counts as an interrupt (strictly greater — a tie stays
// conservative). Live measurements: attack ~0.92, stranger-greeting ~0.15.
const routerInterruptNoulThreshold = 0.5

// routerJudgeSeverityCapSocial caps severity for social-motive interrupts:
// the guardrail ladder compares severities, and a social response must not
// outrank genuine emergencies (the old LLM path asked for severity 1-3).
const routerJudgeSeverityCapSocial = 3

// routerQuestions is the fixed question set for every router call. The
// judgment criteria (three interrupt motives) live here, in the questions —
// the jev API has no system prompt, instructions carry the criteria. The
// instructions read the state's "user" attributes and "conversation"
// (agentic-loop history) alongside.
var routerQuestions = map[string]jev.Question{
	"should_interrupt": jev.NoulQuestion(
		"结合该 NPC 的角色、人际关系、近期对话（conversation，它最近在被要求做什么、做了什么）、当前动作与处境判断：是否应该打断该 NPC 当前正在做的事去应对这条事件？" +
			"事件威胁到该 NPC 或其亲近伙伴时应打断（被攻击、被瞄准、亲近的伙伴发生严重故障）；" +
			"社交事件（有人打招呼、搭话、点名）默认值得停下简短回应，对陌生玩家也一样（礼貌回应后继续原工作），" +
			"只有关系明确恶劣或敌对时才不理会。注意当前动作可能是对先前紧急事件的反应（如正在逃跑）。" +
			"事件描述里的客观严重度是世界视角量级，仅供参考。紧急类事件拿不准时倾向不打断（错误打断会拖出一整轮重规划）。"),
	"motive": jev.ChoiceQuestion(
		"若应打断，最主要原因属于哪种？若不应打断，选 no_interrupt。",
		map[string]string{
			"urgent":             "事件本身对该 NPC 紧急或危险（被攻击、被瞄准、亲近的伙伴严重故障），需立即应对",
			"situation_resolved": "事件解除了当前持续情境，使正在做的动作不再有必要（如逃跑中收到脱离战斗信号，应停止逃跑回日程）",
			"social":             "事件是社交性质的，礼节上值得停下简短回应后继续原工作（对陌生玩家也一样，除非关系恶劣）",
			"no_interrupt":       "以上都不满足，不需要打断",
		}),
	"severity": jev.ScoreQuestion(
		"该事件对该 NPC 的主观紧急程度（situation_resolved 型通常不高；social 型应很低）。",
		"日常小事，无需在意", "紧急事件，需要立即处理"),
}

// routerConversationWindow caps the conversation tail sent to the judgment
// model. The tactical history is already bounded within a day (compaction
// folds the head into a summary), but the first-of-day full header alone is
// ~2KB; the recent tail is what colors the verdict, so anything older is cut.
const routerConversationWindow = 20

// routerConversation renders the agent's agentic-loop history (tactical-layer
// conversation) as the judgment state's "conversation" array — this is what
// gives the router its context knowledge (what the NPC has been asked to do,
// what it did, what it said). user turns carry the tactical instructions
// verbatim; assistant turns carry their text, or the tool calls rendered as
// compact one-liners; tool-role placeholders (result=pending) carry no
// information and are skipped — real action results arrive in the history as
// user-role messages already.
func routerConversation(as *agentstate.AgentState) []jev.ConversationMessage {
	hist := as.Conversation()
	if len(hist) > routerConversationWindow {
		hist = hist[len(hist)-routerConversationWindow:]
	}
	out := make([]jev.ConversationMessage, 0, len(hist))
	for _, m := range hist {
		switch m.Role {
		case "user":
			if m.Content == "" {
				continue
			}
			out = append(out, jev.ConversationMessage{Role: "user", Content: m.Content})
		case "assistant":
			content := m.Content
			if content == "" && len(m.ToolCalls) > 0 {
				parts := make([]string, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					var args map[string]any
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
					parts = append(parts, describeAction(tc.Function.Name, args))
				}
				content = joinStrings(parts, "；")
			}
			if content != "" {
				out = append(out, jev.ConversationMessage{Role: "assistant", Content: content})
			}
		}
	}
	return out
}

// routerDecisionFromJev maps a judgment response to the runtime's verdict.
// ok=false means the response is unusable (missing should_interrupt answer)
// — the caller treats it exactly like a failed call: conservative enqueue.
//
// 判不动的时候倾向入队（§4.3）: a self-contradictory verdict (noul>threshold
// but motive=no_interrupt) also degrades to interrupt=false.
func routerDecisionFromJev(resp *jev.Response) (prompt.RouterDecision, bool) {
	fallback := prompt.RouterDecision{Interrupt: false, Severity: 0, Motive: prompt.RouterMotiveUrgent}
	if resp == nil {
		fallback.Reason = "jev_nil_response"
		return fallback, false
	}
	noulAns, ok := resp.Answers["should_interrupt"]
	if !ok || noulAns.Noul == nil {
		fallback.Reason = "jev_missing_answer: should_interrupt"
		return fallback, false
	}
	noul := *noulAns.Noul

	// motive 白名单：空/非法 → urgent；no_interrupt 不是合法打断动机，但
	// 保留原值供下面的自相矛盾检查（概率说打断、动机说不用 → 保守不打断）。
	rawMotive := resp.Answers["motive"].Choice
	motive := prompt.RouterMotiveUrgent
	switch rawMotive {
	case prompt.RouterMotiveUrgent, prompt.RouterMotiveSituationResolved, prompt.RouterMotiveSocial:
		motive = rawMotive
	}

	// severity：score 0-1 → 0-10 四舍五入；缺失 → 0（与旧 parse 的缺省一致）。
	severity := 0
	if scoreAns, ok := resp.Answers["severity"]; ok && scoreAns.Score != nil {
		severity = int(math.Round(*scoreAns.Score * 10))
		if severity < 0 {
			severity = 0
		}
		if severity > 10 {
			severity = 10
		}
	}

	interrupt := noul > routerInterruptNoulThreshold
	// 自相矛盾（概率说打断、动机说不用）→ 保守不打断。
	if interrupt && rawMotive == "no_interrupt" {
		interrupt = false
	}
	// 社交打断压低 severity：护栏阶梯比较 severity，社交回应不得凌驾真实紧急。
	if interrupt && motive == prompt.RouterMotiveSocial && severity > routerJudgeSeverityCapSocial {
		severity = routerJudgeSeverityCapSocial
	}

	dec := prompt.RouterDecision{Interrupt: interrupt, Severity: severity, Motive: motive}
	if interrupt {
		dec.Reason = fmt.Sprintf("打断概率%.2f，动机%s，紧急度%d/10", noul, motive, severity)
		if conf := resp.Answers["motive"].Confidence; conf != nil {
			dec.Reason = fmt.Sprintf("打断概率%.2f，动机%s（置信度%.2f），紧急度%d/10", noul, motive, *conf, severity)
		}
	} else {
		dec.Reason = fmt.Sprintf("不应打断（概率%.2f）", noul)
	}
	return dec, true
}

// eventRouter runs non-force world events through the judgment model.
type eventRouter struct {
	// lookupJudge resolves the agent's judgment client (per-agent, built in
	// registerAgent alongside strategicHc/tacticalHc). nil (no client —
	// --jev-model="" disables router verdicts) makes every event degrade
	// to enqueue.
	lookupJudge func(string) routerJudge
	kbPtr       **worldkb.KB
	profiles    map[string]*profile.Profile
	logger      *slog.Logger
	lookupAgent func(string) *agentContext
	// processVerdict applies an interrupt verdict. Injected so tests can
	// intercept; production wires it to Runtime.routerInterrupt.
	processVerdict func(agentID string, ev protocol.WorldEventPayload, dec prompt.RouterDecision)
}

// newEventRouter builds the router. lookupJudge returning nil (no judgment
// client for the agent) makes every event degrade to enqueue.
func newEventRouter(lookupJudge func(string) routerJudge, kbPtr **worldkb.KB,
	profiles map[string]*profile.Profile, lookupAgent func(string) *agentContext,
	processVerdict func(agentID string, ev protocol.WorldEventPayload, dec prompt.RouterDecision),
	logger *slog.Logger) *eventRouter {
	return &eventRouter{
		lookupJudge:    lookupJudge,
		kbPtr:          kbPtr,
		profiles:       profiles,
		logger:         logger,
		lookupAgent:    lookupAgent,
		processVerdict: processVerdict,
	}
}

// route runs the router for one event (called async from
// Runtime.handleWorldEvent). Timeout (client-side, --jev-timeout) / client-nil
// / unusable answers all degrade to enqueue — which already happened at
// receipt, so those paths simply log and return. Only an explicit interrupt
// acts.
func (r *eventRouter) route(ctx context.Context, agentID string, ev protocol.WorldEventPayload) {
	if r == nil {
		return
	}
	j := r.lookupJudge(agentID)
	if j == nil {
		return // 无判决客户端：enqueue-only（事件已在队列）
	}
	ac := r.lookupAgent(agentID)
	if ac == nil {
		return
	}

	in := r.buildInput(agentID, ac, ev)

	// 超时单源：jev.Config.Timeout（http.Client.Timeout，--jev-timeout）。
	// state 结构化：conversation = agentic loop 近期历史（上下文知识），
	// user = 该 NPC 的结构化属性（角色/关系/状态/动作/事件）。
	t0 := time.Now()
	resp, err := j.Judge(ctx, &jev.Request{
		State: jev.State{
			Conversation: routerConversation(ac.as),
			User:         prompt.BuildRouterUserAttributes(in),
		},
		Questions: routerQuestions,
	})
	// 判决请求体落盘 docs/actual_prompts.md（layer="router"，仿战略/战术/
	// 对话层；无论成败都记最新一次）。
	dumpLastRequestBody(agentID, "router", j, r.logger)
	e2e := time.Since(t0)

	// 指标埋点：E2E/错误分类/输出 token（非流式无 TTFT/TPOT）。
	outputTokens := 0
	if resp != nil {
		outputTokens = resp.Usage.OutputTokens
	}
	llmMetricsCollector.RecordCall(llmmetrics.CallSample{
		Layer:        "router",
		E2E:          e2e,
		OutputTokens: outputTokens,
		ErrClass:     classifyLLMError(err),
	})
	dumpLLMMetrics(r.logger)

	if err != nil {
		// 判决失败 → 保持入队（保守）。事件已在接收路径入队，无需补偿。
		r.logger.Info("[事件路由] 判决调用失败，按入队处理（保守）",
			"agent_id", agentID, "event_id", ev.EventID, "err", err)
		return
	}
	dec, ok := routerDecisionFromJev(resp)
	llmMetricsCollector.RecordJSON("router", ok)
	if !ok {
		r.logger.Info("[事件路由] 判决响应缺 should_interrupt，按入队处理（保守）",
			"agent_id", agentID, "event_id", ev.EventID)
		return
	}
	if !dec.Interrupt {
		r.logger.Info("[事件路由] 裁决不紧急，保持入队",
			"agent_id", agentID, "event_id", ev.EventID,
			"event_type", ev.EventType, "severity", dec.Severity, "reason", dec.Reason)
		return
	}

	// interrupt：事件需要从队列里撤下（不再等安全点），转打断处理。
	r.logger.Info("[事件路由] 裁决紧急，转打断",
		"agent_id", agentID, "event_id", ev.EventID,
		"event_type", ev.EventType, "severity", dec.Severity, "reason", dec.Reason)
	r.processVerdict(agentID, ev, dec)
}

// buildInput snapshots the agent state and composes the router's view.
func (r *eventRouter) buildInput(agentID string, ac *agentContext, ev protocol.WorldEventPayload) prompt.RouterInput {
	snap := ac.as.Snapshot()
	// 从同一 snapshot 推导权威游戏时间——避免 Snapshot 与 LatestGameTimeSec
	// 两次读取之间 perception 到达导致时间不一致（情境 TTL 过滤口径）。
	nowGameSec := 0.0
	if len(snap.LatestPerception) > 0 {
		var p protocol.PerceptionPayload
		if err := json.Unmarshal(snap.LatestPerception, &p); err == nil {
			nowGameSec = p.Environment.GameTimeSec
		}
	}

	agentName := ""
	agentRole := ""
	kb := *r.kbPtr
	if kb != nil {
		if a := kb.GetAgent(agentID); a != nil {
			agentName = a.DisplayName
		}
		agentRole = prompt.AgentRole(kb, r.profiles, agentID)
	}

	physicalLine := ""
	if snap.LatestPhysical != nil && !snap.LatestPhysical.IsZero() {
		physicalLine = prompt.PhysicalLineActual(*snap.LatestPhysical,
			prompt.BandThresholdsFor(r.profiles, agentID))
	}

	// 当前动作 + 已执行时长：紧急度判断的输入（刚开工 vs 快完成）。
	action := ""
	if snap.CurrentActionID != "" {
		action = describeAction(snap.CurrentActionCmd, snap.CurrentActionParams)
		if elapsed := time.Since(snap.CurrentActionStart); elapsed > 0 {
			action += fmt.Sprintf("，已执行约 %d 分钟", int(elapsed.Minutes()))
		}
		if snap.CurrentActionSrc != "" {
			action += "（来源：" + string(snap.CurrentActionSrc) + "）"
		}
	}
	// 反应窗口上下文：当前动作是对某紧急事件的反应时，路由 LLM 需要
	// 知道这一点——只看 MoveTo(residential_quarters) 的字面目标无法判断
	// 这是"逃跑"还是"正常日程"（2026-09-20 仿真：combat_exit 到达时路由
	// 判"未处于逃跑状态"，漏掉了 situation_resolved 打断）。
	if reactionDesc := ac.reactionDescSnapshot(); reactionDesc != "" {
		if action == "" {
			action = "（空闲，但正应对紧急事件：" + reactionDesc + "）"
		} else {
			action += "。注意：这是对紧急事件的反应动作（" + reactionDesc + "）"
		}
	}

	relationships := ""
	if store := ac.as.Store(); store != nil && kb != nil && len(kb.Agents) > 1 {
		if rels, err := store.LoadRelationships(context.Background(), agentID, 10); err == nil {
			relationships = formatRelationshipsForPrompt(rels, agentID)
		}
	}

	return prompt.RouterInput{
		AgentID:       agentID,
		AgentName:     agentName,
		AgentRole:     agentRole,
		TimeOfDay:     snap.LatestTimeOfDay(),
		Zone:          snap.LatestZone(),
		PhysicalLine:  physicalLine,
		Relationships: relationships,
		CurrentAction: action,
		Situations:    formatActiveSituations(snap.ActiveSituations, nowGameSec),
		WorldOverview: prompt.WorldOverview(kb),
		Event:         ev,
	}
}

// routerInterrupt applies an interrupt verdict. It mirrors the force path
// minus the hard guarantee: the event was NOT force (the router decided),
// so the stop is followed by the event-aware replan but the §4.2 channel
// semantics (synchronous, microsecond) do not apply. The event is drained
// from the pending queue (it is being handled NOW, not at the next safe
// point) and injected as the replan hint.
func (rt *Runtime) routerInterrupt(agentID string, ev protocol.WorldEventPayload, dec prompt.RouterDecision) {
	ac := rt.lookupAgent(agentID)
	if ac == nil {
		return
	}
	ac.coordMu.Lock()
	stopped := ac.stopped
	ac.coordMu.Unlock()
	if stopped {
		return
	}

	// 护栏②（§4.5）：反应打断反应需 severity 严格更高。被拦截的事件保持
	// 入队（已在队列里，等安全点与后续事件一并统筹——保守方向与 §4.3 一致）。
	// force 事件不走此护栏（§4.2 不可否决）。
	// situation_resolved 绕过 severity 阶梯——它在结束当前反应，不是在与
	// 当前反应竞争（如逃跑中收到 combat_exit：逃跑反应的 severity 再高，
	// 解除信号也必须放行，否则 NPC 会继续做已无必要的逃跑）。
	if active, curSev, _ := ac.reactionSnapshot(); active && dec.Severity <= curSev &&
		dec.Motive != prompt.RouterMotiveSituationResolved {
		rt.logger.Info("[事件路由] 反应护栏拦截：severity 不高于进行中的反应，保持入队",
			"agent_id", agentID, "event_id", ev.EventID,
			"event_type", ev.EventType, "event_severity", dec.Severity,
			"reaction_severity", curSev, "reason", dec.Reason)
		return
	}

	// 撤下队列中的这条事件（改走打断路径，不再等安全点）。找不到 = 已被
	// drain 或 Stop 清过——继续打断处理（保守：宁可多一次反应，不丢一次
	// 紧急响应）。
	ac.as.RemoveQueuedWorldEvent(ev.EventID)

	// 掐掉在途战术层 LLM 请求（与 force 同机制，§3.3）：紧急打断不应等
	// 在途思考跑完。
	if ac.cancelInFlightTacticalLLM() {
		rt.logger.Info("[事件路由] 已取消在途战术层 LLM 请求（紧急裁决）",
			"agent_id", agentID, "event_id", ev.EventID)
	}

	// 打断在途动作（若在执行）。排队中（SmartObject 队列）同样 stop。
	actionID := ac.as.CurrentActionID()
	if actionID != "" {
		if err := rt.ws.SendStopAction(agentID, actionID); err != nil {
			rt.logger.Warn("[事件路由] stop_action 发送失败（打断延后到 replan 完成后重试）",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			ac.recordInterrupted("被紧急事件打断（路由裁决：" + truncateRunes(dec.Reason, 40) + "）")
			ac.as.ClearInFlightKeepQueue()
			rt.logger.Warn("[事件路由] 已打断在途动作（路由裁决紧急）",
				"agent_id", agentID, "action_id", actionID, "event_id", ev.EventID,
				"severity", dec.Severity, "reason", dec.Reason)
		}
	}

	var hint string
	switch dec.Motive {
	case prompt.RouterMotiveSituationResolved:
		hint = fmt.Sprintf("【情境解除】%s。当前动作因此不再必要，请停止当前动作并按当前时段目标正常规划。",
			prompt.FormatWorldEvent(ev))
	case prompt.RouterMotiveSocial:
		hint = fmt.Sprintf("【社交回应】%s。请简短回应（如打招呼），然后继续当前时段的原有工作。",
			prompt.FormatWorldEvent(ev))
	default: // urgent 或空（向后兼容）
		hint = fmt.Sprintf("【强制打断】%s（路由裁决：紧急，%s）",
			prompt.FormatWorldEvent(ev), dec.Reason)
	}
	ac.as.SetReplanHint(hint)
	// 反应窗口按 motive 区分：
	//   urgent → 以裁决 severity 开启（后续打断需严格更高）
	//   situation_resolved → 清除现有窗口、不开启新窗口——它是结束反应，
	//     不是开始反应；跳过 beginReaction 使后续路由不受阶梯限制
	//   social → 低 severity 轻量窗口（任何更高级打断可覆盖，防连续社交
	//     ping-pong）
	switch dec.Motive {
	case prompt.RouterMotiveSituationResolved:
		ac.clearReaction()
	case prompt.RouterMotiveSocial:
		ac.beginReaction(2, ac.as.LatestGameTimeSec(), prompt.FormatWorldEvent(ev))
	default:
		ac.beginReaction(dec.Severity, ac.as.LatestGameTimeSec(), prompt.FormatWorldEvent(ev))
	}
	go ac.forceInterruptReplan(rt.ctx, agentID, rt.ws, *rt.kbPtr, rt.profiles, ev, hint, rt.logger)
}
