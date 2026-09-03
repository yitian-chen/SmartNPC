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
	"sync"
	"time"
)

// promptDocAgent 是落盘观察对象。换人观察时改这里。
const promptDocAgent = "H-01"

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

	fmt.Fprintf(f, "# 实际 LLM 请求体留存\n\n记录 H-01 最新一次发给 LLM 的战略层/战术层请求体完整 JSON（model/messages/tools 等所有字段），由 MCP 运行时覆盖落盘。\n\n")

	// 可读 Prompt 预览：取最近更新的 layer 的 system + user 内容，还原换行
	// 后放在文档开头，便于快速查看核心 prompt 而不必在 JSON 里翻找。
	if sys, usr, layer, ok := latestSystemUser(); ok {
		fmt.Fprintf(f, "## 可读 Prompt 预览（最新一次 · %s）\n\n", layerNameOf(layer))
		fmt.Fprintf(f, "### system\n\n%s\n\n", sys)
		fmt.Fprintf(f, "### user\n\n%s\n\n", usr)
	}

	for _, l := range []string{"strategic", "tactical"} {
		b, ok := promptDocBodies[l]
		if !ok {
			continue
		}
		layerName := map[string]string{"strategic": "战略层", "tactical": "战术层"}[l]
		if layerName == "" {
			layerName = l
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, b, "", "  "); err != nil {
			pretty.Write(b)
		}
		fmt.Fprintf(f, "## %s · H-01 最新%s请求体\n\n```json\n%s\n```\n\n",
			promptDocTimes[l], layerName, pretty.String())
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
	default:
		return layer
	}
}

// latestSystemUser 返回最近更新的 layer（strategic/tactical 中时间更晚者）
// 的 system 与最后一条 user 的 content。system 三层共享、各 layer 相同；
// user 取 messages 中最后一条 role=user（即该层最新一次请求的正文）。
// 无任何请求体时返回 ok=false。
func latestSystemUser() (system, user, layer string, ok bool) {
	// 找最近更新的 layer。
	var latest string
	for _, l := range []string{"strategic", "tactical"} {
		if _, exists := promptDocBodies[l]; !exists {
			continue
		}
		if latest == "" || promptDocTimes[l] > promptDocTimes[latest] {
			latest = l
		}
	}
	if latest == "" {
		return "", "", "", false
	}
	sys, usr := extractSystemUser(promptDocBodies[latest])
	return sys, usr, latest, true
}

// extractSystemUser 从请求体 JSON 提取 system 与最后一条 user 的 content。
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
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if system == "" {
				system = m.Content
			}
		case "user":
			user = m.Content // 覆盖取最后一条
		}
	}
	return system, user
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
	if body := rb.LastRequestBody(); len(body) > 0 {
		dumpPromptDoc(agentID, layer, body, logger)
	}
}
