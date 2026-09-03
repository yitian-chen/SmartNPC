package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/AgentTown/agenttown-mcp/pkg/llmmetrics"
)

// llmMetricsCollector 是进程级 LLM 指标收集器（package-level 供 agenticTurn
// 与 tactical 层埋点，避免长串参数传递）。agenticTurn 每次 LLM 调用后
// RecordCall（延迟/错误分类/重试），generateTacticalPlan 在 parseToolCalls
// 后 RecordJSON（JSON 正确率）。/debug/llm-metrics 读 Snapshot，
// dumpLLMMetrics 落盘 docs/llm_metrics.md。
var llmMetricsCollector = llmmetrics.New()

// llmMetricsDocPath 是 metrics markdown 落盘路径（--llm-metrics-doc flag；
// 空串 = 不落盘）。Go 零值空串使测试路径（不经 main）自然跳过落盘。
var llmMetricsDocPath string

// setLLMMetricsDocPath 安装落盘路径（main 在 flag.Parse 后调用；空串关闭）。
func setLLMMetricsDocPath(p string) {
	llmMetricsDocPath = p
}

// dumpLLMMetrics 把聚合结果格式化为 markdown 写入 llmMetricsDocPath。
// best-effort：落盘失败只记 warn，绝不影响决策链路（与 prompt_doc 同约定）。
func dumpLLMMetrics(logger *slog.Logger) {
	if llmMetricsDocPath == "" {
		return
	}
	md := llmMetricsCollector.Snapshot().ToMarkdown()
	if err := os.MkdirAll(filepath.Dir(llmMetricsDocPath), 0o755); err != nil {
		logger.Warn("[llm-metrics] 创建目录失败，跳过落盘", "dir", filepath.Dir(llmMetricsDocPath), "err", err)
		return
	}
	if err := os.WriteFile(llmMetricsDocPath, []byte(md), 0o644); err != nil {
		logger.Warn("[llm-metrics] 落盘失败，跳过", "path", llmMetricsDocPath, "err", err)
		return
	}
}
