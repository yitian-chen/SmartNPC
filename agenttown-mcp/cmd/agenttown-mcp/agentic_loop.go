// Package main — 统一 agentic loop 编排。
//
// 每个 NPC 每天的战略/战术/对话三层共用一份会话历史（AgentState.
// conversation）：每次 LLM 调用（无论哪层）以 [system, ...历史, 本次 user]
// 发送，成功后把 user + assistant 两条消息追加进历史；跨日
// （detectDayRollover）清空历史重新开始，跨日记忆走既有 generateDailyMemories
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
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/llmmetrics"
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
// 立即重试上限 maxTacticalRetries 次。429 限流（公共模型服务并发有限，
// 30/min）退避重试——退避 + 随机抖动既等限流窗口恢复，也让同时被拒的
// 多个 NPC（如 5 个 worker 同时注册触发的战略轮）重试自然错峰。超时/
// 连接/其他错误不重试，由各层调用方自行兜底（战略层默认计划 / 战术层
// fallback 动作 / 对话层默认拒绝或收尾）。
func (a *agentContext) agenticTurn(ctx context.Context, hc llmClient, kb *worldkb.KB, profiles map[string]*profile.Profile, logger *slog.Logger, agentID, layer, userContent, toolChoice, schemaName string, schema []byte) (*llmtypes.Response, error) {
	if hc == nil {
		return nil, fmt.Errorf("no LLM client")
	}

	// system 不入历史：每次发送时现拼（kb/profiles 单次仿真内不变，
	// 对同一 agent 字节级一致，可缓存）。
	system := prompt.BuildSharedSystemPrompt(kb, profiles, agentID)

	// tools 每次都传：与战术层一致的完整行动目录（含 social_chat）。
	var tools []venus.Tool
	if capabilityRegistryRef != nil {
		tools = tacticalToolsFromRegistry(capabilityRegistryRef, agentID)
	}

	// 上下文压缩：估算输入 token，超阈值时把旧历史摘要成稳定 digest、
	// 只保留最近几轮原始尾（保护 [system + tools + 摘要块] 前缀的 KV cache）。
	a.maybeCompactConversation(system, tools, userContent, agentID, kb, profiles, logger)

	// 重新读历史（压缩可能已改写）+ 摘要块。
	history := a.as.Conversation()
	summary := a.as.ConversationSummary()

	// 请求 messages：[system, (摘要块), ...历史, user(本次)]。不先 append
	// user——成功才落历史，失败不留半截。
	messages := make([]llmtypes.Message, 0, len(history)+3)
	messages = append(messages, llmtypes.Message{Role: "system", Content: system})
	if summary != "" {
		messages = append(messages, llmtypes.Message{Role: "user", Content: "【上下文摘要】\n" + summary})
	}
	messages = append(messages, history...)
	messages = append(messages, llmtypes.Message{Role: "user", Content: userContent})

	// 流式采集（仅战术层 + --tactical-stream）：非流式只能测 E2E，流式才能
	// 测 TTFT/TPOT/ITL。onDelta 在 venus.parseStream 内同步回调，时间戳即
	// token 到达时刻。
	streaming := tacticalStreamingEnabled && layer == "tactical"
	t0 := time.Now()
	var (
		ttft    time.Duration
		itls    []time.Duration
		lastTok time.Time
	)
	onDelta := func(delta string) {
		if delta == "" {
			return
		}
		now := time.Now()
		if ttft == 0 {
			ttft = now.Sub(t0)
			lastTok = now
			return
		}
		itls = append(itls, now.Sub(lastTok))
		lastTok = now
	}
	send := func() (*llmtypes.Response, error) {
		if streaming {
			return hc.SendLoopStreaming(ctx, messages, tools, toolChoice, schemaName, schema, onDelta, nil)
		}
		return hc.SendLoop(ctx, messages, tools, toolChoice, schemaName, schema)
	}

	resp, err := send()
	retries := 0
	for attempt := 1; attempt <= maxTacticalRetries; attempt++ {
		if err != nil {
			if isVenusErrorCode(err, "4001") {
				// LLM 输出坏 JSON：立即重试相同请求体，重采样即修复。
				logger.Warn("[agentic-loop] venus 4001，重试相同请求体",
					"agent_id", agentID, "layer", layer, "retry", attempt, "max", maxTacticalRetries, "err", err)
			} else if errors.Is(err, venus.ErrEmptyCompletion) {
				// 后端过载返回空流（200 + 立即 [DONE]，无 content/tool_calls）：
				// 立即重试相同请求体，重采样通常能拿到正常补全。
				logger.Warn("[agentic-loop] venus 空完成，重试相同请求体",
					"agent_id", agentID, "layer", layer, "retry", attempt, "max", maxTacticalRetries, "err", err)
			} else if isRateLimited(err) {
				// 公共模型服务限流：退避 + 随机抖动后重试。等待期间 ctx
				// 取消（进程关停/上层超时）则立即放弃。抖动让同时被拒的
				// 多个 NPC 重试自然错峰。
				backoff := rateLimitBackoffBase + time.Duration(rand.IntN(int(rateLimitBackoffBase)*3/4))
				logger.Warn("[agentic-loop] venus 限流（429），退避后重试",
					"agent_id", agentID, "layer", layer, "retry", attempt, "max", maxTacticalRetries,
					"backoff_ms", backoff.Milliseconds(), "err", err)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff):
				}
			} else {
				// 超时/连接/其他错误不重试。
				break
			}
		} else if isEmptyTacticalResult(resp, toolChoice, agentID) {
			// 战术层空结果（tool_choice=required 但 0 tool_calls / 全是 speak）：
			// 立即重试相同请求体，重采样通常能拿到正常分解。
			logger.Warn("[agentic-loop] 战术层空结果，重试相同请求体",
				"agent_id", agentID, "layer", layer, "retry", attempt, "max", maxTacticalRetries,
				"tool_calls", len(resp.ToolCalls))
		} else {
			// 成功且非空结果。
			break
		}
		retries = attempt
		resp, err = send()
	}
	// 实际 prompt 文档：记录 H-01 最新一次该层请求体完整 JSON（无论成败）。
	dumpLastRequestBody(agentID, layer, hc, logger)

	// 指标埋点：E2E/TTFT/TPOT/ITL/错误分类/重试（无论成败都采集）。
	e2e := time.Since(t0)
	var tpot time.Duration
	var outputTokens int
	if resp != nil {
		outputTokens = resp.Usage.OutputTokens
		if ttft > 0 && outputTokens > 1 {
			tpot = (e2e - ttft) / time.Duration(outputTokens-1)
		}
	}
	errClass := classifyLLMError(err)
	if err == nil && isEmptyTacticalResult(resp, toolChoice, agentID) {
		errClass = llmmetrics.ErrEmptyResult
	}
	llmMetricsCollector.RecordCall(llmmetrics.CallSample{
		Layer:        layer,
		E2E:          e2e,
		TTFT:         ttft,
		TPOT:         tpot,
		ITLs:         itls,
		OutputTokens: outputTokens,
		ErrClass:     errClass,
		Retried:      retries > 0,
		RetryCount:   retries,
	})
	dumpLLMMetrics(logger)

	if err != nil {
		return nil, err
	}
	if isEmptyTacticalResult(resp, toolChoice, agentID) {
		// 重试耗尽后仍是空结果：返回错误，不追加空历史（避免 pending tool 占位残留）。
		return nil, fmt.Errorf("tactical empty result: %d tool_calls", len(resp.ToolCalls))
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

// rateLimitBackoffBase 是 429 限流重试的退避基础时长（实际退避 = base +
// [0, 3/4·base) 随机抖动）。包级变量便于测试临时调小加速。
var rateLimitBackoffBase = 2 * time.Second

// isEmptyTacticalResult 判断 tool_choice=required 的响应是否为"空结果"：
//   - 0 个 tool_calls（LLM 无视 required 返回文本/空体）；
//   - 全是 speak（speak-only，无长动作）；
//   - 除 speak 外全会被 filterValidActions 过滤（scan_area/stop/wait/未知），
//     即过滤后只剩 speak、无有效长动作。
//
// 战术层需要至少一个带 duration 的长动作，空结果应重试而非入队（入队会让
// NPC 秒空队列 → 高频重分解 → 呆站）。用 tacticalActionAvailable 与
// generateTacticalPlan 的 filterValidActions 保持同一判定口径。
func isEmptyTacticalResult(resp *llmtypes.Response, toolChoice, agentID string) bool {
	if toolChoice != "required" || resp == nil {
		return false
	}
	if len(resp.ToolCalls) == 0 {
		return true
	}
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name == "speak" {
			continue // 首动作 speak 不算长动作
		}
		if tacticalActionAvailable(tc.Function.Name, agentID, capabilityRegistryRef) {
			return false // 有一个能被 filterValidActions 接受的长动作
		}
	}
	return true // 全是 speak，或非 speak 全被过滤
}

// classifyLLMError 把一次 LLM 调用的最终错误映射到指标错误类别。复用
// 战术层的 isVenusErrorCode / isRateLimited 分类，再按错误文本区分超时/
// HTTP/网络/其他。
func classifyLLMError(err error) string {
	if err == nil {
		return llmmetrics.ErrSuccess
	}
	if errors.Is(err, venus.ErrEmptyCompletion) {
		return llmmetrics.ErrEmptyCompletion
	}
	if isVenusErrorCode(err, "4001") {
		return llmmetrics.ErrBadJSON4001
	}
	if isRateLimited(err) {
		return llmmetrics.ErrRateLimited
	}
	s := err.Error()
	if strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "timeout") {
		return llmmetrics.ErrTimeout
	}
	if strings.Contains(s, "venus status") {
		return llmmetrics.ErrHTTPError
	}
	if strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") ||
		strings.Contains(s, "no such host") || strings.Contains(s, "dial tcp") ||
		strings.Contains(s, "http do") || strings.Contains(s, "EOF") {
		return llmmetrics.ErrNetwork
	}
	return llmmetrics.ErrOther
}
