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
// LLM call. narrative is injected into the next strategic prompt; the
// memories array is parsed but no longer persisted (long-term memory is now
// written via persistExtractedMemory / daily_summary episode).
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

// memorySystemPrompt is the memory layer's system message: mechanism text
// only (task, JSON schema, rules). Fully static → cacheable.
const memorySystemPrompt = `你是小镇居民 NPC 的记忆总结模块。用户信息给出 NPC 的角色与今日行动记录（按时间顺序），请回顾行动记录，总结经验。

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
5. related_*_id: 若记忆关联特定 agent/object/zone，填对应 id（来自用户信息中的世界知识），无关联则省略
6. narrative 应包含对今天工作节奏、身体状况、社交互动的简要反思
7. 必须以字符 { 开头，以字符 } 结尾`

// memoryUserTemplate is the memory layer's user message template.
// Placeholders: %s = agent role context, %s = chronological action list.
const memoryUserTemplate = `[记忆层/日终总结] 你刚结束一天的活动。

%s

【今日行动记录】（按时间顺序）
%s

请回顾今天的行动记录，总结经验。`

// generateDailyMemories summarizes yesterday's action_history into a
// first-person narrative (昨日总结) for the next strategic prompt, and
// persists that narrative as a daily_summary episode (game_time 锚定) so it
// survives context clearing (day rollover = 该日上下文的 compact)。
//
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
	gameTime int64,
	gameDay int,
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
	promptText := fmt.Sprintf(memoryUserTemplate, roleCtx, actionList)

	logger.Info("[MCP→LLM/MEMORY-PROMPT]", "agent_id", agentID,
		"action_count", len(records), "text", promptText)

	resp, err := sc.SendWithSummary(ctx, memorySystemPrompt, promptText)
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

	// 写 daily_summary episode（昨日总结 → 长期记忆，日切即该日上下文的
	// compact）。best-effort，失败不影响 narrative 返回。
	if store != nil && result.Narrative != "" {
		ep := storage.Episode{
			ID:         newEpisodeID(),
			OwnerID:    agentID,
			GameTime:   gameTime,
			GameDay:    gameDay,
			Category:   "daily_summary",
			Summary:    result.Narrative,
			Importance: 5,
			Source:     "self",
			Status:     "active",
		}
		if err := store.SaveEpisode(ctx, ep); err != nil {
			logger.Warn("[记忆层] 昨日总结 episode 落盘失败",
				"agent_id", agentID, "err", err)
		}
	}

	logger.Info("[记忆层] 昨日总结生成成功",
		"agent_id", agentID, "narrative", result.Narrative)
	return result.Narrative
}

// ─── compact 结构化记忆抽取 ─────────────────────────────

// compactExtract 是 compact 摘要 LLM 的结构化输出：滚动摘要串 + 结构化记忆
// （经历 / 对人认知），一次调用同时产出，写路径据此落 episodic_memories /
// semantic_people。待办（commitments）不再在 compact 抽取——它们由对话结束时
// 的 extractConversationCommitments 及时抽取（见下）。
type compactExtract struct {
	Summary       string            `json:"summary"`
	Episodes      []compactEpisode   `json:"episodes"`
	PeopleUpdates []compactPersonUpd `json:"people_updates"`
}

type compactEpisode struct {
	Summary      string   `json:"summary"`
	Category     string   `json:"category"` // 空 → conversation
	Importance   int      `json:"importance"`
	Emotion      float64  `json:"emotion"`
	Participants []string `json:"participants"`
}

// compactCommitment / compactCmChange 供对话结束的 commitment 抽取复用（原为
// compact 的 new_commitments / commitment_changes，现已移到对话结束路径）。
// due 用「绝对游戏日 + HH:MM」而非绝对游戏秒——LLM 把"明天下午四点"抄成
// day=当前日+1/time="16:00" 可靠，但要它算累计秒（"一天 86400 秒"）不可靠，
// 实测会输出 Unix 墙钟秒或瞎猜整数（2026-10-26 生日派对仿真）。代码侧
// dayTimeToGameSec 负责换算。
type compactCommitment struct {
	Kind            string `json:"kind"`
	CounterpartID   string `json:"counterpart_id"`
	CounterpartName string `json:"counterpart_name"`
	Content         string `json:"content"`
	DueStartDay     int    `json:"due_start_day"`
	DueStartTime    string `json:"due_start_time"` // "HH:MM"
	DueEndDay       int    `json:"due_end_day"`
	DueEndTime      string `json:"due_end_time"`
	DueLocation     string `json:"due_location"`
}

type compactCmChange struct {
	ID           string `json:"id"`
	Action       string `json:"action"` // done / reschedule / cancel
	DueStartDay  int    `json:"due_start_day"`
	DueStartTime string `json:"due_start_time"`
	DueEndDay    int    `json:"due_end_day"`
	DueEndTime   string `json:"due_end_time"`
}

type compactPersonUpd struct {
	TargetID      string `json:"target_id"`
	AffinityDelta int    `json:"affinity_delta"`
	Impression    string `json:"impression"`
}

// newEpisodeID / newCommitmentID 生成主键。唯一性靠纳秒时间戳，单进程内足够。
func newEpisodeID() string    { return fmt.Sprintf("ep_%d", time.Now().UnixNano()) }
func newCommitmentID() string { return fmt.Sprintf("cm_%d", time.Now().UnixNano()) }

// dayTimeToGameSec 把「绝对游戏日（1-based）+ HH:MM」换算成累计游戏秒。游戏日
// 界为午夜：第 1 天 = 0~86399、第 2 天 = 86400~172799（与 episodic_memories
// 的 game_day 口径一致）。day<=0 或时间解析失败返回 0。
func dayTimeToGameSec(day int, timeStr string) int64 {
	if day <= 0 || timeStr == "" {
		return 0
	}
	var hh, mm int
	if _, err := fmt.Sscanf(strings.TrimSpace(timeStr), "%d:%d", &hh, &mm); err != nil {
		return 0
	}
	return int64((day-1)*86400 + hh*3600 + mm*60)
}

// persistExtractedMemory 把 compact LLM 的结构化抽取落盘为长期记忆三表
// （episodic_memories / episodic_commitments / semantic_people）。这是长期
// 记忆的唯一 LLM 写入时机——不 compact 时经历仍在上下文中，无需落盘。
//
// best-effort：任一条写失败仅告警继续，不阻断压缩主流程；store 为 nil
// （内存模式）时直接跳过。
func persistExtractedMemory(store storage.Store, agentID string, ex compactExtract, gameTime int64, gameDay int, logger *slog.Logger) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, ep := range ex.Episodes {
		if ep.Summary == "" {
			continue
		}
		cat := ep.Category
		if cat == "" {
			cat = "conversation"
		}
		clampInt(&ep.Importance, 1, 10)
		clampFloat(&ep.Emotion, -1, 1)
		e := storage.Episode{
			ID:           newEpisodeID(),
			OwnerID:      agentID,
			GameTime:     gameTime,
			GameDay:      gameDay,
			Category:     cat,
			Summary:      ep.Summary,
			Participants: ep.Participants,
			Importance:   ep.Importance,
			Emotion:      ep.Emotion,
			Source:       "self",
			Status:       "active",
		}
		if err := store.SaveEpisode(ctx, e); err != nil {
			logger.Warn("[记忆层] compact episode 落盘失败", "agent_id", agentID, "err", err)
		}
	}

	for _, pu := range ex.PeopleUpdates {
		if pu.TargetID == "" {
			continue
		}
		p := storage.Person{
			OwnerID:          agentID,
			TargetID:         pu.TargetID,
			Impression:       pu.Impression,
			LastInteractTime: gameTime,
		}
		if err := store.UpsertPerson(ctx, p, pu.AffinityDelta); err != nil {
			logger.Warn("[记忆层] compact 好感度更新失败", "agent_id", agentID, "target", pu.TargetID, "err", err)
		}
	}

	if len(ex.Episodes)+len(ex.PeopleUpdates) > 0 {
		logger.Debug("[记忆层] compact 结构化记忆已落盘",
			"agent_id", agentID, "episodes", len(ex.Episodes),
			"people", len(ex.PeopleUpdates))
	}
}

// persistCommitments 把对话结束抽取的待办落盘（新建 + 核销/改期/取消）。返回
// 新建的 commitment（供调用方判断是否触发战略层重规划）。best-effort，store
// 为 nil 时返回 nil。
func persistCommitments(store storage.Store, agentID string, news []compactCommitment, changes []compactCmChange, gameTime int64, logger *slog.Logger) []storage.Commitment {
	if store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var created []storage.Commitment
	for _, cm := range news {
		if cm.Content == "" {
			continue
		}
		kind := cm.Kind
		if kind == "" {
			kind = "appointment"
		}
		c := storage.Commitment{
			ID:              newCommitmentID(),
			OwnerID:         agentID,
			Kind:            kind,
			CounterpartID:   cm.CounterpartID,
			CounterpartName: cm.CounterpartName,
			Content:         cm.Content,
			DueStart:        dayTimeToGameSec(cm.DueStartDay, cm.DueStartTime),
			DueEnd:          dayTimeToGameSec(cm.DueEndDay, cm.DueEndTime),
			DueLocation:     cm.DueLocation,
			Status:          "pending",
			CreatedTime:     gameTime,
		}
		if err := store.SaveCommitment(ctx, c); err != nil {
			logger.Warn("[记忆层] 对话结束待办落盘失败", "agent_id", agentID, "err", err)
			continue
		}
		created = append(created, c)
	}

	for _, ch := range changes {
		if ch.ID == "" {
			continue
		}
		status := ""
		dueStart, dueEnd := int64(-1), int64(-1)
		switch ch.Action {
		case "done":
			status = "done"
		case "cancel", "cancelled":
			status = "cancelled"
		case "reschedule":
			status = "pending"
			dueStart = dayTimeToGameSec(ch.DueStartDay, ch.DueStartTime)
			dueEnd = dayTimeToGameSec(ch.DueEndDay, ch.DueEndTime)
		default:
			continue
		}
		if err := store.UpdateCommitment(ctx, ch.ID, status, dueStart, dueEnd); err != nil {
			logger.Warn("[记忆层] 对话结束待办核销失败", "agent_id", agentID, "id", ch.ID, "err", err)
		}
	}
	return created
}

// conversationCommitmentSystemPrompt / Schema 是对话结束待办抽取的 system 与
// 结构化输出 schema。只抽「未来要做的具体事」，即时寒暄不抽。
const conversationCommitmentSystemPrompt = `你是 NPC 待办抽取模块。用户信息给出 NPC 的人设、当前游戏时间、以及刚结束的一段对话记录。请从中抽取「未来某时某地要做的事」（约好做某事 / 答应帮谁传话 / 受邀参加活动），以及对话中对已有待办的核销/改期/取消。

输出一个 JSON 对象（不要 markdown 围栏，不要其他文字）：
{
  "new_commitments": [{"kind":"appointment/relay/invitation/todo","counterpart_id":"对方id","counterpart_name":"对方名","content":"待办内容","due_start_day":游戏日,"due_start_time":"HH:MM","due_end_day":游戏日,"due_end_time":"HH:MM","due_location":"地点"}],
  "commitment_changes": [{"id":"已有待办id","action":"done/reschedule/cancel","due_start_day":游戏日,"due_start_time":"HH:MM","due_end_day":游戏日,"due_end_time":"HH:MM"}]
}

要求：
1. 只抽取「未来要做的具体事」，即时性寒暄/闲聊/情绪表达不抽。
2. 时间用「绝对游戏日 + HH:MM」两个字段（不要算成累计秒）：游戏日以【当前游戏时间】的"D1"为基准——"明天下午四点" = due_start_day 填 2、due_start_time 填 "16:00"；"今天晚饭后" = due_start_day 填 1、due_start_time 填 "19:00"。把相对时间原样抄成"第几天 + 几点"，不要做任何秒的换算。
3. 区分「谁办的活动」：
   - 别人办的公开活动（生日会/聚会），你受邀参加 → kind 填 invitation，counterpart_id 填活动主角（寿星）的 id。
   - 你自己要办/主持的活动 → kind 填 todo，counterpart_id 留空。
   - 约好和某人一起做某事 → kind 填 appointment，counterpart_id 填对方 id。
   - 答应帮某人传话 → kind 填 relay，counterpart_id 填要传话的对象 id。
4. 只有对话里明确取消/改期/履行了某个已有待办，才填 commitment_changes（id 是已有待办的 id）；否则空数组。
5. 没有就填空数组，不要编造。`

const conversationCommitmentSchema = `{
  "type": "object",
  "properties": {
    "new_commitments": {"type": "array", "items": {"type": "object", "properties": {
      "kind": {"type": "string"},
      "counterpart_id": {"type": "string"},
      "counterpart_name": {"type": "string"},
      "content": {"type": "string"},
      "due_start_day": {"type": "integer"},
      "due_start_time": {"type": "string"},
      "due_end_day": {"type": "integer"},
      "due_end_time": {"type": "string"},
      "due_location": {"type": "string"}
    }, "required": ["content"], "additionalProperties": false}},
    "commitment_changes": {"type": "array", "items": {"type": "object", "properties": {
      "id": {"type": "string"},
      "action": {"type": "string"},
      "due_start_day": {"type": "integer"},
      "due_start_time": {"type": "string"},
      "due_end_day": {"type": "integer"},
      "due_end_time": {"type": "string"}
    }, "required": ["id", "action"], "additionalProperties": false}}
  },
  "required": ["new_commitments", "commitment_changes"],
  "additionalProperties": false
}`

// conversationCommitmentExtract 是对话结束待办抽取的结果。
type conversationCommitmentExtract struct {
	NewCommitments    []compactCommitment `json:"new_commitments"`
	CommitmentChanges []compactCmChange   `json:"commitment_changes"`
}

// extractConversationCommitments 在对话结束时把 transcript 抽成待办（新建 +
// 核销/改期/取消）落盘，返回新建的 commitment。这是待办的唯一及时写入时机——
// 当天约定（如"下午帮我传话"）不必等到日切 compact 才被抽到。失败/无对话时
// 返回 nil。
func (a *agentContext) extractConversationCommitments(ctx context.Context, agentID, peerID string, turns []conversationTurn, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) []storage.Commitment {
	if a.strategicHc == nil || a.as.Store() == nil || len(turns) == 0 {
		return nil
	}
	gameTime := int64(a.as.LatestGameTimeSec())
	gameDay := a.as.LatestDayCount() + 1
	tod := gameTime % 86400

	roleCtx := prompt.AgentRole(kb, profiles, agentID)
	transcript := renderConversationTurnsFor(agentID, turns, kb)
	user := fmt.Sprintf("【你的角色】\n%s\n\n【当前游戏时间】D%d %02d:%02d\n\n【刚结束的对话记录】\n%s\n\n请抽取这段对话中的未来待办（约定/传话/受邀）与待办核销。",
		roleCtx, gameDay, tod/3600, (tod%3600)/60, transcript)

	exCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := a.strategicHc.SendWithSchema(exCtx, conversationCommitmentSystemPrompt, user, "conversation_commitments", []byte(conversationCommitmentSchema))
	if err != nil {
		logger.Warn("[记忆层] 对话结束待办抽取调用失败", "agent_id", agentID, "peer", peerID, "err", err)
		return nil
	}
	raw := strings.TrimSpace(resp.ExtractText())
	var ex conversationCommitmentExtract
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &ex); err != nil {
		logger.Warn("[记忆层] 对话结束待办解析失败", "agent_id", agentID, "peer", peerID, "raw", truncateText(raw, 120), "err", err)
		return nil
	}
	created := persistCommitments(a.as.Store(), agentID, ex.NewCommitments, ex.CommitmentChanges, gameTime, logger)
	if len(created) > 0 {
		logger.Info("[记忆层] 对话结束待办已落盘", "agent_id", agentID, "peer", peerID, "commitments", len(created))
	}
	return created
}

// clampInt / clampFloat 夹紧数值到 [lo, hi]。
func clampInt(v *int, lo, hi int) {
	if *v < lo {
		*v = lo
	}
	if *v > hi {
		*v = hi
	}
}

func clampFloat(v *float64, lo, hi float64) {
	if *v < lo {
		*v = lo
	}
	if *v > hi {
		*v = hi
	}
}

// recordCombatEpisode 规则直写（0 LLM，事件时点）：被攻击 → combat episode +
// 攻击者好感度 -50。攻击是边沿触发的关键事实，等不到 compact（combat-detach
// 直接清上下文、不 compact），故在事件时点直写（见设计文档"特殊说明"）。
func recordCombatEpisode(store storage.Store, agentID, attacker, location string, gameTime int64, gameDay int, logger *slog.Logger) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary := "遭到攻击"
	if attacker != "" && location != "" {
		summary = fmt.Sprintf("在 %s 被 %s 攻击", location, attacker)
	} else if attacker != "" {
		summary = fmt.Sprintf("被 %s 攻击", attacker)
	}
	ep := storage.Episode{
		ID:         newEpisodeID(),
		OwnerID:    agentID,
		GameTime:   gameTime,
		GameDay:    gameDay,
		Location:   location,
		Category:   "combat",
		Summary:    summary,
		Importance: 9,
		Emotion:    -0.9,
		Source:     "self",
		Status:     "active",
	}
	if err := store.SaveEpisode(ctx, ep); err != nil {
		logger.Warn("[记忆层] combat episode 落盘失败", "agent_id", agentID, "err", err)
	}
	if attacker != "" {
		p := storage.Person{
			OwnerID:          agentID,
			TargetID:         attacker,
			TargetName:       attacker,
			Impression:       "危险的袭击者",
			LastInteractTime: gameTime,
		}
		if err := store.UpsertPerson(ctx, p, -50); err != nil {
			logger.Warn("[记忆层] 攻击者好感度下降失败",
				"agent_id", agentID, "attacker", attacker, "err", err)
		}
	}
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

// ─── 社交需求（social need）日终评估 ─────────────────────────

// socialNeedSystemPrompt is the social-need evaluation's system text.
const socialNeedSystemPrompt = `你是 NPC 社交需求评估模块。用户信息给出该 NPC 的人设、今天的日程安排、以及当前的社交需求值。请结合今天做了什么、有没有社交互动、以及人设的性格倾向，评估明天该 NPC 的社交需求值（0-10 整数，越高越渴望主动找人聊天）。只输出 JSON。`

// socialNeedSchema is the structured-output schema for the social-need
// evaluation: a single 0-10 integer.
const socialNeedSchema = `{"type":"object","properties":{"social_need":{"type":"integer","description":"明天的社交需求值 0-10"}},"required":["social_need"],"additionalProperties":false}`

// socialNeedUserTemplate is the social-need evaluation's user message template.
// Placeholders: %s = display name, %s = agent id, %s = persona, %s = today's
// daily plan, %d = current social need.
const socialNeedUserTemplate = `你是 %s（id=%s）。
%s

【今天的日程】%s

【当前社交需求】%d/10

请评估明天你的社交需求值（0-10 整数）。`

// generateDailySocialNeed evaluates the NEXT day's 社交需求 from today's
// schedule via a single LLM call (pro model, structured output). Returns the
// new value (clamped to [0,10]); on any failure it returns the current value
// unchanged so the strategic plan still has a sane number.
func generateDailySocialNeed(
	ctx context.Context,
	sc strategicCaller,
	agentID string,
	kb *worldkb.KB,
	profiles map[string]*profile.Profile,
	logger *slog.Logger,
	dailyPlan string,
	current int,
) int {
	if sc == nil {
		return current
	}
	agentName := agentID
	if kb != nil {
		if ag := kb.GetAgent(agentID); ag != nil && ag.DisplayName != "" {
			agentName = ag.DisplayName
		}
	}
	user := fmt.Sprintf(socialNeedUserTemplate, agentName, agentID,
		prompt.AgentRole(kb, profiles, agentID), dailyPlan, current)

	resp, err := sc.SendWithSchema(ctx, socialNeedSystemPrompt, user, "social_need", []byte(socialNeedSchema))
	if err != nil {
		logger.Warn("[社交需求] LLM 调用失败，保持当前值", "agent_id", agentID, "err", err)
		return current
	}
	var out struct {
		SocialNeed int `json:"social_need"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.ExtractText())), &out); err != nil {
		logger.Warn("[社交需求] 响应解析失败，保持当前值", "agent_id", agentID,
			"raw", truncateText(resp.ExtractText(), 120), "err", err)
		return current
	}
	if out.SocialNeed < 0 {
		out.SocialNeed = 0
	}
	if out.SocialNeed > 10 {
		out.SocialNeed = 10
	}
	logger.Info("[社交需求] 新一天社交需求已生成", "agent_id", agentID,
		"prev", current, "social_need", out.SocialNeed)
	return out.SocialNeed
}
