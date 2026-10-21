package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// relationshipJudgeSystemPrompt is the relationship judge's system message:
// mechanism text only (task + yes/no output convention).
//
// The prompt is deliberately short (~80 tokens) because Ollama runs locally
// on CPU and the decision is a simple yes/no classification. The 5s caller
// timeout is tighter than the reactive layer's 20s — relationship updates
// are best-effort and must not block the decision pipeline.
const relationshipJudgeSystemPrompt = `你是关系判断模块。用户信息给出一个刚完成的动作及其目标 agent，判断是否构成与该 agent 的直接社交互动。仅输出 yes 或 no。yes=面向该 agent 的直接互动(聊天/维修/对话/转身面向); no=仅路过定位或 target 实为物体/区域。`

// relationshipJudgeUserTemplate is the relationship judge's user message
// template. Placeholders: %s = cmd, %s = params JSON, %s = target agent id.
const relationshipJudgeUserTemplate = `动作 cmd: %s
动作 params: %s
目标 agent: %s`

// shouldUpdateRelationship asks Ollama whether the completed action warrants
// a relationship bump with the target agent. Returns false on any error,
// timeout, or unparseable response (conservative — better to skip an update
// than to fire spurious ones). The caller (maybeUpdateRelationship) supplies
// a 5s context; this function does not create its own.
func shouldUpdateRelationship(ctx context.Context, c *ollama.Client, cmd string, params map[string]any, target string) bool {
	if c == nil {
		return false
	}
	paramsJSON, _ := json.Marshal(params)
	promptText := fmt.Sprintf(relationshipJudgeUserTemplate, cmd, string(paramsJSON), target)
	raw, err := c.Chat(ctx, relationshipJudgeSystemPrompt, promptText)
	if err != nil {
		slog.Default().Warn("[关系层] Ollama 判断调用失败，跳过更新",
			"target", target, "cmd", cmd, "err", err)
		return false
	}
	return parseRelationshipJudgeResponse(raw)
}

// parseRelationshipJudgeResponse extracts a yes/no verdict from the Ollama
// raw output. Accepts "yes", "yes.", "yes\n" etc. (case-insensitive, prefix
// match). Rejects everything else (including "no", empty, or garbage) — the
// conservative default is "do not update".
func parseRelationshipJudgeResponse(raw string) bool {
	answer := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(answer, "yes")
}

// defaultAffection is the affection shown for peers with no stored
// relationship row (never interacted / memory mode). Matches migration
// 0003's column default and seedRelationshipsFromKB's seed value.
const defaultAffection = 5

// formatRelationshipsForPrompt renders the merged other-NPC roster +
// affection list, injected as 战略层【其他NPC】/ 战术层【人际关系】/ 路由层
// relationships 属性（同一段内容，三种头部）。
//
// The KB's peers (excluding self) are the skeleton: EVERY peer gets a line —
// its stored relationship row when one exists, else the affection default
// (defaultAffection) — so the roster and affection stay visible from cold
// start (no interactions yet) and in memory mode (no store rows at all);
// before this, an empty table silently hid both from every layer.
//
// Each line reads "- 老王（H-02，物流分拣兼加工员）：好感 5/10（熟悉度 3、
// 互动 2 次）"; the 熟悉度/互动 paren is appended only when the stored row
// actually has interaction history. Display name / profession resolve
// profile > KB > bare id (the AgentRole fallback order). Rows whose peer is
// not in the KB are dropped (stale data must not resurrect deleted agents).
//
// Dedup: LoadRelationships queries both directions (agent_a=? OR agent_b=?),
// so an agent who has interacted with H-03 gets two rows that both read as
// "与 H-03" — that duplication confused the LLM and wasted tokens
// (2026-09-18 仿真实测). When both directions exist, the row with the
// higher familiarity / interaction count wins.
//
// Returns "" when kb == nil or the KB has ≤1 agent (single-NPC scenario
// has no peers to list).
func formatRelationshipsForPrompt(rels []storage.Relationship, kb *worldkb.KB, profiles map[string]*profile.Profile, agentID string) string {
	if kb == nil || len(kb.Agents) <= 1 {
		return ""
	}
	// 存储行按对方聚合（双向取更高值）。
	best := make(map[string]storage.Relationship, len(rels))
	for _, r := range rels {
		other := r.AgentB
		if r.AgentA != agentID {
			other = r.AgentA
		}
		if other == "" || other == agentID {
			continue
		}
		cur, ok := best[other]
		if !ok || r.Familiarity > cur.Familiarity || r.InteractionCount > cur.InteractionCount {
			best[other] = r
		}
	}
	var sb strings.Builder
	for _, ag := range kb.Agents {
		if ag.ID == agentID || ag.ID == "" {
			continue
		}
		// 显示名 / 职业：profile > KB（与 AgentRole 同一回退序）。profile 的
		// ## 职业 常带动作映射括注（"物流分拣兼加工员（分拣传送带分拣
		// sort_cargo + …）"）——那是人设 prompt 的写法，名单行只要自然职业，
		// 截掉首个全/半角括号起的尾注。
		name, prof := ag.DisplayName, ag.Profession
		if p := profiles[ag.ID]; p != nil {
			if p.DisplayName != "" {
				name = p.DisplayName
			}
			if p.Profession != "" {
				prof = p.Profession
			}
		}
		if i := strings.IndexAny(prof, "（("); i >= 0 {
			prof = strings.TrimSpace(prof[:i])
		}
		var label string
		switch {
		case name != "" && name != ag.ID && prof != "":
			label = fmt.Sprintf("%s（%s，%s）", name, ag.ID, prof)
		case name != "" && name != ag.ID:
			label = fmt.Sprintf("%s（%s）", name, ag.ID)
		case prof != "":
			label = fmt.Sprintf("%s（%s）", ag.ID, prof)
		default:
			label = ag.ID
		}
		// 好感度：存储行优先；无行（未互动过 / 内存模式）默认 5。
		aff := defaultAffection
		history := ""
		if r, ok := best[ag.ID]; ok {
			aff = r.Affection
			if r.Familiarity > 0 || r.InteractionCount > 0 {
				history = fmt.Sprintf("（熟悉度 %d、互动 %d 次）", r.Familiarity, r.InteractionCount)
			}
		}
		fmt.Fprintf(&sb, "- %s：好感 %d/10%s\n", label, aff, history)
	}
	if sb.Len() == 0 {
		return ""
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// seedRelationshipsFromKB imports KB-declared relationships into the DB at
// cold start. For each KB relationship involving agentID (either side), it
// inserts a directional row with agentID as agent_a (so both A→B and B→A
// end up in the table once each agent has registered). Uses INSERT IGNORE
// so existing rows (from a prior run or from live interactions) are never
// overwritten. Best-effort: logs warn on per-row error and continues.
//
// Called from the agent_registered handler in main.go, after LoadPersistent
// and before worker start. Reconnects skip this (registerAgent returns early
// on existing agent), so seed runs only on cold start.
func seedRelationshipsFromKB(ctx context.Context, kb *worldkb.KB, store storage.Store, agentID string, logger *slog.Logger) error {
	if kb == nil || store == nil {
		return nil
	}
	if len(kb.Relationships) == 0 {
		return nil
	}
	// seen tracks the "other" agent already seeded in this call. KB may
	// declare both directions of a bidirectional pair (e.g. H-01→H-02 and
	// H-02→H-01), in which case registering H-01 would match both rows and
	// seed H-01→H-02 twice. Dedupe in-memory to avoid redundant INSERT
	// IGNORE calls (DB would dedupe anyway, but skip the extra round-trip).
	seen := make(map[string]bool)
	seeded := 0
	for _, rel := range kb.Relationships {
		// Only seed rows where agentID is involved. Import as agentID→other
		// so each agent owns its outgoing row; the other agent's registration
		// will seed the reverse direction.
		var other string
		var fam, aff int
		switch {
		case rel.From == agentID:
			other = rel.To
			fam, aff = rel.Familiarity, rel.Affection
		case rel.To == agentID:
			other = rel.From
			fam, aff = rel.Familiarity, rel.Affection
		default:
			continue // relationship doesn't involve this agent
		}
		if other == "" {
			continue // self-relationship guard, shouldn't happen (KB validates)
		}
		// 好感度默认 5：KB 未声明（affection=0）或越界时回落默认。
		if aff <= 0 || aff > 10 {
			aff = 5
		}
		if seen[other] {
			continue // already seeded agentID→other in this call (KB declared both directions)
		}
		seen[other] = true
		if err := store.SeedRelationship(ctx, agentID, other, fam, aff); err != nil {
			logger.Warn("[关系层] KB 种子导入单条失败（继续其余）",
				"agent_id", agentID, "other", other, "err", err)
			continue
		}
		seeded++
	}
	if seeded > 0 {
		logger.Info("[关系层] KB 种子关系导入完成",
			"agent_id", agentID, "seeded", seeded)
	}
	return nil
}

// relationshipJudgeTimeout is the hard deadline for a single Ollama call in
// shouldUpdateRelationship. Shorter than the reactive layer's 20s because the
// judgment is a trivial yes/no and the update is best-effort.
const relationshipJudgeTimeout = 5 * time.Second

// ─── 好感度（affection）会话与评估 ──────────────────────────────

// conversationTurn is one utterance in an in-progress NPC↔NPC conversation.
type conversationTurn struct {
	speakerID string
	content   string
}

// conversationIdleTimeoutGameSec bounds how long a conversation may sit idle
// (no turn) before the worker closes it and triggers the affection evaluation.
// 20 游戏分钟：必须覆盖一轮完整回话往返（UE 的 no_reply 窗 ~10 现实秒 ≈
// 15 游戏分钟 @time_scale=90 + 对方回应 LLM 1-6s）。旧值 5 游戏分钟在
// 90 倍速下只有 3.3 现实秒——比 venus 回应往返还短，会话在对方回话到达
// 前就被关掉（2026-10-16 实测：H-01 的会话在应答到达前 0.12s 关闭，
// turns 重置导致确定性打断失效、应答被判不紧急留队、邀请方永不确认）。
const conversationIdleTimeoutGameSec = 20 * 60.0

// recordConversationTurn appends a turn to the agent's in-progress conversation
// session keyed by peerID. A peer change clears the old session (an abandoned
// conversation is discarded, not evaluated)——unless the same peer's session was
// closed just now (within conversationCarryoverWindow), in which case the closed
// session's turns carry over first: the late-arriving reply lands in a session
// that still holds the full transcript instead of a context-free orphan line.
// nowGameSec is the authoritative game time of the turn.
func (a *agentContext) recordConversationTurn(speakerID, peerID, content string, nowGameSec float64) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if a.convPeerID != peerID {
		a.convPeerID = peerID
		a.convTurns = nil
	}
	// 会话续接（2026-10-20）：新会话起始（无轮次）且同 peer 的会话刚在宽限
	// 窗内关闭时，旧轮次先行续接。实测 16:27 场景：邀请 no_reply 终局与
	// 应答送达同秒，会话重开只剩应答 1 轮——社交 replan 看到孤立发言、
	// 守约 hint 只有邀请半边，约定被劈成两个碎片，邀请方回了车间。
	if len(a.convTurns) == 0 && a.lastClosedPeer == peerID && len(a.lastClosedTurns) > 0 &&
		time.Since(a.lastClosedAt) <= conversationCarryoverWindow {
		a.convTurns = append(a.convTurns, a.lastClosedTurns...)
	}
	a.convTurns = append(a.convTurns, conversationTurn{speakerID: speakerID, content: content})
	a.convLastGameSec = nowGameSec
}

// conversationCarryoverWindow 是会话续接的现实时间宽限窗：同一 peer 的会话
// 关闭后，peer 的新发言在窗内到达则旧轮次续接进新会话。覆盖应答方回话
// 往返（收邀请 ~2s + 判决/LLM ~5s + 派发送达 ~1s ≈ 8s，与邀请方 no_reply 窗
// 几乎等长）加一次 LLM 延迟的余量。包级变量便于测试临时调小/关掉。
var conversationCarryoverWindow = 60 * time.Second

// closeConversationIfIdle closes the in-progress conversation session once it
// has idled past conversationIdleTimeoutGameSec, returning the captured peer +
// turns (empty peer = no session or not yet idle). The caller spawns the
// affection evaluation in the background.
func (a *agentContext) closeConversationIfIdle(nowGameSec float64) (string, []conversationTurn) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if a.convPeerID == "" || a.convLastGameSec <= 0 {
		return "", nil
	}
	if nowGameSec-a.convLastGameSec < conversationIdleTimeoutGameSec {
		return "", nil
	}
	return a.closeConversationLocked()
}

// closeConversationByPeer closes the in-progress session immediately — but
// only when its peer matches the given target (the no_reply speak_to's
// addressee). Used by the "对话终局即时判定" path in recordActionCompletion:
// a speak_to completing with no_reply means the peer never spoke back within
// the wait window, which is a terminal state from this agent's perspective —
// no need to wait out conversationIdleTimeoutGameSec. Mismatched peer (stale
// session from another conversation) returns "" and leaves the session intact.
func (a *agentContext) closeConversationByPeer(target string) (string, []conversationTurn) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if a.convPeerID == "" || a.convPeerID != target {
		return "", nil
	}
	return a.closeConversationLocked()
}

// closeConversationLocked clears the in-progress session and returns what it
// held, stashing it as the carryover snapshot — a same-peer utterance within
// conversationCarryoverWindow re-opens from these turns. Caller holds coordMu.
func (a *agentContext) closeConversationLocked() (string, []conversationTurn) {
	peer := a.convPeerID
	turns := a.convTurns
	if peer != "" && len(turns) > 0 {
		a.lastClosedPeer = peer
		a.lastClosedTurns = turns
		a.lastClosedAt = time.Now()
	}
	a.convPeerID = ""
	a.convTurns = nil
	a.convLastGameSec = 0
	return peer, turns
}

// clearConversation discards the in-progress conversation session (combat-detach
// entry). An interrupted conversation is not evaluated — but it still lands in
// the carryover snapshot: a peer reply arriving shortly after combat resumes
// the pre-combat transcript rather than starting context-free.
func (a *agentContext) clearConversation() {
	a.coordMu.Lock()
	a.closeConversationLocked()
	a.coordMu.Unlock()
}

// conversationAffectionSystemPrompt is the affection evaluation's system text.
const conversationAffectionSystemPrompt = `你是 NPC 好感度评估模块。用户信息给出该 NPC 的人设、与对方当前的好感度、以及刚结束的一段对话记录。请判断这段对话对该 NPC（对对方）好感度的影响，输出一个整数 delta：正数=更亲近、负数=更疏远、0=无变化，范围 -3 到 +3。只输出 JSON。`

// conversationAffectionSchema is the structured-output schema for the affection
// evaluation: a single integer delta in [-3, +3].
const conversationAffectionSchema = `{"type":"object","properties":{"delta":{"type":"integer","description":"好感度变化量，范围 -3 到 +3"}},"required":["delta"],"additionalProperties":false}`

// evaluateConversationAffection runs the post-conversation affection evaluation
// in the background (best-effort): the agent's persona, the current affection
// toward peer, and the full transcript go to the strategic (pro) model, which
// returns a delta; the delta is clamped to [-3,+3] and the resulting affection
// to [0,10], then applied to the A→B directional row only (B evaluates its own
// direction when its own session closes). Any error is logged and dropped.
func (a *agentContext) evaluateConversationAffection(ctx context.Context, agentID, peerID string, turns []conversationTurn, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) {
	store := a.as.Store()
	if store == nil || a.strategicHc == nil || len(turns) == 0 {
		return // 内存模式 / 无战略客户端 / 无对话
	}

	// 当前好感度（本方向 agentID→peerID）。
	cur := 5
	if rels, err := store.LoadRelationships(ctx, agentID, 10); err == nil {
		for _, r := range rels {
			other := r.AgentB
			if r.AgentA != agentID {
				other = r.AgentA
			}
			if other == peerID {
				if r.Affection >= 0 && r.Affection <= 10 {
					cur = r.Affection
				}
				break
			}
		}
	}

	agentName := agentID
	peerName := peerID
	if kb != nil {
		if ag := kb.GetAgent(agentID); ag != nil && ag.DisplayName != "" {
			agentName = ag.DisplayName
		}
		if ag := kb.GetAgent(peerID); ag != nil && ag.DisplayName != "" {
			peerName = ag.DisplayName
		}
	}
	role := prompt.AgentRole(kb, profiles, agentID)
	user := fmt.Sprintf("你是 %s（id=%s）。\n%s\n\n【与对方的好感度】%s（%d/10）\n\n【本次对话记录】\n%s\n\n请评估这段对话对你与 %s 之间好感度的影响，输出一个整数 delta（-3 到 +3）。",
		agentName, agentID, role, peerName, cur, formatConversationTurns(turns, kb), peerName)

	evalCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := a.strategicHc.SendWithSchema(evalCtx, conversationAffectionSystemPrompt, user, "affection_delta", []byte(conversationAffectionSchema))
	if err != nil {
		logger.Warn("[关系层] 好感度评估调用失败，跳过", "agent_id", agentID, "peer", peerID, "err", err)
		return
	}
	delta, err := parseAffectionDelta(resp.ExtractText())
	if err != nil {
		logger.Warn("[关系层] 好感度评估解析失败，跳过", "agent_id", agentID, "peer", peerID, "err", err)
		return
	}
	if delta < -3 {
		delta = -3
	}
	if delta > 3 {
		delta = 3
	}
	newAff := cur + delta
	if newAff < 0 {
		newAff = 0
	}
	if newAff > 10 {
		newAff = 10
	}
	applied := newAff - cur
	if applied == 0 {
		return
	}
	if err := store.SaveRelationship(evalCtx, agentID, peerID, 0, applied); err != nil {
		logger.Warn("[关系层] 保存好感度 A→B 失败", "agent_id", agentID, "peer", peerID, "err", err)
		return
	}
	logger.Info("[关系层] 好感度已调整", "agent_id", agentID, "peer", peerID, "delta", applied, "affection", newAff)
}

// parseAffectionDelta extracts the integer delta from the structured-output JSON.
func parseAffectionDelta(raw string) (int, error) {
	var out struct {
		Delta int `json:"delta"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err != nil {
		return 0, fmt.Errorf("parse delta: %w", err)
	}
	return out.Delta, nil
}

// formatConversationTurns renders the turn transcript as "名字：内容" lines
// (chronological, most recent last), using KB display names where available.
func formatConversationTurns(turns []conversationTurn, kb *worldkb.KB) string {
	var sb strings.Builder
	for _, t := range turns {
		name := t.speakerID
		if kb != nil {
			if ag := kb.GetAgent(t.speakerID); ag != nil && ag.DisplayName != "" {
				name = ag.DisplayName
			}
		}
		fmt.Fprintf(&sb, "%s：%s\n", name, t.content)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// maxConversationTurns 是单次对话会话的轮次上限（一句 speak_to 计一轮，
// 双方会话各自全量计数——own turn 由 speak_to 下发记录、对方的话由
// speech_heard 记录）。超限后【社交回应】hint 追加硬收尾指令：对话的
// 结束不再依赖 LLM 采样自觉或护栏运气（2026-10-16 前实测：应邀后双方
// 无限互道"走吧"的 ping-pong，最终靠 severity 阶梯碰巧拦断收场）。
const maxConversationTurns = 6

// renderConversationTurnsFor renders turns from agentID's perspective: own
// turns as "你", peer turns by KB display name (fall back to raw id).
func renderConversationTurnsFor(agentID string, turns []conversationTurn, kb *worldkb.KB) string {
	var sb strings.Builder
	for _, t := range turns {
		speaker := t.speakerID
		if t.speakerID == agentID {
			speaker = "你"
		} else if kb != nil {
			if ag := kb.GetAgent(t.speakerID); ag != nil && ag.DisplayName != "" {
				speaker = ag.DisplayName
			}
		}
		fmt.Fprintf(&sb, "%s：%s\n", speaker, t.content)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// conversationContextForPrompt renders the in-progress conversation context
// for the tactical prompt's 【社交回应】 hint: full transcript (own turns as
// "你", peer turns by display name) + round count. capped reports whether the
// session has reached maxConversationTurns. No active session → ("", false).
func (a *agentContext) conversationContextForPrompt(agentID string, kb *worldkb.KB) (ctx string, capped bool) {
	a.coordMu.Lock()
	peer := a.convPeerID
	turns := append([]conversationTurn(nil), a.convTurns...)
	a.coordMu.Unlock()
	if peer == "" || len(turns) == 0 {
		return "", false
	}
	peerName := peer
	if kb != nil {
		if ag := kb.GetAgent(peer); ag != nil && ag.DisplayName != "" {
			peerName = ag.DisplayName
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "【对话记录】（与 %s（%s）的当前对话，已 %d 轮）\n", peerName, peer, len(turns))
	// 轮次有上限（超限即收尾），全量注入无负担；防御性截最近 12 条。
	start := 0
	if len(turns) > 12 {
		start = len(turns) - 12
	}
	sb.WriteString(renderConversationTurnsFor(agentID, turns[start:], kb))
	return sb.String(), len(turns) >= maxConversationTurns
}

// conversationPeerState returns the active conversation peer id and the
// recorded turn count ("" = no session). route() uses it to distinguish an
// in-conversation reply (peer match AND ≥2 turns — the recording in
// handleWorldEvent makes ANY speaker the peer before routing, so the turn
// count is what separates a reply from a fresh greeting's first utterance).
func (a *agentContext) conversationPeerState() (peer string, turns int) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	return a.convPeerID, len(a.convTurns)
}

// conversationEndedHint builds the post-conversation replan hint: the full
// transcript plus a follow-through directive. speak_to decompositions are
// single-action by design (rule 10) — the agreed activity is NOT carried by
// the conversation replan; this hint, set when the idle-close ends the
// session, is what drives the follow-through at the next refill (2026-10-16
// 设计：约定事项由系统在对话结束后提示，NPC 届时再执行——优先于当前
// 时段目标，补上 slot goal 主导下事件只作弱参考的仲裁缺口）。
func conversationEndedHint(agentID, peer string, turns []conversationTurn, kb *worldkb.KB) string {
	if peer == "" || len(turns) == 0 {
		return ""
	}
	peerName := peer
	if kb != nil {
		if ag := kb.GetAgent(peer); ag != nil && ag.DisplayName != "" {
			peerName = ag.DisplayName
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "【对话结束】你与 %s（%s）的对话刚刚结束，完整对话记录：\n", peerName, peer)
	// 会话轮次有上限，全量注入无负担；防御性截最近 12 条。
	start := 0
	if len(turns) > 12 {
		start = len(turns) - 12
	}
	sb.WriteString(renderConversationTurnsFor(agentID, turns[start:], kb))
	sb.WriteString("\n请结合对话内容决定下一步：若对话中你们约定了一起做某事（如一起去跑步机晨跑），立即去执行该约定——守约优先于当前时段目标，不要因为时段切换而弃约。" +
		"对话记录中的时间戳早于当前游戏时间是正常的（对话往返在高倍速仿真下会消耗数十游戏分钟）——约定刚刚达成、此刻仍然有效：立即执行不算迟到，更不意味着约定的活动已经完成或窗口已过，禁止以\"时间已过\"为理由把约定叙述为已发生。" +
		"注意：只用一句独白宣告\"我要去了\"不算执行——本轮分解必须包含执行约定的长动作（如 InteractSmartObject 使用约定设施，按规则 7/8 填 duration），可以先一条 speak 表态，但长动作不可省略。" +
		"执行约定动作的 duration 按活动实际需要设置（如晨跑 1800-3600 秒），可以超过当前时段的剩余时长——时段切换会在安全点等约定动作结束后再进行，不要为赶日程把约定活动压缩成几分钟的象征性动作；" +
		"若你的邀请被婉拒或双方只是寒暄/告别，回到当前时段目标的原有日程。")
	return sb.String()
}
