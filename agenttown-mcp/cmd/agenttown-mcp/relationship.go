package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/ollama"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// relationshipJudgePromptTemplate is the prompt sent to Ollama to decide
// whether a completed action should bump the relationship with the target
// agent. Placeholders: %s = cmd, %s = params JSON, %s = target agent id.
//
// The prompt is deliberately short (~80 tokens) because Ollama runs locally
// on CPU and the decision is a simple yes/no classification. The 5s caller
// timeout is tighter than the reactive layer's 20s — relationship updates
// are best-effort and must not block the decision pipeline.
const relationshipJudgePromptTemplate = `[关系判断] 机器人刚完成一个动作，判断是否构成与 target_agent 的直接社交互动。
动作 cmd: %s
动作 params: %s
目标 agent: %s
仅输出 yes 或 no。yes=面向该 agent 的直接互动(聊天/维修/对话/转身面向); no=仅路过定位或 target 实为物体/区域。`

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
	promptText := fmt.Sprintf(relationshipJudgePromptTemplate, cmd, string(paramsJSON), target)
	raw, err := c.Chat(ctx, "", promptText)
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
func formatRelationshipsForPrompt(rels []storage.Relationship, agentID string) string {
	if len(rels) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, r := range rels {
		other := r.AgentB
		if r.AgentA != agentID {
			// agentID is agent_b side, so the other side is agent_a.
			other = r.AgentA
		}
		fmt.Fprintf(&sb, "- 与 %s：熟悉度 %d、好感 %d（互动 %d 次）\n",
			other, r.Familiarity, r.Affection, r.InteractionCount)
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
