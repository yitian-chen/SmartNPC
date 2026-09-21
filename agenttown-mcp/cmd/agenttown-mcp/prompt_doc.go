package main

// 实际 LLM 请求体落盘：记录 H-01 最新一次发给 LLM 的战略层/战术层
// 请求体的完整 JSON（model + messages + tools + tool_choice 等所有字段），
// 覆盖写入 docs/actual_prompts.md，用于在 docs 下维护一份"实际发给 LLM
// 的请求"文档——无需从日志反推，直接留存现场。
//
// 行为约定：
//   - 仅记录 H-01（命名常量，便于日后调整观察对象）
//   - 每个 layer（strategic/tactical）每次调用都更新"最新"请求体，并整体
//     重写文档（O_TRUNC），文档始终只反映两层的各自最新一次请求
//   - --prompt-doc="" 可关闭；默认 docs/actual_prompts.md（相对进程
//     工作目录，与 assets/ 等默认路径同一约定——服务器在仓库根目录启动）
//   - 落盘失败只记 warn 日志，绝不影响决策链路

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// promptDocAgent 是落盘观察对象。换人观察时改这里。
const promptDocAgent = "H-01"

// promptDocLayers 是文档中按此顺序展示的 layer（可读预览 + 完整 JSON）。
// router = 事件路由器（P2-5，无状态单发，event_router.go）。
var promptDocLayers = []string{"strategic", "tactical", "router", "dialogue"}

var (
	promptDocMu     sync.Mutex
	promptDocPath   string                // --prompt-doc flag；空串 = 关闭
	promptDocBodies = map[string][]byte{} // layer → 最新请求体 JSON
	promptDocTimes  = map[string]string{} // layer → 最近落盘时间
)

// setPromptDocPath 安装文档路径（main 在 flag.Parse 后调用；空串关闭）。
func setPromptDocPath(p string) {
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	promptDocPath = p
}

// dumpPromptDoc 记录 H-01 指定 layer 的最新请求体并重写文档。body 是发给
// LLM 的完整请求体 JSON（含 model/messages/tools/tool_choice 等所有字段）。
func dumpPromptDoc(agentID, layer string, body []byte, logger *slog.Logger) {
	if promptDocPath == "" || agentID != promptDocAgent {
		return
	}
	if len(body) == 0 {
		return
	}
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	promptDocBodies[layer] = body
	promptDocTimes[layer] = time.Now().Format("2006-01-02 15:04:05")

	if err := os.MkdirAll(filepath.Dir(promptDocPath), 0o755); err != nil {
		logger.Warn("[prompt-doc] 创建目录失败，跳过落盘", "dir", filepath.Dir(promptDocPath), "err", err)
		return
	}
	f, err := os.OpenFile(promptDocPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		logger.Warn("[prompt-doc] 打开文档失败，跳过落盘", "path", promptDocPath, "err", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "# 实际 LLM 请求体留存\n\n记录 H-01 最新一次发给 LLM 的战略层/战术层/对话层请求体完整 JSON（model/messages/tools 等所有字段），由 MCP 运行时覆盖落盘。\n\n")

	// 可读 Prompt 预览：每个 layer 的 system + user 内容，还原换行后放在
	// 文档开头，便于快速查看核心 prompt 而不必在 JSON 里翻找。user 预览
	// 捡拾末尾两条 user 消息（本轮 user prompt + <agent_state> 状态栏）。
	fmt.Fprintf(f, "## 可读 Prompt 预览\n\n")
	for _, l := range promptDocLayers {
		b, ok := promptDocBodies[l]
		if !ok {
			continue
		}
		sys, usr := extractSystemUser(b)
		if sys == "" && usr == "" {
			continue
		}
		fmt.Fprintf(f, "### %s · system\n\n%s\n\n", layerNameOf(l), sys)
		fmt.Fprintf(f, "### %s · user\n\n%s\n\n", layerNameOf(l), usr)
	}

	for _, l := range promptDocLayers {
		b, ok := promptDocBodies[l]
		if !ok {
			continue
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, b, "", "  "); err != nil {
			pretty.Write(b)
		}
		fmt.Fprintf(f, "## %s · H-01 最新%s请求体\n\n```json\n%s\n```\n\n",
			promptDocTimes[l], layerNameOf(l), pretty.String())
	}
	logger.Info("[prompt-doc] 已落盘 H-01 最新请求体",
		"path", promptDocPath, "layer", layerNameOf(layer), "bytes", len(body))
}

// layerNameOf maps a layer key to its display name ("strategic"→战略层）。
func layerNameOf(layer string) string {
	switch layer {
	case "strategic":
		return "战略层"
	case "tactical":
		return "战术层"
	case "router":
		return "事件路由"
	case "dialogue":
		return "对话层"
	default:
		return layer
	}
}

// extractSystemUser 从请求体 JSON 提取 system 与末尾 user 的 content。
//
// 末尾 user 捡拾倒数两条：agenticTurn 在本轮 user prompt 之后追加瞬态
// <agent_state> 状态栏（同为 user role），只取最后一条会只剩状态栏——
// 预览应为"最后一条 user prompt + 状态栏"两条拼接。请求未带状态栏
// （无感知数据等）时保持原行为，只取最后一条 user。
func extractSystemUser(body []byte) (system, user string) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", ""
	}
	var users []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if system == "" {
				system = m.Content
			}
		case "user":
			users = append(users, m.Content)
		}
	}
	if len(users) == 0 {
		return system, ""
	}
	// 末条是状态栏 → 拼上其前一条（本轮 user prompt），两条都进预览。
	if last := users[len(users)-1]; strings.HasPrefix(last, agentStateBarOpen) && len(users) >= 2 {
		return system, users[len(users)-2] + "\n\n" + last
	}
	return system, users[len(users)-1]
}

// dumpLastRequestBody 读取 LLM 客户端最近一次发送的完整请求体并落盘。
// lc 未实现 LastRequestBody（如测试 fake）或为 nil 时静默跳过。
func dumpLastRequestBody(agentID, layer string, lc any, logger *slog.Logger) {
	if promptDocPath == "" || agentID != promptDocAgent {
		return
	}
	rb, ok := lc.(interface{ LastRequestBody() []byte })
	if !ok {
		return
	}
	body := rb.LastRequestBody()
	if len(body) == 0 {
		return
	}
	// 跳过压缩层请求体：maybeCompactConversation 在 agenticTurn 内部用同一
	// tacticalHc 调 SendWithSummary 做压缩，lastRequestBody 可能被设为压缩
	// 请求体（system 含"对话压缩模块"）。某些路径（流式/限流重试）下
	// send() 未覆盖它，dumpLastRequestBody 就会把压缩请求体记为战术层
	// 请求体——压缩层 user prompt 含完整历史原文（每轮战术 prompt 重复
	// 【全天日程】/【分解规则】等），在 actual_prompts.md 里显示为大量重复。
	if isCompactRequestBody(body) {
		return
	}
	dumpPromptDoc(agentID, layer, body, logger)
}

// isCompactRequestBody reports whether a request body is the compaction
// layer's (system message = 对话压缩模块), so dumpLastRequestBody can skip
// it — it must not be recorded as a tactical/strategic/dialogue request.
func isCompactRequestBody(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "对话压缩模块") {
			return true
		}
	}
	return false
}
