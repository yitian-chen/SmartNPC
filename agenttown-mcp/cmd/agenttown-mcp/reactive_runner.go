// Package main — reactive layer runner.
//
// reactiveRunner 把反应层纯函数（reactive.go）与外部副作用（Ollama 调用、
// WS stop_action/SendAction、agentContext 读写）串起来，是反应层的运行时。
//
// 设计要点（与 docs/AgentTown_Reactive_Layer.md 决策 2/3 一致）：
//   - 不阻塞战术层：trigger() 由 WS handler 通过 `go ...` 异步调用，
//     战术 worker 不等待 Ollama 返回。
//   - 串行化 Ollama 调用：进程级 mutex 避免多触发源并发打爆本地模型。
//   - 60s 去抖：相同 (agentID, trigger, detail) 在窗口内不重复触发。
//   - 静默降级：Ollama 不可达 / 解析失败 → 视为 continue，不打断战术层。
//
// 触发入口（WS message handler）：
//   - perception_update → observePerception 返回 (trigger, detail)
//   - state_report      → updateState 返回 (trigger, detail)
//   - event_notification → recordEventNotification 返回 (trigger, detail)
//   三处均在 trigger != "" 时 `go reactiveRunnerRef.trigger(...)`。

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// reactiveCallTimeout 是单次 Ollama 调用的硬超时（context deadline）。
//
// 实测 qwen2.5:7b-instruct-q4_K_M 在当前宿主上 eval ~7.5 tok/s，
// num_predict=80 最坏 ~10.5s eval + 冷启动 load ~3-5s ≈ 15s。
// 取 20s 覆盖冷启动 + 满额输出，常态热调用 2-5s 远低于此。
//
// 注：ollama.Client 的 HTTP Timeout 必须大于此值（main.go 中显式传入
// reactiveCallTimeout + 5s），否则 HTTP 超时会先于 ctx 触发，违背
// "ctx 是硬截止、HTTP Timeout 仅作 backstop" 的设计意图。
const reactiveCallTimeout = 20 * time.Second

// reactiveRunner 持有反应层运行所需的全部依赖。
// 通过 package-level `reactiveRunnerRef` 在 main() 中注入，WS handler 调用。
type reactiveRunner struct {
	ollama *ollama.Client
	ws     *wsserver.Server
	kb     *worldkb.KB
	logger *slog.Logger

	// mu 串行化 Ollama 调用。多触发源（perception/state/event）并发到达时
	// 排队执行，避免本地模型过载。持有期间包括 Ollama 调用 + 决策执行
	// （stop_action / SendAction），确保决策基于的状态不被并发触发覆盖。
	mu sync.Mutex
}

// newReactiveRunner 构造 reactiveRunner。client 为 nil 时返回 nil（反应层禁用）。
func newReactiveRunner(client *ollama.Client, ws *wsserver.Server, kb *worldkb.KB, logger *slog.Logger) *reactiveRunner {
	if client == nil {
		return nil
	}
	return &reactiveRunner{
		ollama: client,
		ws:     ws,
		kb:     kb,
		logger: logger,
	}
}

// trigger 是反应层的统一入口。WS handler 在显著变化时 `go r.trigger(...)` 调用。
//
// 流程（与文档 P0.2 runReactiveCheck 一致）：
//  1. 去抖：相同 (agentID, trigger, detail) 60s 内跳过
//  2. 串行化：r.mu.Lock()
//  3. 构造 prompt（从 agentContext 读当前状态）
//  4. 调 Ollama（3s 超时）
//  5. 解析决策（容错：失败默认 continue）
//  6. 执行：continue/observe 不动作；interrupt 发 stop_action；act 先 stop 再下发新 action
//
// 任何步骤失败都静默降级，不影响战术层正常运行。
func (r *reactiveRunner) trigger(agentID string, ac *agentContext, trigger ReactiveTrigger, detail string) {
	if r == nil || ac == nil || trigger == "" {
		return
	}

	// 1. 去抖检查（与文档决策 3 一致：事件类 60s，周期性 45s）
	key := dedupeKey(agentID, trigger, detail)
	dedupeWindow := reactiveDedupeWindow
	if trigger == TriggerPeriodic {
		dedupeWindow = reactivePeriodicDedupeWindow
	}
	now := time.Now()
	ac.mu.Lock()
	if ac.stopped {
		ac.mu.Unlock()
		return
	}
	lastAt, exists := ac.lastReactiveAt[key]
	if exists && now.Sub(lastAt) < dedupeWindow {
		ac.mu.Unlock()
		r.logger.Debug("[反应层] 去抖跳过",
			"agent_id", agentID, "trigger", trigger, "detail", detail)
		return
	}
	ac.lastReactiveAt[key] = now
	ac.mu.Unlock()

	// 2. 串行化 Ollama 调用
	r.mu.Lock()
	defer r.mu.Unlock()

	// 3. 构造 prompt 输入
	input := r.buildInput(agentID, ac, trigger, detail)
	prompt := buildReactivePrompt(input)
	r.logger.Info("[反应层/触发]",
		"agent_id", agentID, "trigger", trigger, "detail", detail,
		"zone", input.Zone, "time_of_day", input.TimeOfDay,
		"energy", input.Energy, "fatigue", input.Fatigue, "health", input.Health,
		"current_action", input.CurrentAction, "elapsed_sec", input.ElapsedSec,
		"action_src", input.ActionSrc, "current_slot", input.CurrentSlot)
	r.logger.Info("[反应层/PROMPT]",
		"agent_id", agentID, "trigger", trigger, "model", r.ollama.Model(),
		"text", prompt)

	// 4. 调用 Ollama（8s 超时）
	ctx, cancel := context.WithTimeout(context.Background(), reactiveCallTimeout)
	defer cancel()
	raw, err := r.ollama.Chat(ctx, prompt)
	if err != nil {
		r.logger.Warn("[反应层/失败]",
			"agent_id", agentID, "trigger", trigger, "err", err)
		return
	}
	r.logger.Info("[反应层/RESPONSE]",
		"agent_id", agentID, "trigger", trigger, "raw", raw)

	// 5. 解析决策
	dec := parseReactiveDecision(raw)
	r.logger.Info("[反应层/决策]",
		"agent_id", agentID, "reaction", dec.Reaction, "reason", dec.Reason,
		"trigger", trigger,
		"zone", input.Zone, "time_of_day", input.TimeOfDay,
		"energy", input.Energy, "fatigue", input.Fatigue, "health", input.Health,
		"current_action", input.CurrentAction, "elapsed_sec", input.ElapsedSec,
		"action_src", input.ActionSrc, "current_slot", input.CurrentSlot)

	// 6. 执行
	r.execute(agentID, ac, dec)
}

// buildInput 从 agentContext 提取反应层决策所需的状态快照。
// 持有 ac.mu 期间完成读取，避免与战术层并发写入竞争。
func (r *reactiveRunner) buildInput(agentID string, ac *agentContext, trigger ReactiveTrigger, detail string) ReactiveInput {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	zone := ac.latestZoneLocked()
	tod := ac.latestTimeOfDayLocked()
	// 物理状态默认值：energy/health 满格，fatigue 为 0（保守安全的"正常"状态）。
	// state_report 尚未到达时用默认值，避免反应层拿到 0 误判为警戒带触发。
	energy, fatigue, health := 100.0, 0.0, 100.0
	if ac.latestPhysical != nil {
		energy = ac.latestPhysical.Energy
		fatigue = ac.latestPhysical.Fatigue
		health = ac.latestPhysical.Health
	}
	// 构造可读的"在途动作"描述：cmd + 关键 params（target/duration_min）
	currentAction := ""
	elapsedSec := 0
	actionSrc := ""
	if ac.currentActionID != "" {
		currentAction = describeAction(ac.currentActionCmd, ac.currentActionParams)
		if !ac.currentActionStart.IsZero() {
			elapsedSec = int(time.Since(ac.currentActionStart).Seconds())
		}
		actionSrc = string(ac.currentActionSrc)
	}

	// 战术层上下文：截断 dailyPlan 避免 prompt 过长（反应层只需摘要）
	plan := ac.dailyPlan
	if len(plan) > 400 {
		plan = plan[:400] + "…"
	}

	// 从 KB 查 agent 显示名，供 prompt 中角色称呼使用（避免硬编码"老陈"）。
	agentName := ""
	if r.kb != nil {
		if agent := r.kb.GetAgent(agentID); agent != nil {
			agentName = agent.DisplayName
		}
	}

	return ReactiveInput{
		AgentID:       agentID,
		AgentName:     agentName,
		TimeOfDay:     tod,
		Zone:          zone,
		Energy:        energy,
		Fatigue:       fatigue,
		Health:        health,
		CurrentAction: currentAction,
		ElapsedSec:    elapsedSec,
		ActionSrc:     actionSrc,
		CurrentSlot:   ac.currentSlot,
		DailyPlan:     plan,
		Trigger:       trigger,
		TriggerDetail: detail,
	}
}

// describeAction 把 cmd + params 构造为人类可读的描述，供反应层 prompt 使用。
// 例如：MoveToLocation(target=workbench_01) / WorkAtWorkbench(target_object_id=workbench_01, duration_sec=3600)
func describeAction(cmd string, params map[string]any) string {
	if cmd == "" {
		return ""
	}
	if len(params) == 0 {
		return cmd
	}
	// 只提取关键参数：target / target_object_id / target_zone / target_agent_id /
	// duration_sec / dest / content / emotion / topic
	keys := []string{"target", "target_object_id", "target_zone", "target_agent_id", "dest", "duration_sec", "content", "emotion", "topic"}
	var parts []string
	for _, k := range keys {
		if v, ok := params[k]; ok && v != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return cmd
	}
	return cmd + "(" + joinStrings(parts, ", ") + ")"
}

// joinStrings 用 sep 连接 strings，避免引入 strings 包（reactive_runner.go 未 import）。
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}

// execute 执行反应层决策。interrupt/act 会发 stop_action 打断在途 action。
func (r *reactiveRunner) execute(agentID string, ac *agentContext, dec ReactiveDecision) {
	switch dec.Reaction {
	case ReactionContinue, ReactionObserve:
		// 不打断，仅日志（trigger 已记录决策）
		return

	case ReactionInterrupt:
		ac.mu.Lock()
		actionID := ac.currentActionID
		ac.mu.Unlock()
		if actionID == "" {
			r.logger.Debug("[反应层] interrupt 但无在途 action，跳过",
				"agent_id", agentID)
			return
		}
		if err := r.ws.SendStopAction(agentID, actionID); err != nil {
			r.logger.Warn("[反应层] stop_action 发送失败",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			r.logger.Info("[反应层] 已发 stop_action 打断在途 action",
				"agent_id", agentID, "action_id", actionID)
		}

	case ReactionAct:
		if dec.Action == nil {
			r.logger.Warn("[反应层] act 但 action 为空（不应到达，parseReactiveDecision 已降级）",
				"agent_id", agentID)
			return
		}
		// 先停当前在途 action（若有）
		ac.mu.Lock()
		actionID := ac.currentActionID
		ac.mu.Unlock()
		if actionID != "" {
			_ = r.ws.SendStopAction(agentID, actionID)
		}
		// 映射并下发新 action
		cmd, params, err := mapReactionAction(*dec.Action, r.kb)
		if err != nil {
			r.logger.Warn("[反应层] action 映射失败",
				"agent_id", agentID, "cmd", dec.Action.Cmd, "err", err)
			return
		}
		ack, err := r.ws.SendAction(context.Background(), agentID, cmd, params)
		if err != nil {
			r.logger.Warn("[反应层] action 下发失败",
				"agent_id", agentID, "cmd", cmd, "err", err)
			return
		}
		if ack != nil {
			// 反应层下发的 action 标记 sourceHermes（不进战术队列，completion
			// 不触发 pop），并通过 armActionTimeout 注册超时回收。
			ac.recordActionStarted(ack.ActionID, cmd, params, 0, sourceHermes)
			ac.armActionTimeout(ack.ActionID, ack.EstimatedDurationSec, r.ws, agentID,
				func(string) *agentContext { return ac })
			r.logger.Info("[反应层] act 已下发新 action",
				"agent_id", agentID, "cmd", cmd, "action_id", ack.ActionID)
		}

	case ReactionReplan:
		// replan：先规划后打断，防止角色无 action。
		// 1. 30 wall-clock 分钟去抖（agent 全局，不按 trigger/detail）
		ac.mu.Lock()
		if ac.stopped {
			ac.mu.Unlock()
			return
		}
		lastReplan := ac.lastReplanAt
		ac.mu.Unlock()
		if !lastReplan.IsZero() && time.Since(lastReplan) < replanDedupeWindow {
			r.logger.Info("[反应层] replan 去抖跳过（30 分钟内已 replan）",
				"agent_id", agentID, "last_replan_at", lastReplan, "window", replanDedupeWindow)
			return
		}

		// 2. 防重入：规划进行中跳过
		ac.mu.Lock()
		if ac.replanInProgress {
			ac.mu.Unlock()
			r.logger.Debug("[反应层] replan 已在进行，跳过", "agent_id", agentID)
			return
		}
		ac.replanInProgress = true
		ac.replanHint = dec.Reason
		ac.mu.Unlock()
		defer func() {
			ac.mu.Lock()
			ac.replanInProgress = false
			ac.mu.Unlock()
		}()

		// 3. 调战术层重规划（用 context.Background()，战术层需 30s，
		//    不复用反应层的 8s ctx）。规划期间 agent 继续原 action，
		//    worker 被 replanInProgress 守卫挡住不 pop 不 refill。
		//    规划失败 → 不打断原 action（损失最小）。
		r.logger.Info("[反应层] replan 开始，规划期间保持原 action",
			"agent_id", agentID, "replan_reason", dec.Reason)
		ok := ac.tacticalRefillForReplan(context.Background(), agentID, r.ws, r.kb, r.logger, dec.Reason)
		if !ok {
			r.logger.Warn("[反应层] replan 规划失败，保持原 action 不打断",
				"agent_id", agentID, "replan_reason", dec.Reason)
			return
		}

		// 4. 规划成功：更新去抖时间戳
		ac.mu.Lock()
		ac.lastReplanAt = time.Now()
		actionID := ac.currentActionID
		ac.mu.Unlock()

		// 5. 打断原在途 action（新队列已就绪，worker 待 signal）
		if actionID != "" {
			if err := r.ws.SendStopAction(agentID, actionID); err != nil {
				r.logger.Warn("[反应层] replan stop_action 发送失败（新队列已就绪，等 completion 自然推进）",
					"agent_id", agentID, "action_id", actionID, "err", err)
			} else {
				r.logger.Info("[反应层] replan 已发 stop_action 打断原 action",
					"agent_id", agentID, "action_id", actionID, "replan_reason", dec.Reason)
			}
		} else {
			r.logger.Info("[反应层] replan 无在途 action，worker 将 pop 新队列",
				"agent_id", agentID, "replan_reason", dec.Reason)
		}

		// 6. signal worker（tacticalRefillForReplan 内部已 signal，此处幂等再发一次）
		ac.signal()
	}
}

// mapReactionAction 把 ReactionAction 映射到 ws.SendAction 需要的 (cmd, params)。
// 反应层允许的 cmd 子集由 isValidReactionCmd 限制（move_to_location /
// move_to_agent / speak / emote / wait / interact），与战术层 mapTacticalAction
// 的对应分支共享映射逻辑。
func mapReactionAction(ra ReactionAction, kb *worldkb.KB) (string, map[string]any, error) {
	pa := plannedAction{Action: ra.Cmd, Params: ra.Params}
	cmd, params, err := mapTacticalAction(pa, kb)
	if err != nil {
		return "", nil, fmt.Errorf("map reaction action %q: %w", ra.Cmd, err)
	}
	return cmd, params, nil
}
