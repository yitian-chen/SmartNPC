package agentstate

import "github.com/AgentTown/agenttown-mcp/contract/protocol"

// Active situations（P3-9 后续修复 A，事件驱动设计 §3.4/§6.1）。
//
// 事件模型是边沿触发（一次性），但 combat 类事件描述的是**持续状态**：
// 被玩家攻击/瞄准后，威胁在收到 combat_exit（协议明确定义的解除信号）
// 之前一直存在。没有这层状态，反应耗尽后的日程 refill 看不到任何威胁
// 痕迹，LLM 只能脑补"威胁解除了"——缺失状态幻觉。
//
// 情境由 cmd 层按事件类别登记/解除（agentstate 只提供通用容器）：
//   - player_attacked / player_targeted → BeginSituation("combat", …)
//   - combat_exit                        → EndSituation("combat")
//   - TTL 兜底：超过 ttl 的情境在读取时过滤（UE 永远不发解除信号也不至于
//     永久卡死）。

// ActiveSituation is one ongoing world situation started by an event.
type ActiveSituation struct {
	Kind         string  // semantic kind, e.g. "combat"
	Desc         string  // pre-rendered event description (FormatWorldEvent)
	StartGameSec float64 // authoritative game seconds when it began
}

// BeginSituation registers (or keeps) an ongoing situation. Idempotent per
// kind: a repeat start while the situation is already active keeps the
// EARLIEST start (已持续的口径以首次为准).
func (a *AgentState) BeginSituation(kind, desc string, startGameSec float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.activeSituations {
		if a.activeSituations[i].Kind == kind {
			return // 已在持续中：保留最早起点
		}
	}
	a.activeSituations = append(a.activeSituations, ActiveSituation{
		Kind: kind, Desc: desc, StartGameSec: startGameSec,
	})
}

// EndSituation resolves a situation by kind; reports whether one was active.
func (a *AgentState) EndSituation(kind string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.activeSituations {
		if a.activeSituations[i].Kind == kind {
			a.activeSituations = append(a.activeSituations[:i], a.activeSituations[i+1:]...)
			return true
		}
	}
	return false
}

// ActiveSituations returns the live situations filtered by TTL
// (ttlGameSec <= 0 = no expiry). Expired ones are dropped from storage as a
// side effect.
func (a *AgentState) ActiveSituations(nowGameSec, ttlGameSec float64) []ActiveSituation {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := make([]ActiveSituation, 0, len(a.activeSituations))
	for _, s := range a.activeSituations {
		if ttlGameSec > 0 && nowGameSec > 0 && s.StartGameSec > 0 &&
			nowGameSec-s.StartGameSec > ttlGameSec {
			continue // TTL 兜底：无解除信号的情境到时自动降级
		}
		live = append(live, s)
	}
	a.activeSituations = live
	return append([]ActiveSituation(nil), live...)
}

// EchoWorldEvent appends an event to the pending queue WITHOUT the dedup
// path（P3-9 后续修复 B：force 事件的"回声"）。Used by the interrupt's
// success path so the FIRST schedule refill after the reaction sees the
// event once more in 【发生的事件】——被反应 replan 消费掉的 hint 不至于
// 成为上下文里最后的痕迹。Cap eviction mirrors EnqueueWorldEvent.
func (a *AgentState) EchoWorldEvent(ev protocol.WorldEventPayload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.worldEventQueue) >= worldEventQueueCap {
		a.worldEventQueue = a.worldEventQueue[1:]
	}
	a.worldEventQueue = append(a.worldEventQueue, ev)
}

// FilterSituations is the lock-free TTL filter for a snapshot copy（供
// cmd 层格式化用：Snapshot 取原始列表，TTL 按当前游戏时间在此过滤）。
func FilterSituations(situations []ActiveSituation, nowGameSec, ttlGameSec float64) []ActiveSituation {
	live := make([]ActiveSituation, 0, len(situations))
	for _, s := range situations {
		if ttlGameSec > 0 && nowGameSec > 0 && s.StartGameSec > 0 &&
			nowGameSec-s.StartGameSec > ttlGameSec {
			continue
		}
		live = append(live, s)
	}
	return live
}
