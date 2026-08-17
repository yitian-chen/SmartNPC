package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"

	"log/slog"
	"strings"
)

// dialogueMaxTurns is the soft cap after which the LLM is urged to end
// gracefully. Hard cap (force-end) is a few turns above this so the LLM
// gets a chance to say goodbye.
const dialogueMaxTurns = 6

// dialoguePhase tracks where this agent is in the 4-step handshake + turn
// exchange. Mirrors the design doc's per-Mind conversation_state.phase.
type dialoguePhase string

const (
	phaseNone    dialoguePhase = "none"
	phaseInviting dialoguePhase = "inviting"
	phaseActive  dialoguePhase = "active"
	phaseClosing dialoguePhase = "closing"
)

// dialogueRole distinguishes the initiator (A, sent social_chat) from the
// target (B, received chat_invite). Affects which steps of the handshake
// this agent drives.
type dialogueRole string

const (
	roleInitiator dialogueRole = "initiator"
	roleTarget    dialogueRole = "target"
)

// dialogueRunner owns one agent's conversation state and implements the
// Agent-side dialogue decision logic (Phase 2 Module C). It is constructed
// per-agent (a field on agentContext) and accessed from the WS handler
// goroutine (chat_invite/rsp/turn arrival) and the worker goroutine
// (inDialogue() suppression check).
//
// Lock discipline:
//   - mu protects all conversation state fields (convID/peerID/role/phase/
//     shortTermContext/turnCount). Held during LLM calls + outbound sends
//     so turns are serialized.
//   - Never hold mu while calling agentContext.coordMu or AgentState methods
//     that take their own lock — read snapshots BEFORE acquiring mu, or
//     release mu first. In practice we read the snapshot up front, then
//     hold mu for the LLM call + send + persist.
type dialogueRunner struct {
	ac       *agentContext
	ws       *wsserver.Server
	kb       *worldkb.KB
	profiles map[string]*profile.Profile
	logger   *slog.Logger

	mu               sync.Mutex
	convID           string
	peerID           string
	role             dialogueRole
	phase            dialoguePhase
	shortTermContext []prompt.DialogueTurnEntry
	turnCount        int
}

// newDialogueRunner constructs a per-agent dialogue runner. Returns a zero
// runner (phase=none); conversation state is populated on the first invite
// or invite_rsp. deps may be nil when dialogue is disabled — callers should
// nil-check the returned runner before invoking handlers.
func newDialogueRunner(ac *agentContext, ws *wsserver.Server, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) *dialogueRunner {
	if ac == nil || ws == nil {
		return nil
	}
	return &dialogueRunner{
		ac:       ac,
		ws:       ws,
		kb:       kb,
		profiles: profiles,
		logger:   logger,
		phase:    phaseNone,
	}
}

// active reports whether this agent is currently in a dialogue (phase other
// than none). Called by the worker suppression guard (inDialogue) — no lock
// held by caller, so we take mu briefly.
func (d *dialogueRunner) active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.phase != phaseNone
}

// handleInvite is called when this agent (as B, the target) receives a
// chat_invite from A via UE. Decides accept/reject via LLM, sends
// chat_invite_rsp, and on accept sends the opening chat_turn (B's reply)
// then waits for A's turns.
func (d *dialogueRunner) handleInvite(_ context.Context, payload protocol.ChatInvitePayload) {
	if d == nil {
		return
	}
	agentID := d.ac.as.AgentID()
	if payload.FromAgentID == "" || payload.ConvID == "" {
		d.logger.Warn("[对话层/invite] 缺少 from/conv_id，丢弃", "agent_id", agentID, "conv_id", payload.ConvID)
		return
	}

	// Read snapshot BEFORE acquiring mu (lock discipline: no nested locks).
	snap := d.ac.as.Snapshot()
	d.mu.Lock()
	if d.phase != phaseNone {
		d.mu.Unlock()
		d.logger.Warn("[对话层/invite] 已在对话中，拒绝新邀请",
			"agent_id", agentID, "existing_conv", d.convID, "new_conv", payload.ConvID)
		// Decline politely so UE frees A.
		d.sendInviteRsp(payload.ConvID, false)
		return
	}
	// Occupy the conversation slot.
	d.convID = payload.ConvID
	d.peerID = payload.FromAgentID
	d.role = roleTarget
	d.phase = phaseInviting
	d.shortTermContext = []prompt.DialogueTurnEntry{
		{SpeakerID: payload.FromAgentID, SpeakerName: d.peerName(payload.FromAgentID), Content: payload.Content},
	}
	d.turnCount = 1
	d.mu.Unlock()

	d.logger.Info("[对话层/invite] 收到邀请，决策中",
		"agent_id", agentID, "peer", payload.FromAgentID, "conv_id", payload.ConvID)

	decision, err := d.generateInviteDecision(snap, payload.FromAgentID, payload.Content)
	if err != nil {
		d.logger.Warn("[对话层/invite] LLM 决策失败，默认拒绝", "agent_id", agentID, "err", err)
		d.sendInviteRsp(payload.ConvID, false)
		d.cleanup()
		return
	}

	if !decision.Accept {
		d.logger.Info("[对话层/invite] 拒绝邀请", "agent_id", agentID, "peer", payload.FromAgentID)
		d.sendInviteRsp(payload.ConvID, false)
		d.cleanup()
		return
	}

	// Accept: send rsp, then opening turn (if any reply), enter active.
	d.sendInviteRsp(payload.ConvID, true)
	d.mu.Lock()
	d.phase = phaseActive
	d.mu.Unlock()
	d.logger.Info("[对话层/invite] 接受邀请", "agent_id", agentID, "peer", payload.FromAgentID)

	if decision.Reply != "" {
		// B's opening reply is the first chat_turn.
		d.sendTurn(decision.Reply, false)
		d.mu.Lock()
		d.shortTermContext = append(d.shortTermContext, prompt.DialogueTurnEntry{
			SpeakerID: agentID, SpeakerName: d.peerName(agentID), Content: decision.Reply,
		})
		d.mu.Unlock()
		d.persistTurn(payload.ConvID, agentID, payload.FromAgentID, decision.Reply, 1, false)
	}
}

// handleInviteRsp is called when this agent (as A, the initiator) receives
// B's chat_invite_rsp forwarded by UE. On accept, enters active and waits
// for B's opening turn. On reject, cleans up (UE has already ended A's
// social_chat with an interrupted completion).
func (d *dialogueRunner) handleInviteRsp(_ context.Context, payload protocol.ChatInviteRspPayload) {
	if d == nil {
		return
	}
	agentID := d.ac.as.AgentID()
	d.mu.Lock()
	if d.phase != phaseInviting || d.convID != payload.ConvID {
		d.mu.Unlock()
		d.logger.Warn("[对话层/rsp] 非预期响应（未邀请或 conv 不匹配），忽略",
			"agent_id", agentID, "phase", d.phase, "conv", payload.ConvID, "existing", d.convID)
		return
	}
	if !payload.Accept {
		d.logger.Info("[对话层/rsp] 对方拒绝，结束对话", "agent_id", agentID, "peer", d.peerID)
		d.mu.Unlock()
		d.cleanup()
		return
	}
	// Accepted: seed context with A's own opening line (from the in-flight
	// social_chat action params) so the LLM remembers what A said first.
	d.phase = phaseActive
	opening := openingContent(snapCurrentActionParams(d.ac).CurrentActionParams)
	if opening != "" {
		d.shortTermContext = append([]prompt.DialogueTurnEntry{{
			SpeakerID: agentID, SpeakerName: d.peerName(agentID), Content: opening,
		}}, d.shortTermContext...)
	}
	peer := d.peerID
	d.mu.Unlock()
	d.logger.Info("[对话层/rsp] 对方接受，进入对话", "agent_id", agentID, "peer", peer)
	// A waits for B's opening chat_turn (if B sent one); otherwise B's first
	// turn will arrive via handleTurn. No proactive turn here.
}

// handleTurn is called when a peer's chat_turn arrives. Appends to context,
// checks end conditions, and generates + sends a reply. If the peer ended
// (end=true), optionally replies with our own end turn and cleans up.
func (d *dialogueRunner) handleTurn(_ context.Context, payload protocol.ChatTurnPayload) {
	if d == nil {
		return
	}
	agentID := d.ac.as.AgentID()

	// Stale/unknown conv → ignore (session already closed on the peer side).
	d.mu.Lock()
	if d.phase == phaseNone || d.convID != payload.ConvID {
		d.mu.Unlock()
		d.logger.Warn("[对话层/turn] 非预期 turn（无会话或 conv 不匹配），忽略",
			"agent_id", agentID, "conv", payload.ConvID, "existing", d.convID, "phase", d.phase)
		return
	}
	peer := d.peerID
	// Append peer's turn to short-term context.
	d.shortTermContext = append(d.shortTermContext, prompt.DialogueTurnEntry{
		SpeakerID: peer, SpeakerName: d.peerName(peer), Content: payload.Content,
	})
	d.turnCount++
	convID := d.convID
	// Persist the peer's turn (peer is the speaker, this agent is listener).
	// persistTurn no longer locks mu, so safe to call here.
	d.persistTurn(convID, peer, agentID, payload.Content, d.turnCount, payload.End)
	ctx := d.shortTermContext
	count := d.turnCount
	d.mu.Unlock()

	// Peer ended gracefully.
	if payload.End {
		d.logger.Info("[对话层/turn] 对方结束对话", "agent_id", agentID, "peer", peer, "turns", count)
		// Optionally reply with our own goodbye + end. For Phase 1, just
		// acknowledge and close.
		d.mu.Lock()
		d.phase = phaseClosing
		d.mu.Unlock()
		d.finalizeDialogue()
		return
	}

	// Interrupted end: peer left before the conversation established.
	if payload.Interrupted {
		d.logger.Info("[对话层/turn] 对话未成立（对方已离开）", "agent_id", agentID, "peer", peer)
		d.cleanup()
		return
	}

	// Normal turn: generate reply.
	snap := d.ac.as.Snapshot()
	result, err := d.generateTurn(snap, peer, payload.Content, ctx, count)
	if err != nil {
		d.logger.Warn("[对话层/turn] LLM 生成失败，优雅结束", "agent_id", agentID, "err", err)
		// End gracefully to avoid stalling the conversation.
		fallbackLine := "（沉默片刻）回头再说吧。"
		d.sendTurn(fallbackLine, true)
		d.mu.Lock()
		d.shortTermContext = append(d.shortTermContext, prompt.DialogueTurnEntry{
			SpeakerID: agentID, SpeakerName: d.peerName(agentID), Content: fallbackLine,
		})
		d.persistTurn(convID, agentID, peer, fallbackLine, count+1, true)
		d.mu.Unlock()
		d.finalizeDialogue()
		return
	}

	d.sendTurn(result.Content, result.End)
	d.mu.Lock()
	d.shortTermContext = append(d.shortTermContext, prompt.DialogueTurnEntry{
		SpeakerID: agentID, SpeakerName: d.peerName(agentID), Content: result.Content,
	})
	d.turnCount++
	d.persistTurn(convID, agentID, peer, result.Content, d.turnCount, result.End)
	d.mu.Unlock()

	if result.End {
		d.logger.Info("[对话层/turn] 主动结束对话", "agent_id", agentID, "peer", peer, "turns", count+1)
		d.finalizeDialogue()
	}
}

// onActionCompleted is called from recordActionCompletion when the social_chat
// action ends (UE sends action_completed with success/interrupted). Cleans
// up conversation state regardless of outcome — the dialogue is over.
func (d *dialogueRunner) onActionCompleted(completion protocol.ActionCompletedPayload, res agentstate.CompletionResult) {
	if d == nil {
		return
	}
	if res.Cmd != protocol.CmdSocialChat {
		return
	}
	if !d.active() {
		return
	}
	agentID := d.ac.as.AgentID()
	d.logger.Info("[对话层/action_completed] social_chat 动作结束，清理对话",
		"agent_id", agentID, "result", completion.Result, "conv", d.convID)
	// If interrupted (UE force-closed), skip the graceful finalize path —
	// just clean up. The peer side is handled by UE's ForceCloseDialogue.
	if completion.Result == "interrupted" {
		d.cleanup()
		return
	}
	d.finalizeDialogue()
}

// finalizeDialogue is the graceful-close path: bumps relationship, persists
// a summary memory, then resets state. Called when either side sends
// chat_turn{end:true} or the social_chat action completes successfully.
func (d *dialogueRunner) finalizeDialogue() {
	if d == nil {
		return
	}
	d.mu.Lock()
	peer := d.peerID
	turns := d.turnCount
	conv := d.convID
	ctx := d.shortTermContext
	d.mu.Unlock()
	if peer == "" {
		d.cleanup()
		return
	}
	d.bumpRelationship(peer)
	d.persistDialogueMemory(peer, turns, ctx)
	d.logger.Info("[对话层] 对话归档", "agent_id", d.ac.as.AgentID(), "peer", peer, "conv", conv, "turns", turns)
	d.cleanup()
}

// cleanup resets all conversation state to none. Safe to call multiple times.
func (d *dialogueRunner) cleanup() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.convID = ""
	d.peerID = ""
	d.role = ""
	d.phase = phaseNone
	d.shortTermContext = nil
	d.turnCount = 0
}

// ─── LLM generation ───

// generateInviteDecision calls the dialogue LLM to decide accept/reject and
// produce an optional opening reply. snapshot is the AgentState snapshot
// captured BEFORE mu was acquired (caller ensures recency).
func (d *dialogueRunner) generateInviteDecision(snap agentstate.Snapshot, peerID, peerContent string) (prompt.DialogueInviteDecision, error) {
	agentID := d.ac.as.AgentID()
	in := prompt.DialogueInviteInput{
		AgentID:        agentID,
		AgentName:      d.peerName(agentID),
		PeerID:         peerID,
		PeerName:       d.peerName(peerID),
		PeerContent:    peerContent,
		Persona:        d.persona(agentID),
		CurrentAction:  describeAction(snap.CurrentActionCmd, snap.CurrentActionParams),
		Physical:       prompt.PhysicalLine(snap.LatestPhysical),
		TimeOfDay:      snap.LatestTimeOfDay(),
		Relationship:   d.relationshipLine(peerID),
		RecentMemories: d.recentMemories(),
	}
	promptText := prompt.BuildDialogueInvite(in)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := d.ac.tacticalHc
	if hc == nil {
		return prompt.DialogueInviteDecision{}, fmt.Errorf("no LLM client")
	}
	resp, err := hc.SendWithSummary(ctx, promptText, "")
	if err != nil {
		return prompt.DialogueInviteDecision{}, fmt.Errorf("llm call: %w", err)
	}
	hc.ResetSession()
	decision, err := prompt.ParseDialogueInviteDecision(resp.ExtractText())
	if err != nil {
		return prompt.DialogueInviteDecision{}, fmt.Errorf("parse: %w", err)
	}
	return decision, nil
}

// generateTurn calls the dialogue LLM to produce one reply turn. snapshot
// is the physical-state snapshot for prompt context.
func (d *dialogueRunner) generateTurn(snap agentstate.Snapshot, peerID, peerContent string, ctx []prompt.DialogueTurnEntry, turnCount int) (prompt.DialogueTurnResult, error) {
	agentID := d.ac.as.AgentID()
	in := prompt.DialogueTurnInput{
		AgentID:          agentID,
		AgentName:        d.peerName(agentID),
		PeerID:           peerID,
		PeerName:         d.peerName(peerID),
		Persona:          d.persona(agentID),
		PeerContent:      peerContent,
		ShortTermContext: ctx,
		RecentMemories:   d.recentMemories(),
		Relationship:     d.relationshipLine(peerID),
		Physical:         prompt.PhysicalLine(snap.LatestPhysical),
		TimeOfDay:        snap.LatestTimeOfDay(),
		TurnCount:        turnCount,
		MaxTurns:         dialogueMaxTurns,
	}
	promptText := prompt.BuildDialogueTurn(in)
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := d.ac.tacticalHc
	if hc == nil {
		return prompt.DialogueTurnResult{}, fmt.Errorf("no LLM client")
	}
	resp, err := hc.SendWithSummary(callCtx, promptText, "")
	if err != nil {
		return prompt.DialogueTurnResult{}, fmt.Errorf("llm call: %w", err)
	}
	hc.ResetSession()
	result, err := prompt.ParseDialogueTurn(resp.ExtractText())
	if err != nil {
		return prompt.DialogueTurnResult{}, fmt.Errorf("parse: %w", err)
	}
	if result.Content == "" {
		return prompt.DialogueTurnResult{}, fmt.Errorf("empty content")
	}
	return result, nil
}

// ─── outbound sends ───

func (d *dialogueRunner) sendInviteRsp(convID string, accept bool) {
	agentID := d.ac.as.AgentID()
	err := d.ws.SendEnvelope(agentID, protocol.TypeChatInviteRsp, protocol.ChatInviteRspPayload{
		ConvID: convID,
		Accept: accept,
	})
	if err != nil {
		d.logger.Warn("[对话层] 发送 chat_invite_rsp 失败", "agent_id", agentID, "conv", convID, "err", err)
	}
}

func (d *dialogueRunner) sendTurn(content string, end bool) {
	agentID := d.ac.as.AgentID()
	d.mu.Lock()
	convID := d.convID
	d.mu.Unlock()
	err := d.ws.SendEnvelope(agentID, protocol.TypeChatTurn, protocol.ChatTurnPayload{
		ConvID:  convID,
		Content: content,
		End:     end,
	})
	if err != nil {
		d.logger.Warn("[对话层] 发送 chat_turn 失败", "agent_id", agentID, "conv", convID, "err", err)
	}
}

// ─── persistence ───

func (d *dialogueRunner) persistTurn(convID, speakerID, listenerID, content string, turnIdx int, isEnd bool) {
	store := d.ac.as.Store()
	if store == nil || convID == "" {
		return
	}
	agentID := d.ac.as.AgentID()
	rec := storage.Dialogue{
		ConvID:     convID,
		SpeakerID:  speakerID,
		ListenerID: listenerID,
		Content:    content,
		TurnIndex:  turnIdx,
		IsEnd:      isEnd,
		CreatedAt:  time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.SaveDialogue(ctx, rec); err != nil {
		d.logger.Warn("[对话层] 保存对话回合失败", "agent_id", agentID, "conv", convID, "err", err)
	}
}

// persistDialogueMemory saves one summary memory row recording that this
// agent had a conversation with peer. Content is a factual record (no extra
// LLM summarization in Phase 1); the daily memory generation pass can
// summarize it further later.
func (d *dialogueRunner) persistDialogueMemory(peerID string, turns int, ctx []prompt.DialogueTurnEntry) {
	store := d.ac.as.Store()
	if store == nil {
		return
	}
	agentID := d.ac.as.AgentID()
	peerName := d.peerName(peerID)
	// Build a compact transcript (capped to avoid huge rows).
	transcript := buildTranscript(ctx, 10)
	content := fmt.Sprintf("和 %s（id=%s）进行了一次对话，共 %d 轮。摘要：%s",
		peerName, peerID, turns, transcript)
	mem := storage.Memory{
		AgentID:        agentID,
		MemoryType:     "relationship",
		Content:        content,
		Importance:     55,
		RelatedAgentID: peerID,
		CreatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
		DecayScore:     1.0,
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := store.SaveMemory(ctx2, agentID, mem); err != nil {
		d.logger.Warn("[对话层] 保存对话记忆失败", "agent_id", agentID, "peer", peerID, "err", err)
	}
}

// bumpRelationship increments familiarity by 1 in both directions after a
// dialogue completes. Skips the Ollama judgment (a real dialogue happened →
// familiarity goes up). Affection is not auto-bumped (reserved for future
// sentiment-based updates).
func (d *dialogueRunner) bumpRelationship(peerID string) {
	store := d.ac.as.Store()
	if store == nil || peerID == "" {
		return
	}
	agentID := d.ac.as.AgentID()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.SaveRelationship(ctx, agentID, peerID, 1, 0); err != nil {
		d.logger.Warn("[对话层] 更新关系失败 A→B", "agent_id", agentID, "peer", peerID, "err", err)
		return
	}
	if err := store.SaveRelationship(ctx, peerID, agentID, 1, 0); err != nil {
		d.logger.Warn("[对话层] 更新关系失败 B→A", "agent_id", agentID, "peer", peerID, "err", err)
	}
}

// ─── prompt helpers (read-only, no mu needed) ───

func (d *dialogueRunner) persona(agentID string) string {
	return prompt.AgentRole(d.kb, d.profiles, agentID)
}

func (d *dialogueRunner) peerName(peerID string) string {
	if d.kb != nil {
		if a := d.kb.GetAgent(peerID); a != nil {
			return a.DisplayName
		}
	}
	return peerID
}

func (d *dialogueRunner) relationshipLine(peerID string) string {
	store := d.ac.as.Store()
	if store == nil {
		return ""
	}
	agentID := d.ac.as.AgentID()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rels, err := store.LoadRelationships(ctx, agentID, 10)
	if err != nil {
		return ""
	}
	for _, r := range rels {
		other := r.AgentB
		if r.AgentA != agentID {
			other = r.AgentA
		}
		if other == peerID {
			return fmt.Sprintf("熟悉度 %d、好感 %d（互动 %d 次）",
				r.Familiarity, r.Affection, r.InteractionCount)
		}
	}
	return ""
}

func (d *dialogueRunner) recentMemories() []string {
	store := d.ac.as.Store()
	if store == nil {
		return nil
	}
	agentID := d.ac.as.AgentID()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	memories, err := store.LoadRecentMemories(ctx, agentID, 5)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(memories))
	for _, m := range memories {
		out = append(out, m.Content)
	}
	return out
}

// openingContent extracts the `content` field from a social_chat action's
// params (A's opening line). Returns "" if params is nil/no content.
func openingContent(params map[string]any) string {
	if params == nil {
		return ""
	}
	if c, ok := params["content"]; ok {
		if s, ok := c.(string); ok {
			return s
		}
	}
	return ""
}

// buildTranscript concatenates up to maxEntries turns into a single string
// for the memory record. Each turn is "name：content" separated by " | ".
func buildTranscript(ctx []prompt.DialogueTurnEntry, maxEntries int) string {
	if len(ctx) == 0 {
		return "（无内容）"
	}
	if len(ctx) > maxEntries {
		ctx = ctx[len(ctx)-maxEntries:]
	}
	var sb strings.Builder
	for i, t := range ctx {
		if i > 0 {
			sb.WriteString(" | ")
		}
		name := t.SpeakerName
		if name == "" {
			name = t.SpeakerID
		}
		sb.WriteString(name)
		sb.WriteString("：")
		sb.WriteString(t.Content)
	}
	return sb.String()
}

// snapCurrentActionParams reads the current in-flight action params from
// AgentState. Used by handleInviteRsp to recover A's opening content.
func snapCurrentActionParams(ac *agentContext) agentstate.Snapshot {
	return ac.as.Snapshot()
}
