package main

// Lightweight event router runtime (事件驱动设计 §4.3, P2-5).
//
// Every non-force world event goes through one small LLM call that answers
// exactly one question: interrupt or enqueue? The verdict's interrupt branch
// reuses the force machinery minus the hard guarantee (stop with progress
// + replan with the event injected); the enqueue branch is the default and
// the failure fallback (判不动的时候倾向入队 — the cost asymmetry).
//
// Router calls go through the tactical flash client via Venus (not Ollama:
// cold-start timeouts were what paralyzed the old reactive layer). Unlike
// the tactical layer, router calls are stateless one-shots — they never
// touch the agent's conversation history, so a cancelled/failed verdict
// leaves no trace.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// routerCallTimeout bounds one router LLM call. A few seconds: the verdict
// feeds an interrupt decision, and holding the event in limbo longer than
// that is itself a wrong answer — timeout degrades to enqueue.
const routerCallTimeout = 8 * time.Second

// eventRouter runs non-force world events through the lightweight router.
type eventRouter struct {
	// lookupHC resolves the agent's tactical flash client (LLM clients are
	// per-agent in this codebase — registerAgent builds one venus.Client
	// per agentContext). The router borrows it for a stateless one-shot
	// call; it never touches the agent's conversation history.
	lookupHC    func(string) llmClient
	kbPtr       **worldkb.KB
	profiles    map[string]*profile.Profile
	logger      *slog.Logger
	lookupAgent func(string) *agentContext
	// processVerdict applies an interrupt verdict. Injected so tests can
	// intercept; production wires it to Runtime.routerInterrupt.
	processVerdict func(agentID string, ev protocol.WorldEventPayload, dec prompt.RouterDecision)
}

// newEventRouter builds the router. lookupHC returning nil (no LLM client
// for the agent) makes every event degrade to enqueue.
func newEventRouter(lookupHC func(string) llmClient, kbPtr **worldkb.KB,
	profiles map[string]*profile.Profile, lookupAgent func(string) *agentContext,
	processVerdict func(agentID string, ev protocol.WorldEventPayload, dec prompt.RouterDecision),
	logger *slog.Logger) *eventRouter {
	return &eventRouter{
		lookupHC:       lookupHC,
		kbPtr:          kbPtr,
		profiles:       profiles,
		logger:         logger,
		lookupAgent:    lookupAgent,
		processVerdict: processVerdict,
	}
}

// route runs the router for one event (called async from
// Runtime.handleWorldEvent). Timeout / client-nil / parse failure all
// degrade to enqueue — which already happened at receipt, so those paths
// simply log and return. Only an explicit interrupt=true acts.
func (r *eventRouter) route(ctx context.Context, agentID string, ev protocol.WorldEventPayload) {
	if r == nil {
		return
	}
	hc := r.lookupHC(agentID)
	if hc == nil {
		return // no LLM client: enqueue-only mode
	}
	ac := r.lookupAgent(agentID)
	if ac == nil {
		return
	}

	routerCtx, cancel := context.WithTimeout(ctx, routerCallTimeout)
	defer cancel()

	in := r.buildInput(agentID, ac, ev)
	system := prompt.BuildRouterSystem(in)
	user := prompt.BuildRouterPrompt(in)
	resp, err := hc.SendWithSummary(routerCtx, system, user)
	// 路由请求体落盘 docs/actual_prompts.md（layer="router"，仿战略/战术/
	// 对话层；无论成败都记最新一次）。
	dumpLastRequestBody(agentID, "router", hc, r.logger)
	if err != nil {
		// 路由失败 → 保持入队（保守）。事件已在接收路径入队，无需补偿。
		r.logger.Info("[事件路由] 裁决调用失败，按入队处理（保守）",
			"agent_id", agentID, "event_id", ev.EventID, "err", err)
		return
	}
	dec := prompt.ParseRouterDecision(resp.ExtractText())
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
