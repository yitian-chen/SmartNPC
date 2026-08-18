package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// memoryGenerationResult is the JSON schema returned by the memory-generation
// LLM call. narrative is injected into the next strategic prompt; memories
// are saved as individual agent_memories rows.
type memoryGenerationResult struct {
	Narrative string `json:"narrative"`
	Memories  []struct {
		Type            string `json:"type"`
		Content         string `json:"content"`
		Importance      int    `json:"importance"`
		RelatedAgentID  string `json:"related_agent_id,omitempty"`
		RelatedObjectID string `json:"related_object_id,omitempty"`
		RelatedZoneID   string `json:"related_zone_id,omitempty"`
	} `json:"memories"`
}

// memoryPromptTemplate is the prompt for daily memory consolidation.
// Placeholders: %s = agent role context, %s = chronological action list.
const memoryPromptTemplate = `[记忆层/日终总结] 你刚结束一天的活动，请回顾今天的行动记录，总结经验。

%s

【今日行动记录】（按时间顺序）
%s

请输出一个 JSON 对象（不要 markdown 围栏，不要其他文字）：
{
  "narrative": "一段第一人称的叙事总结，描述今天做了什么、有什么感受或发现（2-3句话，用于明天规划参考）",
  "memories": [
    {"type":"event","content":"一句话描述","importance":60,"related_object_id":"workbench_01"}
  ]
}

要求：
1. memories 数组 3-5 条，每条是一个独立的记忆单元
2. type 取值: event（事件）/ skill（技能经验）/ relationship（社交关系）/ daily_summary（日常总结）
3. content: 一句话描述这条记忆的内容（第一人称）
4. importance: 0-100，越重要越高（默认 50）
5. related_*_id: 若记忆关联特定 agent/object/zone，填对应 id（来自上方世界知识），无关联则省略
6. narrative 应包含对今天工作节奏、身体状况、社交互动的简要反思
7. 必须以字符 { 开头，以字符 } 结尾`

// generateDailyMemories summarizes yesterday's action_history into 3-5
// structured memories via a single LLM call. Returns the narrative string
// (for strategic prompt injection). Saves structured memories to the store.
// Returns "" on any failure or empty history — caller falls back to the
// yesterdaySummaryForFirstDay constant. Never blocks the decision pipeline.
func generateDailyMemories(
	ctx context.Context,
	sc strategicCaller,
	store storage.Store,
	agentID string,
	kb *worldkb.KB,
	profiles map[string]*profile.Profile,
	logger *slog.Logger,
) string {
	if store == nil {
		return "" // in-memory mode, no history
	}
	records, err := store.LoadActionHistory(ctx, agentID, 500)
	if err != nil {
		logger.Warn("[记忆层] 加载 action_history 失败，跳过记忆生成",
			"agent_id", agentID, "err", err)
		return ""
	}
	if len(records) == 0 {
		logger.Info("[记忆层] 无 action_history，跳过记忆生成（首日或冷启动）",
			"agent_id", agentID)
		return ""
	}

	// Reverse DESC -> chronological for the LLM.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	roleCtx := ""
	if role := prompt.AgentRole(kb, profiles, agentID); role != "" {
		roleCtx = "【你的角色】\n" + role
	}
	actionList := formatActionHistoryForPrompt(records)
	promptText := fmt.Sprintf(memoryPromptTemplate, roleCtx, actionList)

	logger.Info("[MCP→LLM/MEMORY-PROMPT]", "agent_id", agentID,
		"action_count", len(records), "text", promptText)

	resp, err := sc.SendWithSummary(ctx, "", promptText)
	if err != nil {
		logger.Warn("[记忆层] LLM 调用失败，跳过记忆生成",
			"agent_id", agentID, "err", err)
		return ""
	}
	sc.ResetSession()

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/MEMORY-RESPONSE]", "agent_id", agentID,
		"tokens", resp.Usage.TotalTokens, "raw", raw)

	result, err := parseMemoryGenerationResult(raw)
	if err != nil {
		logger.Warn("[记忆层] 响应解析失败，跳过记忆生成",
			"agent_id", agentID, "raw", truncateText(raw, 200), "err", err)
		return ""
	}

	// Save structured memories (best-effort: continue on per-row error).
	now := time.Now()
	saved := 0
	for _, m := range result.Memories {
		mem := storage.Memory{
			AgentID:         agentID,
			MemoryType:      m.Type,
			Content:         m.Content,
			Importance:      m.Importance,
			RelatedAgentID:  m.RelatedAgentID,
			RelatedObjectID: m.RelatedObjectID,
			RelatedZoneID:   m.RelatedZoneID,
			CreatedAt:       now,
			LastAccessedAt:  now,
			DecayScore:      1.0,
		}
		if mem.Importance == 0 {
			mem.Importance = 50 // default
		}
		if _, err := store.SaveMemory(ctx, agentID, mem); err != nil {
			logger.Warn("[记忆层] 保存单条记忆失败（继续保存其余）",
				"agent_id", agentID, "type", m.Type, "err", err)
			continue
		}
		saved++
	}

	logger.Info("[记忆层] 日终记忆生成成功",
		"agent_id", agentID, "memories_saved", saved,
		"narrative", result.Narrative)
	return result.Narrative
}

// formatActionHistoryForPrompt renders action records as a numbered
// chronological list for the LLM. Each line: "N. HH:MM Cmd(params) [src] -> result".
func formatActionHistoryForPrompt(records []storage.ActionRecord) string {
	if len(records) == 0 {
		return "（无行动记录）"
	}
	var sb strings.Builder
	for i, r := range records {
		fmt.Fprintf(&sb, "%d. %s %s(%s) [%s] -> %s\n",
			i+1,
			r.StartedAt.Format("15:04"),
			r.Cmd,
			formatParamsShort(r.Params),
			r.Source,
			r.Result)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// formatParamsShort renders params as "key=value,key=value" (truncated).
func formatParamsShort(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(params))
	for k, v := range params {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	s := strings.Join(parts, ",")
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

// parseMemoryGenerationResult extracts the JSON object from the LLM response.
// Tolerates markdown fences and leading/trailing prose.
func parseMemoryGenerationResult(raw string) (memoryGenerationResult, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return memoryGenerationResult{}, fmt.Errorf("no JSON object found")
	}
	var result memoryGenerationResult
	if err := json.Unmarshal([]byte(s[start:end+1]), &result); err != nil {
		return memoryGenerationResult{}, fmt.Errorf("unmarshal: %w", err)
	}
	return result, nil
}
