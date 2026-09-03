// Package main — 统一 agentic loop 编排。
//
// 每个 NPC 每天的战略/战术/对话三层共用一份会话历史（AgentState.
// conversation）：每次 LLM 调用（无论哪层）以 [system, ...历史, 本次 user]
// 发送，成功后把 user + assistant 两条消息追加进历史；跨日
//（detectDayRollover）清空历史重新开始，跨日记忆走既有 generateDailyMemories
// → 昨日总结注入次日战略轮 user 内容。
//
// 各层差异收敛为 agenticTurn 的三个参数：
//   - userContent：本次 append 的 user role 内容（各层 prompt builder 产出）
//   - toolChoice：战略/对话 = "none"（披露目录但必须以文本/JSON 作答），
//     战术 = "required"（必须调用工具）
//   - schemaName/schema：战略层传 daily_plan（response_format json_schema
//     strict），其余层空（不带 response_format）
//
// tools 字段每次请求都传（tacticalToolsFromRegistry 派生，含 social_chat）。
//
// 失败语义：LLM 调用失败（含 4001 重试耗尽）时历史保持不变——失败的
// user 消息不留悬空在历史里，下次调用重试完整请求。
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// agenticTurn 跑统一 loop 的一轮。请求 messages = [system(共享), ...历史,
// user(本次)]；成功后 append user + assistant 两条消息进
// AgentState.conversation（a.as），失败则历史不动。
//
// hc 决定本轮模型（战略轮传 strategicHc=pro，战术/对话轮传 tacticalHc=
// flash——Venus 客户端无状态，同一份历史在不同轮次间切模型可行）。
// kb/profiles 用于拼共享 system prompt（与各层 prompt 构建同源）。
// layer 仅用于日志与 docs/actual_prompts.md dump（"strategic"|"tactical"|
// "dialogue"）。
//
// 4001 重试（venus 校验 tools JSON 失败）在此统一处理，三层共用：相同请求体
// 重试上限 maxTacticalRetries 次；超时/连接/非 4001 错误不重试，由各层调用方
// 自行兜底（战略层默认计划 / 战术层 fallback 动作 / 对话层默认拒绝或收尾）。
func (a *agentContext) agenticTurn(ctx context.Context, hc llmClient, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger, agentID, layer, userContent, toolChoice, schemaName string, schema []byte) (*llmtypes.Response, error) {
	if hc == nil {
		return nil, fmt.Errorf("no LLM client")
	}

	// system 不入历史：每次发送时现拼（kb/profiles 单次仿真内不变，
	// 对同一 agent 字节级一致，可缓存）。
	system := prompt.BuildSharedSystemPrompt(kb, profiles, agentID)
	history := a.as.Conversation()

	// 请求 messages：[system, ...历史, user(本次)]。不先 append user——
	// 成功才落历史，失败不留半截。
	messages := make([]llmtypes.Message, 0, len(history)+2)
	messages = append(messages, llmtypes.Message{Role: "system", Content: system})
	messages = append(messages, history...)
	messages = append(messages, llmtypes.Message{Role: "user", Content: userContent})

	// tools 每次都传：与战术层一致的完整行动目录（含 social_chat）。
	var tools []venus.Tool
	if capabilityRegistryRef != nil {
		tools = tacticalToolsFromRegistry(capabilityRegistryRef, agentID)
	}

	resp, err := hc.SendLoop(ctx, messages, tools, toolChoice, schemaName, schema)
	for attempt := 1; err != nil && isVenusErrorCode(err, "4001") && attempt <= maxTacticalRetries; attempt++ {
		logger.Warn("[agentic-loop] venus 4001，重试相同请求体",
			"agent_id", agentID, "layer", layer, "retry", attempt, "max", maxTacticalRetries, "err", err)
		resp, err = hc.SendLoop(ctx, messages, tools, toolChoice, schemaName, schema)
	}
	// 实际 prompt 文档：记录 H-01 最新一次该层请求体完整 JSON（无论成败）。
	dumpLastRequestBody(agentID, layer, hc, logger)
	if err != nil {
		return nil, err
	}

	// 成功：user + assistant 追加进当天 loop 历史。战术轮的 assistant 携带
	// tool_calls，立即在其后 append 占位 tool（result=pending）闭环序列——
	// 满足 "assistant(tool_calls) 后必须紧跟 tool 消息" 的协议硬约束，且占位
	// 字节稳定不变以吃 prefix cache。真实结果由 recordActionCompletion 以
	// user role 注入到末尾（见 systemInjectedToolResult）。
	assistant := llmtypes.Message{Role: "assistant", Content: resp.ExtractText(), ToolCalls: resp.ToolCalls}
	a.as.AppendConversationMessage(messages[len(messages)-1])
	a.as.AppendConversationMessage(assistant)
	for _, tc := range resp.ToolCalls {
		if tc.ID != "" {
			a.as.AppendConversationMessage(llmtypes.Message{
				Role:       "tool",
				Content:    pendingToolResult,
				ToolCallID: tc.ID,
			})
		}
	}
	hc.ResetSession() // no-op（Venus 无状态），保留接口语义
	return resp, nil
}

// pendingToolResult 是 tool 占位消息的固定 content——字节级不变，保证
// conversation 前缀稳定、Venus prefix cache 可复用。真实结果不覆盖它，
// 而是以 user role 追加到末尾。
const pendingToolResult = "result=pending"
