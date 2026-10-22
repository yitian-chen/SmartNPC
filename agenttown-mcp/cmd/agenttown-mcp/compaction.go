package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtokens"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// 上下文压缩常量。压缩把超出预算的旧历史摘要成一块稳定 digest，只保留
// 最近 compactTailRounds 轮原始尾，保护 [system + tools + 摘要块] 前缀的
// KV cache（摘要块两次压缩之间字节不变）。
const (
	// compactTriggerTokens：估算输入 token 超过此值触发压缩（10k 封顶；
	// 2026-10-20 由 8k 上调——实测 8k 下当天首条全量头（含全天日程 ~2-3k
	// 字符）在早上第一轮对话期间就会被逐出压缩，摘要把日程叙述成过去式
	// 回忆、误导 LLM 认为晨练等计划事项已发生；10k 让全量头多留几轮）。
	compactTriggerTokens = 10000
	// compactTailRounds：压缩时保留的最近完整轮数（每轮=一个 assistant 及其
	// 紧跟的 tool 占位与后续注入的 [系统注入] 结果）。
	compactTailRounds = 4
	// compactSummaryMaxChars：摘要块长度上限（字符）。
	compactSummaryMaxChars = 200
	// compactSummaryTimeout：摘要 LLM 调用的独立超时。摘要与战术层主调用
	// 解耦——不复用 tacticalCallTimeout（60s），因为战略层模型慢 + 摘要输入
	// 大，60s 内经常完不成导致「摘要失败→跳过」，压缩永远落不了地；失败时
	// 只是跳过本次、下轮重试，故给较长的独立预算。
	compactSummaryTimeout = 120 * time.Second
)

// compactSystemPrompt 是上下文压缩的 system 消息：机制文本，静态可缓存。
// 输出从纯文本摘要扩展为「摘要串 + 结构化记忆 JSON」（对齐长期记忆设计
// 文档 §4.2：一次调用同时产出经历 / 待办 / 对人认知）。
const compactSystemPrompt = `你是小镇居民 NPC 的记忆压缩模块。请把下方多轮决策历史压缩并抽取，输出一个 JSON 对象（不要 markdown 围栏，不要其他文字）。

{
  "summary": "不超过200字的回忆式摘要",
  "episodes": [{"summary":"一句话经历","category":"conversation/combat/action_failed","importance":1-10,"emotion":-1.0~1.0,"participants":["对方id"]}],
  "new_commitments": [{"kind":"appointment/relay/invitation","counterpart_id":"对方id","counterpart_name":"对方名","content":"待办内容","due_start":游戏秒,"due_end":游戏秒,"due_location":"地点"}],
  "commitment_changes": [{"id":"已有待办id","action":"done/reschedule/cancel","due_start":游戏秒,"due_end":游戏秒}],
  "people_updates": [{"target_id":"对方id","affinity_delta":-5~5,"impression":"对他的一句话印象"}]
}

要求：
1. summary：不超过 200 字，纯散文口语化，只写三类内容（干了什么事 / 有什么感想经验 / 与谁有过什么互动），不含物理状态数字、日程计划文本、系统提示语、分解规则原文。
2. episodes：只从【新一段历史原文】里抽取新发生的事（0~3 条），不要把旧摘要里已有的经历重复抽出。
3. new_commitments：只抽取对话里新约定的「未来某时某地要做的事」（约好明天几点下棋、答应帮谁传话、受邀参加活动）。没有新约定就填空数组。时间换算成绝对游戏秒（一天 86400 秒）。
4. commitment_changes：对话里取消 / 改期 / 履行了某个已有待办时，填对应 id + action（done=已履约、reschedule=改期并给新 due_start/due_end、cancel=取消）。没有就填空数组。
5. people_updates：对对话中互动的对方，给好感度变化 affinity_delta（-5~+5）与更新后的一句话印象 impression。没有明显变化就填空数组。
6. 没有的字段填空数组 / 空字符串，不要编造。`

// compactSchema 是 compact 结构化输出的 JSON Schema（Structured Outputs）。
const compactSchema = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "episodes": {"type": "array", "items": {"type": "object", "properties": {
      "summary": {"type": "string"},
      "category": {"type": "string"},
      "importance": {"type": "integer"},
      "emotion": {"type": "number"},
      "participants": {"type": "array", "items": {"type": "string"}}
    }, "required": ["summary"], "additionalProperties": false}},
    "new_commitments": {"type": "array", "items": {"type": "object", "properties": {
      "kind": {"type": "string"},
      "counterpart_id": {"type": "string"},
      "counterpart_name": {"type": "string"},
      "content": {"type": "string"},
      "due_start": {"type": "integer"},
      "due_end": {"type": "integer"},
      "due_location": {"type": "string"}
    }, "required": ["content"], "additionalProperties": false}},
    "commitment_changes": {"type": "array", "items": {"type": "object", "properties": {
      "id": {"type": "string"},
      "action": {"type": "string"},
      "due_start": {"type": "integer"},
      "due_end": {"type": "integer"}
    }, "required": ["id", "action"], "additionalProperties": false}},
    "people_updates": {"type": "array", "items": {"type": "object", "properties": {
      "target_id": {"type": "string"},
      "affinity_delta": {"type": "integer"},
      "impression": {"type": "string"}
    }, "required": ["target_id"], "additionalProperties": false}}
  },
  "required": ["summary"],
  "additionalProperties": false
}`

// compactUserTemplate 是上下文压缩的首轮 user 消息模板（无历史摘要时）。
// 占位符：%s = 角色上下文，%s = 按时间顺序渲染的对话历史。
const compactUserTemplate = `[压缩层/上下文压缩] 请压缩以下多轮决策历史。

%s

【历史原文】（按时间顺序）
%s`

// compactMergeUserTemplate 是连续压缩的合并式模板：此前已有一份摘要
// （覆盖更早的历史），本次必须把它与新一段历史**合并**成一份更新后的
// 摘要——不是拼接、更不是丢弃旧摘要只写新块（那会让每次压缩都遗忘
// 上一次逐出的内容，摘要块失去"滚动吸收"语义）。
// 占位符：%s = 角色上下文，%s = 此前摘要，%s = 新一段历史。
const compactMergeUserTemplate = `[压缩层/上下文压缩] 此前已有一份摘要（更早的对话历史已压缩为该摘要），请把它与下方新一段历史合并，输出一份更新后的完整摘要：覆盖两者的全部关键信息，删除重复，保持时间线连贯。合并后仍不超过 200 字，保持回忆式叙事风格。不要只概括新历史、不要遗漏旧摘要中的事实。

%s

【此前摘要】
%s

【新一段历史原文】（按时间顺序）
%s`

// maybeCompactConversation 估算本次请求输入 token，超阈值时把旧历史摘要
// 成稳定 digest、只保留最近 compactTailRounds 轮。摘要失败时跳过本轮压缩
// （带全量历史继续、下轮再试），绝不无摘要地丢弃历史（那正是滚动窗口的
// 目标漂移问题）。
func (a *agentContext) maybeCompactConversation(system string, tools []venus.Tool, userContent, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) {
	if a.tacticalHc == nil {
		return // 无战术层客户端可做摘要，跳过
	}
	history := a.as.Conversation()
	summary := a.as.ConversationSummary()
	if estimateInputTokens(system, tools, summary, history, userContent) < compactTriggerTokens {
		return
	}
	evict, tail := splitTailRounds(history, compactTailRounds)
	if len(evict) == 0 {
		return // 已是小尾巴，无从逐出
	}
	// in-flight 互斥：两个并发 agenticTurn（worker refill 进行中 + 战略
	// replan 插入等——worker 的 LLM 调用期间不持 replanInProgress）可能
	// 同时过阈值：双倍摘要成本，且第二次 CompactConversation 用旧快照
	// 覆盖会丢两次写入之间追加的消息。进行中则跳过，下轮调用重试
	// （2026-09-18 日志实测同秒双 COMPACT-PROMPT、双完成）。
	if !a.compacting.CompareAndSwap(false, true) {
		logger.Debug("[压缩层] 已有压缩在进行，跳过本次（下轮重试）", "agent_id", agentID)
		return
	}
	defer a.compacting.Store(false)

	ex, err := a.summarizeConversation(evict, summary, agentID, kb, profiles, logger)
	if err != nil {
		logger.Warn("[压缩层] 摘要生成失败，跳过本次压缩（下轮重试）",
			"agent_id", agentID, "evict_msgs", len(evict), "err", err)
		return
	}
	a.as.CompactConversation(ex.Summary, tail)
	// 长期记忆唯一写入时机：compact 把即将逐出的内容抽成结构化记忆，此时才
	// 落盘（不 compact 时内容仍在上下文中，无需写库）。best-effort，失败不
	// 阻断压缩主流程（persistExtractedMemory 内部 10s 超时 + 告警）。
	persistExtractedMemory(a.as.Store(), agentID, ex, int64(a.as.LatestGameTimeSec()), a.as.LatestDayCount()+1, logger)
	logger.Info("[压缩层] 上下文压缩完成",
		"agent_id", agentID,
		"before_msgs", len(history), "after_msgs", len(tail),
		"summary_chars", len(ex.Summary), "merged", summary != "",
		"episodes", len(ex.Episodes), "commitments", len(ex.NewCommitments), "people", len(ex.PeopleUpdates),
		"est_tokens", estimateInputTokens(system, tools, ex.Summary, tail, userContent))
}

// summarizeConversation 用战术层模型把 evict 历史压缩 + 抽取为 compactExtract
// （best-effort）。prevSummary 非空时走合并式模板——旧摘要与新历史合并成更新
// 后的摘要，连续压缩不丢更早的内容；episodes/commitments/people 只从新历史抽取。
func (a *agentContext) summarizeConversation(evict []llmtypes.Message, prevSummary, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) (compactExtract, error) {
	roleCtx := ""
	if role := prompt.AgentRole(kb, profiles, agentID); role != "" {
		roleCtx = "【你的角色】\n" + role
	}
	var promptText string
	if prevSummary != "" {
		promptText = fmt.Sprintf(compactMergeUserTemplate, roleCtx, prevSummary, formatConversationForSummary(evict))
	} else {
		promptText = fmt.Sprintf(compactUserTemplate, roleCtx, formatConversationForSummary(evict))
	}

	logger.Info("[MCP→LLM/COMPACT-PROMPT]", "agent_id", agentID,
		"evict_msgs", len(evict), "text", promptText)

	// 摘要调用独立超时：不复用调用方（战术层）的 60s ctx，避免 Venus 排队时
	// 摘要被过早取消。best-effort，失败只是跳过本次压缩。
	summaryCtx, cancel := context.WithTimeout(context.Background(), compactSummaryTimeout)
	defer cancel()
	resp, err := a.tacticalHc.SendWithSchema(summaryCtx, compactSystemPrompt, promptText, "compact_extract", []byte(compactSchema))
	// 清掉压缩调用留下的 lastRequestBody——防 dumpLastRequestBody 把
	// 压缩请求体误记为战术层请求（压缩 user prompt 含完整历史原文，
	// 在 actual_prompts.md 里会显示为大量重复段）。
	if c, ok := any(a.tacticalHc).(interface{ ClearLastRequestBody() }); ok {
		c.ClearLastRequestBody()
	}
	if err != nil {
		return compactExtract{}, fmt.Errorf("compact llm: %w", err)
	}
	a.tacticalHc.ResetSession()

	raw := strings.TrimSpace(resp.ExtractText())
	var ex compactExtract
	if err := json.Unmarshal([]byte(raw), &ex); err != nil {
		return compactExtract{}, fmt.Errorf("compact parse: %w", err)
	}
	ex.Summary = strings.TrimSpace(ex.Summary)
	if ex.Summary == "" {
		return compactExtract{}, fmt.Errorf("compact llm: empty summary")
	}
	if n := len([]rune(ex.Summary)); n > compactSummaryMaxChars {
		ex.Summary = string([]rune(ex.Summary)[:compactSummaryMaxChars])
	}
	logger.Info("[LLM→MCP/COMPACT-RESPONSE]", "agent_id", agentID,
		"tokens", resp.Usage.TotalTokens, "summary", ex.Summary,
		"episodes", len(ex.Episodes), "commitments", len(ex.NewCommitments), "people", len(ex.PeopleUpdates))
	return ex, nil
}

// splitTailRounds 把历史切成 (evict, tail)：保留最后 rounds 轮（以 assistant
// 为边界计数），返回被逐出的头部与保留的尾部。tail 起点总是 assistant（或其
// 后续消息），保证「assistant(tool_calls) → 紧跟 tool 占位」的协议配对不被拆断。
// 不足 rounds 轮时 evict 为空、tail 为全部。
func splitTailRounds(history []llmtypes.Message, rounds int) (evict, tail []llmtypes.Message) {
	if rounds <= 0 {
		return nil, history
	}
	if len(history) <= 1 {
		return nil, history
	}
	startIdx := -1
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" {
			continue
		}
		count++
		if count == rounds {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return nil, history // 不足 rounds 轮：无从逐出
	}
	return history[:startIdx], history[startIdx:]
}

// formatConversationForSummary 把被逐出历史渲染成文本供摘要 LLM 阅读。assistant
// 的 tool_calls 渲染成「助手调用工具 name(arguments)」；容忍其中可能存在的坏
// JSON（历史污染时只当文本，不解析）。
func formatConversationForSummary(msgs []llmtypes.Message) string {
	if len(msgs) == 0 {
		return "（空）"
	}
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "助手调用工具 %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			}
			if m.Content != "" {
				fmt.Fprintf(&sb, "助手：%s\n", m.Content)
			}
		case "tool":
			fmt.Fprintf(&sb, "工具结果[%s]：%s\n", m.ToolCallID, m.Content)
		default: // user / system
			fmt.Fprintf(&sb, "用户：%s\n", m.Content)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// estimateInputTokens 累加 system + tools + summary + history + user 的估算 token。
func estimateInputTokens(system string, tools []venus.Tool, summary string, history []llmtypes.Message, user string) int {
	total := llmtokens.EstimateTokens(system) + llmtokens.EstimateTokens(summary) + llmtokens.EstimateTokens(user)
	for _, t := range tools {
		total += llmtokens.EstimateTokens(t.Function.Name)
		total += llmtokens.EstimateTokens(t.Function.Description)
		total += llmtokens.EstimateTokens(string(t.Function.Parameters))
	}
	for _, m := range history {
		total += llmtokens.EstimateTokens(m.Content)
		total += llmtokens.EstimateTokens(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			total += llmtokens.EstimateTokens(tc.ID)
			total += llmtokens.EstimateTokens(tc.Function.Name)
			total += llmtokens.EstimateTokens(tc.Function.Arguments)
		}
	}
	return total
}
