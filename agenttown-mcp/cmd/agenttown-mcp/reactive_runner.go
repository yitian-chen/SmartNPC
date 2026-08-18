// Package main — reactive layer runner.
//
// reactiveRunner 把反应层纯函数（reactive.go）与外部副作用（Ollama 调用、
// WS stop_action/SendAction、agentContext 读写）串起来，是反应层的运行时。
//
// 设计要点（与 docs/AgentTown_Reactive_Layer.md 决策 2/3 一致）：
//   - 不阻塞战术层：trigger() 由 WS handler 通过 `go ...` 异步调用，
//     战术 worker 不等待 Ollama 返回。
//   - 串行化 Ollama 调用：进程级 mutex 避免多触发源并发打爆本地模型。
//   - 60s wall-clock 去抖：相同 (agentID, trigger, detail) 在窗口内不重复触发。
//   - replan 按 1 游戏小时去抖（agent 全局，见 execute()）：避免 wall-clock
//     窗口在高倍率仿真下被放大到数十游戏小时，使整日仿真中合法 replan 被
//     第一次触发拦截。
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
	"strings"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
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
	ollama   *ollama.Client
	ws       *wsserver.Server
	kb       *worldkb.KB
	profiles map[string]*profile.Profile // NPC persona override（profile.md），nil=禁用
	logger   *slog.Logger

	// mu 串行化 Ollama 调用。多触发源（perception/state/event）并发到达时
	// 排队执行，避免本地模型过载。持有期间包括 Ollama 调用 + 决策执行
	// （stop_action），确保决策基于的状态不被并发触发覆盖。
	mu sync.Mutex
}

// newReactiveRunner 构造 reactiveRunner。client 为 nil 时返回 nil（反应层禁用）。
func newReactiveRunner(client *ollama.Client, ws *wsserver.Server, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) *reactiveRunner {
	if client == nil {
		return nil
	}
	return &reactiveRunner{
		ollama:   client,
		ws:       ws,
		kb:       kb,
		profiles: profiles,
		logger:   logger,
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
//  6. 执行：continue/observe 不动作；replan 调战术层重规划并 stop 在途 action
//
// 任何步骤失败都静默降级，不影响战术层正常运行。
func (r *reactiveRunner) trigger(agentID string, ac *agentContext, trigger ReactiveTrigger, detail string) {
	if r == nil || ac == nil || trigger == "" {
		return
	}

	// 1. 去抖检查（与文档决策 3 一致：事件类 60s，周期性 45s）
	key := prompt.DedupeKey(agentID, trigger, detail)
	dedupeWindow := reactiveDedupeWindow
	if trigger == TriggerPeriodic {
		dedupeWindow = reactivePeriodicDedupeWindow
	}
	now := time.Now()
	// stopped 是协调字段（coordMu），去抖时间戳是业务字段（AgentState.mu）。
	// 两次加锁不嵌套：先查 stopped，再原子 check-and-set 去抖时间戳。
	ac.coordMu.Lock()
	if ac.stopped {
		ac.coordMu.Unlock()
		return
	}
	ac.coordMu.Unlock()
	if !ac.as.DedupeReactive(key, now, dedupeWindow) {
		r.logger.Debug("[反应层] 去抖跳过",
			"agent_id", agentID, "trigger", trigger, "detail", detail)
		return
	}

	// 2. 串行化 Ollama 调用
	r.mu.Lock()
	defer r.mu.Unlock()

	// 拿锁后二次检查 stopped：排队期间 agent 可能已被 WS 断连回调 stop()。
	// 不检查则排队 goroutine 继续调 Ollama，是 UE 被 kill 后反应层仍出日志
	// 的直接根因（全局 r.mu 排队 + stopped 检查在锁外）。
	ac.coordMu.Lock()
	if ac.stopped {
		ac.coordMu.Unlock()
		r.logger.Debug("[反应层] 拿锁后 agent 已停止，跳过",
			"agent_id", agentID, "trigger", trigger, "detail", detail)
		return
	}
	ac.coordMu.Unlock()

	// 3. 构造 prompt 输入
	input := r.buildInput(agentID, ac, trigger, detail)
	promptText := prompt.BuildReactive(input)
	r.logger.Info("[反应层/触发]",
		"agent_id", agentID, "trigger", trigger, "detail", detail,
		"zone", input.Zone, "time_of_day", input.TimeOfDay,
		"energy", input.Energy, "fatigue", input.Fatigue, "joint_wear", input.JointWear, "money", input.Money,
		"current_action", input.CurrentAction, "elapsed_sec", input.ElapsedSec,
		"action_src", input.ActionSrc, "current_slot", input.CurrentSlot)
	r.logger.Info("[反应层/PROMPT]",
		"agent_id", agentID, "trigger", trigger, "model", r.ollama.Model(),
		"text", promptText)

	// 4. 调用 Ollama（8s 超时）
	ctx, cancel := context.WithTimeout(context.Background(), reactiveCallTimeout)
	defer cancel()
	raw, err := r.ollama.Chat(ctx, prompt.ReactiveSystemPrompt, promptText)
	if err != nil {
		r.logger.Warn("[反应层/失败]",
			"agent_id", agentID, "trigger", trigger, "err", err)
		return
	}
	r.logger.Info("[反应层/RESPONSE]",
		"agent_id", agentID, "trigger", trigger, "raw", raw)

	// 5. 解析决策（容错：失败默认 continue）
	dec := prompt.ParseReactiveDecision(raw)
	// 代码层兜底：物理状态告警时强制升级 continue/observe 为 interrupt。
	// LLM 在 fatigue=80+ 时仍可能输出 observe，仅靠 prompt 约束不可靠。
	dec = prompt.UpgradeIfPhysicalAlert(input, dec)
	r.logger.Info("[反应层/决策]",
		"agent_id", agentID, "reaction", dec.Reaction, "reason", dec.Reason,
		"trigger", trigger,
		"zone", input.Zone, "time_of_day", input.TimeOfDay,
		"energy", input.Energy, "fatigue", input.Fatigue, "joint_wear", input.JointWear, "money", input.Money,
		"current_action", input.CurrentAction, "elapsed_sec", input.ElapsedSec,
		"action_src", input.ActionSrc, "current_slot", input.CurrentSlot)

	// 6. 执行
	r.execute(agentID, ac, dec)
}

// buildInput 从 agentContext 提取反应层决策所需的状态快照。
// 通过 AgentState.Snapshot() 原子读取所有业务字段，无需持有协调锁，
// 避免与战术层并发写入竞争。
func (r *reactiveRunner) buildInput(agentID string, ac *agentContext, trigger ReactiveTrigger, detail string) ReactiveInput {
	snap := ac.as.Snapshot()

	zone := snap.LatestZone()
	tod := snap.LatestTimeOfDay()
	// 物理状态默认值：energy 满格，fatigue/joint_wear 为 0（保守安全的"正常"状态）。
	// perception_update 尚未到达时用默认值，避免反应层拿到 0 误判为警戒带触发。
	// money 默认 200（与 mock_ue 初始余额一致），余额为 0 是合法值不参与告警。
	energy, fatigue, jointWear, money := 100.0, 0.0, 0.0, 200.0
	if snap.LatestPhysical != nil {
		energy = snap.LatestPhysical.Energy
		fatigue = snap.LatestPhysical.Fatigue
		jointWear = snap.LatestPhysical.JointWear
		money = snap.LatestPhysical.Money
	}
	// 构造可读的"在途动作"描述：cmd + 关键 params（target/duration_min）
	currentAction := ""
	elapsedSec := 0
	actionSrc := ""
	if snap.CurrentActionID != "" {
		currentAction = describeAction(snap.CurrentActionCmd, snap.CurrentActionParams)
		if !snap.CurrentActionStart.IsZero() {
			elapsedSec = int(time.Since(snap.CurrentActionStart).Seconds())
		}
		actionSrc = string(snap.CurrentActionSrc)
	}

	// 排队状态（约定21）：snap.QueuedActionID 非空表示正在排队等待目标
	// Smart Object 释放。构造可读描述注入反应层 prompt，让 Ollama 决策时
	// 知道"在途动作其实在排队"——长时间排队时可考虑 replan 换目标。
	queuedFor := ""
	if snap.QueuedActionID != "" {
		pos := -1
		if snap.QueuedPosition != nil {
			pos = *snap.QueuedPosition
		}
		wait := 0.0
		if snap.QueuedEstimatedWait != nil {
			wait = *snap.QueuedEstimatedWait
		}
		queuedFor = fmt.Sprintf("正在排队等待 %s（位置 %d，预计等待 %.0f 秒）",
			snap.QueuedGroup, pos, wait)
	}

	// 战术层上下文：截断 dailyPlan 避免 prompt 过长（反应层只需摘要）
	plan := snap.DailyPlan
	if len(plan) > 400 {
		plan = plan[:400] + "…"
	}

	// 从 KB 查 agent 显示名 + 从 profile 取完整角色段，供 prompt 中角色称呼与性格注入使用。
	// agentName 用于 prompt 开头的 "你是 NPC %s" 称呼；agentRole 用于【你的角色】
	// 段（由 AgentRole 生成，含名字/职业/背景/性格/说话风格），
	// 让反应层决策也参考角色性格（如"沉稳"→偏向 continue，"急躁"→偏向 replan）。
	// persona 仅以 profile 为准（KB 性格字段被忽略），kb==nil 或 agent 不存在时
	// AgentRole 降级到 hardcoded fallback（H-01 硬编码兜底），agentName 降级为 agentID。
	agentName := ""
	if r.kb != nil {
		if agent := r.kb.GetAgent(agentID); agent != nil {
			agentName = agent.DisplayName
		}
	}
	agentRole := prompt.AgentRole(r.kb, r.profiles, agentID)

	// 实时从 dailyPlan 计算 slot，避免长动作在途时 currentSlot stale。
	// __debug__ 前缀的 slot 是 /debug/schedule 注入的临时覆盖，保留原值。
	liveSlot := snap.CurrentSlot
	if !strings.HasPrefix(snap.CurrentSlot, "__debug__") {
		if _, s, _ := selectCurrentGoal(snap.DailyPlan, tod); s != "" {
			liveSlot = s
		}
	}

	return ReactiveInput{
		AgentID:           agentID,
		AgentName:         agentName,
		AgentRole:         agentRole,
		TimeOfDay:         tod,
		Zone:              zone,
		Energy:            energy,
		Fatigue:           fatigue,
		JointWear:         jointWear,
		Money:             money,
		PhysicalAvailable: snap.LatestPhysical != nil && !snap.LatestPhysical.IsZero(),
		CurrentAction:     currentAction,
		ElapsedSec:        elapsedSec,
		ActionSrc:         actionSrc,
		QueuedFor:         queuedFor,
		CurrentSlot:       liveSlot,
		DailyPlan:         plan,
		Trigger:           trigger,
		TriggerDetail:     detail,
	}
}

// describeAction 把 cmd + params 构造为人类可读的描述，供反应层 prompt 使用。
// 例如：MoveTo(target_type=zone, target_id=workshop) / WorkShift(semantic_group=workbench, interaction=assemble)
func describeAction(cmd string, params map[string]any) string {
	if cmd == "" {
		return ""
	}
	if len(params) == 0 {
		return cmd
	}
	// 只提取关键参数：target_type / target_id / target_position / semantic_group /
	// interaction / content / emotion / thought / behavior
	keys := []string{"target_type", "target_id", "target_position", "semantic_group", "interaction", "content", "emotion", "thought", "behavior"}
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

// execute 执行反应层决策。replan 会调战术层重规划并打断在途 action。
func (r *reactiveRunner) execute(agentID string, ac *agentContext, dec ReactiveDecision) {
	switch dec.Reaction {
	case ReactionContinue, ReactionObserve:
		// 不打断，仅日志（trigger 已记录决策）
		return

	case ReactionReplan:
		// replan：先规划后打断，防止角色无 action。
		// 1. 游戏时间去抖（agent 全局，不按 trigger/detail）：1 游戏小时内至多 1 次。
		//    用游戏时间而非 wall-clock 是因为仿真倍率最高 600x，wall-clock 窗口会
		//    在游戏时间轴上放大到数十小时，使整日仿真中合法 replan 被第一次拦截。
		ac.coordMu.Lock()
		if ac.stopped {
			ac.coordMu.Unlock()
			return
		}
		ac.coordMu.Unlock()
		{
			snap := ac.as.Snapshot()
			lastReplanGT := snap.LastReplanGameTime
			curGT := snap.LatestTimeOfDay()
			if lastReplanGT != "" && curGT != "" {
				delta := prompt.GameTimeDeltaMinutes(lastReplanGT, curGT)
				if delta < replanDedupeGameMinutes {
					r.logger.Info("[反应层] replan 去抖跳过（1 游戏小时内已 replan）",
						"agent_id", agentID,
						"last_replan_game_time", lastReplanGT,
						"current_game_time", curGT,
						"delta_min", delta,
						"window_min", replanDedupeGameMinutes)
					return
				}
			}
		}

		// 2. 防重入：规划进行中跳过（协调字段，coordMu）
		ac.coordMu.Lock()
		if ac.replanInProgress {
			ac.coordMu.Unlock()
			r.logger.Debug("[反应层] replan 已在进行，跳过", "agent_id", agentID)
			return
		}
		ac.replanInProgress = true
		ac.coordMu.Unlock()
		ac.as.SetReplanHint(dec.Reason)
		defer func() {
			ac.coordMu.Lock()
			ac.replanInProgress = false
			ac.coordMu.Unlock()
		}()

		// 3. 调战术层重规划（用 context.Background()，战术层需 30s，
		//    不复用反应层的 8s ctx）。规划期间 agent 继续原 action，
		//    worker 被 replanInProgress 守卫挡住不 pop 不 refill。
		r.logger.Info("[反应层] replan 开始，规划期间保持原 action",
			"agent_id", agentID, "replan_reason", dec.Reason)
		ok := ac.tacticalRefillForReplan(context.Background(), agentID, r.ws, r.kb, r.profiles, r.logger, dec.Reason)
		if !ok {
			// 规划失败：仍需打断坏 action，否则 agent 继续执行触发 replan 的
			// 不合理 action（如疲劳仍工作），且旧队列也可能已过期。清空队列 +
			// 清 currentActionID + stop_action，让 worker 醒来后通过自然
			// tacticalRefill 路径基于当前状态重新规划（replanHint 已注入）。
			// 提前清除 replanInProgress 让 worker 能直接 refill，不阻塞。
			r.logger.Warn("[反应层] replan 规划失败，打断原 action 让 worker 自然 refill",
				"agent_id", agentID, "replan_reason", dec.Reason)
			r.fallbackStopAndRefill(agentID, ac, dec.Reason)
			return
		}

		// 4. 规划成功：更新去抖游戏时间戳（同时记录 wall-clock 仅用于日志）
		ac.as.SetReplanTimestamps(time.Now(), ac.as.LatestTimeOfDay())
		actionID := ac.as.CurrentActionID()

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

// fallbackStopAndRefill 是 replan 规划失败后的兜底：清空旧队列 + 清在途追踪 +
// 发 stop_action + signal worker。worker 醒来后通过自然 tacticalRefill 路径
// 基于当前状态重新规划（replanHint 已注入战术层 prompt）。
//
// 关键点：提前清除 replanInProgress + currentActionID，让 worker 的
// hasInFlightAction()/replanInProgress 守卫都不阻止 refill，避免
// "stop_action 后等 action_completed 才能 refill" 的 75 游戏分钟延迟。
// 旧 action 的超时 timer 必须取消，防止 fire 后误清新 currentActionID。
// 旧 action 的 action_completed 迟到时 currentActionID 已不匹配，自然忽略。
func (r *reactiveRunner) fallbackStopAndRefill(agentID string, ac *agentContext, reason string) {
	// 业务字段（queue + 在途追踪 + slot）通过 AgentState 原子清理；
	// 协调字段（replanInProgress + pending timer）通过 coordMu 清理。
	// 两次加锁不嵌套。
	info := ac.as.ClearForReplan()
	actionID := info.ActionID
	queueLen := info.QueueLen
	ac.as.SetReplanHint(reason)
	ac.as.SetReplanTimestamps(time.Now(), ac.as.LatestTimeOfDay()) // 游戏时间去抖，防止 1 游戏小时内反复 replan 失败

	ac.coordMu.Lock()
	ac.replanInProgress = false
	if actionID != "" {
		if timer, ok := ac.pendingActionTimeouts[actionID]; ok {
			timer.Stop()
			delete(ac.pendingActionTimeouts, actionID)
		}
	}
	ac.coordMu.Unlock()

	if actionID != "" {
		if err := r.ws.SendStopAction(agentID, actionID); err != nil {
			r.logger.Warn("[反应层] replan 失败后 stop_action 发送失败",
				"agent_id", agentID, "action_id", actionID, "err", err)
		} else {
			r.logger.Info("[反应层] replan 失败，已 stop 原 action，worker 将自然 refill",
				"agent_id", agentID, "action_id", actionID,
				"queue_len", queueLen, "replan_reason", reason)
		}
	} else {
		r.logger.Info("[反应层] replan 失败，无在途 action，worker 将自然 refill",
			"agent_id", agentID, "queue_len", queueLen, "replan_reason", reason)
	}
	ac.signal()
}
