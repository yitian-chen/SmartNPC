package llmmetrics

import (
	"fmt"
	"strings"
	"time"
)

// Percentiles is a latency percentile pair in milliseconds.
type Percentiles struct {
	P50Ms float64 `json:"p50_ms"`
	P99Ms float64 `json:"p99_ms"`
}

// LayerReport is one decision layer's aggregated metrics.
type LayerReport struct {
	Calls     int64            `json:"calls"`
	E2E       Percentiles      `json:"e2e"`
	TTFT      *Percentiles     `json:"ttft,omitempty"`      // streaming only
	TPOT      *Percentiles     `json:"tpot,omitempty"`      // streaming only
	ITL       *Percentiles     `json:"itl,omitempty"`       // streaming only
	ErrorDist map[string]int64 `json:"error_dist"`          // errClass → count
	RetryRate float64          `json:"retry_rate"`          // calls with ≥1 retry / calls
	RetryAvg  float64          `json:"retry_avg"`           // cumulative retries / calls
	JSONOK    int64            `json:"json_ok"`             // parseToolCalls successes
	JSONFail  int64            `json:"json_fail"`           // parseToolCalls failures
	JSONRate  *float64         `json:"json_rate,omitempty"` // OK/(OK+Fail), nil when no samples
}

// Report is the full aggregated snapshot, ready for JSON encoding or
// markdown rendering.
type Report struct {
	UpdatedAt time.Time               `json:"updated_at"`
	Layers    map[string]*LayerReport `json:"layers"`
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func copyDist(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func optPct(samples []time.Duration) *Percentiles {
	if len(samples) == 0 {
		return nil
	}
	p50, p99 := percentiles(samples)
	return &Percentiles{P50Ms: ms(p50), P99Ms: ms(p99)}
}

// Snapshot returns a point-in-time copy of the aggregated metrics. The
// returned Report shares no mutable state with the Collector.
func (c *Collector) Snapshot() Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	rep := Report{
		UpdatedAt: time.Now(),
		Layers:    map[string]*LayerReport{},
	}
	// Union of all layers that have any recorded data.
	layers := map[string]bool{}
	for l := range c.calls {
		layers[l] = true
	}
	for l := range c.e2e {
		layers[l] = true
	}
	for l := range c.jsonOK {
		layers[l] = true
	}
	for l := range c.jsonFail {
		layers[l] = true
	}

	for layer := range layers {
		lr := &LayerReport{
			Calls:     c.calls[layer],
			E2E:       Percentiles{},
			ErrorDist: copyDist(c.errDist[layer]),
			JSONOK:    c.jsonOK[layer],
			JSONFail:  c.jsonFail[layer],
		}
		if p50, p99 := percentiles(c.e2e[layer]); len(c.e2e[layer]) > 0 {
			lr.E2E = Percentiles{P50Ms: ms(p50), P99Ms: ms(p99)}
		}
		lr.TTFT = optPct(c.ttft[layer])
		lr.TPOT = optPct(c.tpot[layer])
		lr.ITL = optPct(c.itl[layer])
		if lr.Calls > 0 {
			lr.RetryRate = float64(c.retried[layer]) / float64(lr.Calls)
			lr.RetryAvg = float64(c.retries[layer]) / float64(lr.Calls)
		}
		if total := c.jsonOK[layer] + c.jsonFail[layer]; total > 0 {
			rate := float64(c.jsonOK[layer]) / float64(total)
			lr.JSONRate = &rate
		}
		rep.Layers[layer] = lr
	}
	return rep
}

// ToMarkdown renders the report as a markdown document for docs/llm_metrics.md.
// Latency percentiles are in milliseconds; "-" marks metrics with no samples
// (e.g. TTFT/TPOT/ITL before any streaming call).
func (r Report) ToMarkdown() string {
	var sb strings.Builder
	sb.WriteString("# LLM 服务表现指标\n\n")
	sb.WriteString("按决策层聚合的 LLM 调用表现，由 MCP 运行时采集。延迟分位数为毫秒，")
	sb.WriteString("`-` 表示尚无样本（TTFT/TPOT/ITL 仅在 `--tactical-stream` 流式下可得）。\n\n")
	sb.WriteString(fmt.Sprintf("生成时间：%s\n\n", r.UpdatedAt.Format("2006-01-02 15:04:05")))

	sb.WriteString("## 延迟与正确率\n\n")
	sb.WriteString("| 层 | 调用数 | E2E P50 | E2E P99 | TTFT P50 | TTFT P99 | TPOT P50 | TPOT P99 | ITL P50 | ITL P99 | 重试率 | JSON正确率 |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, l := range layerOrder() {
		lr, ok := r.Layers[l]
		if !ok {
			continue
		}
		ttftP50, ttftP99 := pctMs(lr.TTFT)
		tpotP50, tpotP99 := pctMs(lr.TPOT)
		itlP50, itlP99 := pctMs(lr.ITL)
		sb.WriteString(fmt.Sprintf("| %s | %d | %.1f | %.1f | %s | %s | %s | %s | %s | %s | %.1f%% | %s |\n",
			layerDisplay(l), lr.Calls,
			lr.E2E.P50Ms, lr.E2E.P99Ms,
			ttftP50, ttftP99,
			tpotP50, tpotP99,
			itlP50, itlP99,
			lr.RetryRate*100, jsonRateStr(lr)))
	}

	sb.WriteString("\n## 错误类型分布\n\n")
	sb.WriteString("| 层 | success | bad_json_4001 | rate_limited | timeout | http_error | network | other |\n")
	sb.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, l := range layerOrder() {
		lr, ok := r.Layers[l]
		if !ok {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %d | %d |\n",
			layerDisplay(l),
			lr.ErrorDist[ErrSuccess], lr.ErrorDist[ErrBadJSON4001],
			lr.ErrorDist[ErrRateLimited], lr.ErrorDist[ErrTimeout],
			lr.ErrorDist[ErrHTTPError], lr.ErrorDist[ErrNetwork],
			lr.ErrorDist[ErrOther]))
	}
	return sb.String()
}

// pctMs renders a Percentiles pair as two strings ("-"/"-" when nil).
func pctMs(p *Percentiles) (p50, p99 string) {
	if p == nil {
		return "-", "-"
	}
	return fmt.Sprintf("%.1f", p.P50Ms), fmt.Sprintf("%.1f", p.P99Ms)
}

func jsonRateStr(lr *LayerReport) string {
	if lr.JSONRate == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *lr.JSONRate*100)
}

// layerOrder is the canonical display order for the three decision layers.
func layerOrder() []string {
	return []string{"strategic", "tactical", "dialogue"}
}

// layerDisplay maps a layer key to a Chinese display name.
func layerDisplay(layer string) string {
	switch layer {
	case "strategic":
		return "战略层"
	case "tactical":
		return "战术层"
	case "dialogue":
		return "对话层"
	case "unknown":
		return "未知"
	default:
		return layer
	}
}
