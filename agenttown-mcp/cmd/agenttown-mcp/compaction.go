package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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
	// compactTriggerTokens：估算输入 token 超过此值触发压缩（16k 封顶下取 14k）。
	compactTriggerTokens = 14000
	// compactTailRounds：压缩时保留的最近完整轮数（每轮=一个 assistant 及其
	// 紧跟的 tool 占位与后续注入的 [系统注入] 结果）。
	compactTailRounds = 4
	// compactSummaryMaxChars：摘要块长度上限（字符）。
	compactSummaryMaxChars = 800
)

// compactSystemPrompt 是上下文压缩的 system 消息：机制文本，静态可缓存。
const compactSystemPrompt = `你是小镇居民 NPC 的对话压缩模块。用户信息给出 NPC 的角色与一段多轮决策对话历史（按时间顺序），请把这段历史压缩成一份紧凑摘要，用于替代原文、供后续决策参考。

要求：
1. 摘要不超过 800 字，纯散文、无 markdown、无列表符号
2. 必须覆盖四点：
   - 已完成动作时间线：NPC 今天已经做了什么、结果如何
   - 当前时段目标与进度：现在在做什么、进行到哪一步
   - 未完成/待办：接下来还计划做什么
   - 关键约束与社交约定：任何中断原因、与其它 NPC 的约定、需要记住的事项
3. 直接输出摘要文本本身，不要任何解释或前缀`

// compactUserTemplate 是上下文压缩的 user 消息模板。
// 占位符：%s = 角色上下文，%s = 按时间顺序渲染的对话历史。
const compactUserTemplate = `[压缩层/上下文压缩] 请压缩以下多轮决策历史。

%s

【历史原文】（按时间顺序）
%s`

// maybeCompactConversation 估算本次请求输入 token，超阈值时把旧历史摘要
// 成稳定 digest、只保留最近 compactTailRounds 轮。摘要失败时跳过本轮压缩
// （带全量历史继续、下轮再试），绝不无摘要地丢弃历史（那正是滚动窗口的
// 目标漂移问题）。
func (a *agentContext) maybeCompactConversation(ctx context.Context, system string, tools []venus.Tool, userContent, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) {
	if a.strategicHc == nil {
		return // 无战略层客户端可做摘要，跳过
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
	newSummary, err := a.summarizeConversation(ctx, evict, agentID, kb, profiles, logger)
	if err != nil {
		logger.Warn("[压缩层] 摘要生成失败，跳过本次压缩（下轮重试）",
			"agent_id", agentID, "evict_msgs", len(evict), "err", err)
		return
	}
	a.as.CompactConversation(newSummary, tail)
	logger.Info("[压缩层] 上下文压缩完成",
		"agent_id", agentID,
		"before_msgs", len(history), "after_msgs", len(tail),
		"summary_chars", len(newSummary),
		"est_tokens", estimateInputTokens(system, tools, newSummary, tail, userContent))
}

// summarizeConversation 用战略层模型把 evict 历史压缩成一份摘要串（best-effort，
// 不 JSON 解析，直接取文本）。
func (a *agentContext) summarizeConversation(ctx context.Context, evict []llmtypes.Message, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger) (string, error) {
	roleCtx := ""
	if role := prompt.AgentRole(kb, profiles, agentID); role != "" {
		roleCtx = "【你的角色】\n" + role
	}
	promptText := fmt.Sprintf(compactUserTemplate, roleCtx, formatConversationForSummary(evict))

	logger.Info("[MCP→LLM/COMPACT-PROMPT]", "agent_id", agentID,
		"evict_msgs", len(evict), "text", promptText)

	resp, err := a.strategicHc.SendWithSummary(ctx, compactSystemPrompt, promptText)
	if err != nil {
		return "", fmt.Errorf("compact llm: %w", err)
	}
	a.strategicHc.ResetSession()

	summary := strings.TrimSpace(resp.ExtractText())
	if summary == "" {
		return "", fmt.Errorf("compact llm: empty summary")
	}
	if n := len([]rune(summary)); n > compactSummaryMaxChars {
		summary = string([]rune(summary)[:compactSummaryMaxChars])
	}
	logger.Info("[LLM→MCP/COMPACT-RESPONSE]", "agent_id", agentID,
		"tokens", resp.Usage.TotalTokens, "summary", summary)
	return summary, nil
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
