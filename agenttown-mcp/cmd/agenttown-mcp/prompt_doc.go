package main

// 实际 prompt 文档落盘：每次仿真（进程）运行期间，把 H-01 的第一次
// 战略层 prompt 与第一次战术层 prompt（system + user 全文）追加写入
// docs/actual_prompts.md，用于在 docs 下维护一份"实际发给 LLM 的
// prompt"文档——无需从日志反推，直接留存现场。
//
// 行为约定：
//   - 仅记录 H-01（命名常量，便于日后调整观察对象）
//   - 每个（agent, layer）组合每次进程只落盘一次 = "每次仿真的第一次"
//   - 追加写入（O_APPEND），文档按仿真时间累积，各节带时间戳标题
//   - --prompt-doc="" 可关闭；默认 docs/actual_prompts.md（相对进程
//     工作目录，与 assets/ 等默认路径同一约定——服务器在仓库根目录启动）
//   - 落盘失败只记 warn 日志，绝不影响决策链路

import (
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
	promptDocMu          sync.Mutex
	promptDocPath        string // --prompt-doc flag；空串 = 关闭
	promptDocDumped      = map[string]bool{}
	promptDocInitialized bool
)

// setPromptDocPath 安装文档路径（main 在 flag.Parse 后调用；空串关闭）。
func setPromptDocPath(p string) {
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	promptDocPath = p
}

// dumpPromptDoc 把一次 LLM 调用的 system+user prompt 追加写入文档。
// layer 取 "strategic" / "tactical"。同 layer 每进程只写一次。
func dumpPromptDoc(agentID, layer, system, user string, logger *slog.Logger) {
	if promptDocPath == "" || agentID != promptDocAgent {
		return
	}
	promptDocMu.Lock()
	defer promptDocMu.Unlock()
	if promptDocDumped[layer] {
		return
	}
	promptDocDumped[layer] = true

	layerName := map[string]string{"strategic": "战略层", "tactical": "战术层"}[layer]
	if layerName == "" {
		layerName = layer
	}

	if err := os.MkdirAll(filepath.Dir(promptDocPath), 0o755); err != nil {
		logger.Warn("[prompt-doc] 创建目录失败，跳过落盘", "dir", filepath.Dir(promptDocPath), "err", err)
		return
	}
	f, err := os.OpenFile(promptDocPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warn("[prompt-doc] 打开文档失败，跳过落盘", "path", promptDocPath, "err", err)
		return
	}
	defer f.Close()

	if !promptDocInitialized {
		// 文档首节：说明用途（仅进程内首次写时输出；文件已存在时不重复，
		// 因为 promptDocInitialized 是进程级状态，跨仿真累积）。
		if fi, err := os.Stat(promptDocPath); err != nil || fi.Size() == 0 {
			fmt.Fprintf(f, "# 实际 Prompt 留存\n\n每次仿真自动追加 H-01 的第一次战略层与战术层 prompt（system + user 全文），由 MCP 运行时落盘。\n\n")
		}
		promptDocInitialized = true
	}

	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "## %s 仿真 · H-01 首次%s prompt\n\n", ts, layerName)
	fmt.Fprintf(f, "### System Prompt\n\n```\n%s\n```\n\n", system)
	fmt.Fprintf(f, "### User Prompt\n\n```\n%s\n```\n\n", user)
	logger.Info("[prompt-doc] 已落盘 H-01 首次 prompt",
		"path", promptDocPath, "layer", layerName,
		"system_chars", len(system), "user_chars", len(user))
}
