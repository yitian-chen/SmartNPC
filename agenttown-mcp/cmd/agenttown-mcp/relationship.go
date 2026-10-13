package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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

// formatRelationshipsForPrompt renders the relationship rows as a bullet list
// for the tactical prompt【人际关系】段. Only rows involving agentID are
// passed in (from LoadRelationships); each line shows the other side and the
// current familiarity/affection/interaction_count. Returns "" when the slice
// is empty (caller skips the prompt segment).
// formatRelationshipsForPrompt renders the relationship lines for the
// prompt, deduplicated by the OTHER agent id: LoadRelationships queries
// both A→B and B→A rows (WHERE agent_a=? OR agent_b=?), so an agent who
// has interacted with H-03 gets two rows that both read "与 H-03" — that
// duplication confused the LLM and wasted tokens (2026-09-18 仿真实测).
// When both directions exist, the higher familiarity/interaction wins
// (the direction with the latest update_at is already first per the query
// ORDER BY, but dedup by max value is safer).
func formatRelationshipsForPrompt(rels []storage.Relationship, agentID string) string {
	if len(rels) == 0 {
		return ""
	}
	type relLine struct {
		familiarity  int
		affection    int
		interactions int
	}
	best := make(map[string]*relLine)
	for _, r := range rels {
		other := r.AgentB
		if r.AgentA != agentID {
			other = r.AgentA
		}
		cur := best[other]
		if cur == nil || r.Familiarity > cur.familiarity || r.InteractionCount > cur.interactions {
			best[other] = &relLine{
				familiarity:  r.Familiarity,
				affection:    r.Affection,
				interactions: r.InteractionCount,
			}
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, other := range keys {
		r := best[other]
		fmt.Fprintf(&sb, "- 与 %s：熟悉度 %d、好感 %d/10（互动 %d 次）\n",
			other, r.familiarity, r.affection, r.interactions)
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
// 5 游戏分钟：对话冷掉即视为结束，既给多轮往返留足时间、也不无限挂起。
const conversationIdleTimeoutGameSec = 5 * 60.0

// recordConversationTurn appends a turn to the agent's in-progress conversation
// session keyed by peerID. A peer change clears the old session (an abandoned
// conversation is discarded, not evaluated). nowGameSec is the authoritative
// game time of the turn.
func (a *agentContext) recordConversationTurn(speakerID, peerID, content string, nowGameSec float64) {
	a.coordMu.Lock()
	defer a.coordMu.Unlock()
	if a.convPeerID != peerID {
		a.convPeerID = peerID
		a.convTurns = nil
	}
	a.convTurns = append(a.convTurns, conversationTurn{speakerID: speakerID, content: content})
	a.convLastGameSec = nowGameSec
}

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
	peer := a.convPeerID
	turns := a.convTurns
	a.convPeerID = ""
	a.convTurns = nil
	a.convLastGameSec = 0
	return peer, turns
}

// clearConversation discards the in-progress conversation session (combat-detach
// entry). An interrupted conversation is not evaluated.
func (a *agentContext) clearConversation() {
	a.coordMu.Lock()
	a.convPeerID = ""
	a.convTurns = nil
	a.convLastGameSec = 0
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
