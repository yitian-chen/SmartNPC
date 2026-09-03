package llmmetrics

import (
	"strings"
	"testing"
	"time"
)

func TestPercentileSorted(t *testing.T) {
	// 11 个样本 0..10ms：最近秩法 p50 → index round(0.5*10)=5 → 5ms；
	// p99 → index round(0.99*10)=10 → 10ms。
	samples := make([]time.Duration, 11)
	for i := range samples {
		samples[i] = time.Duration(i) * time.Millisecond
	}
	if got := percentileSorted(samples, 50); got != 5*time.Millisecond {
		t.Errorf("p50 = %v, want 5ms", got)
	}
	if got := percentileSorted(samples, 99); got != 10*time.Millisecond {
		t.Errorf("p99 = %v, want 10ms", got)
	}
	// 边界：空切片、p≤0、p≥100。
	if got := percentileSorted(nil, 50); got != 0 {
		t.Errorf("empty p50 = %v, want 0", got)
	}
	if got := percentileSorted(samples, 0); got != 0 {
		t.Errorf("p0 = %v, want 0", got)
	}
	if got := percentileSorted(samples, 100); got != 10*time.Millisecond {
		t.Errorf("p100 = %v, want 10ms", got)
	}
}

func TestPercentiles(t *testing.T) {
	samples := []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	p50, p99 := percentiles(samples)
	if p50 != 20*time.Millisecond {
		t.Errorf("p50 = %v, want 20ms", p50)
	}
	if p99 != 30*time.Millisecond {
		t.Errorf("p99 = %v, want 30ms", p99)
	}
	// 输入切片不被排序污染。
	if samples[0] != 30*time.Millisecond {
		t.Errorf("input mutated: samples[0] = %v", samples[0])
	}
	if _, p99 := percentiles(nil); p99 != 0 {
		t.Errorf("empty percentiles p99 = %v, want 0", p99)
	}
}

func TestCollector_RecordCallAggregates(t *testing.T) {
	c := New()
	c.RecordCall(CallSample{Layer: "tactical", E2E: 100 * time.Millisecond, ErrClass: ErrSuccess})
	c.RecordCall(CallSample{Layer: "tactical", E2E: 300 * time.Millisecond, ErrClass: ErrBadJSON4001, Retried: true, RetryCount: 2})
	c.RecordCall(CallSample{Layer: "strategic", E2E: 200 * time.Millisecond})

	rep := c.Snapshot()
	if rep.Layers["tactical"].Calls != 2 {
		t.Errorf("tactical calls = %d, want 2", rep.Layers["tactical"].Calls)
	}
	if rep.Layers["tactical"].E2E.P50Ms != 100 || rep.Layers["tactical"].E2E.P99Ms != 300 {
		t.Errorf("tactical E2E = %+v, want P50=100 P99=300", rep.Layers["tactical"].E2E)
	}
	if rep.Layers["tactical"].ErrorDist[ErrSuccess] != 1 || rep.Layers["tactical"].ErrorDist[ErrBadJSON4001] != 1 {
		t.Errorf("tactical errDist = %+v", rep.Layers["tactical"].ErrorDist)
	}
	// 重试率 = 1/2，平均重试 = 2/2 = 1。
	if rep.Layers["tactical"].RetryRate != 0.5 || rep.Layers["tactical"].RetryAvg != 1.0 {
		t.Errorf("tactical retry = rate %v avg %v, want 0.5/1.0", rep.Layers["tactical"].RetryRate, rep.Layers["tactical"].RetryAvg)
	}
	if rep.Layers["strategic"].Calls != 1 {
		t.Errorf("strategic calls = %d, want 1", rep.Layers["strategic"].Calls)
	}
}

func TestCollector_StreamingMetricsOnlyWhenPresent(t *testing.T) {
	c := New()
	// 非流式：TTFT/TPOT/ITL 为零/空 → 不产生流式样本。
	c.RecordCall(CallSample{Layer: "tactical", E2E: 100 * time.Millisecond})
	rep := c.Snapshot()
	if rep.Layers["tactical"].TTFT != nil || rep.Layers["tactical"].TPOT != nil || rep.Layers["tactical"].ITL != nil {
		t.Errorf("non-streaming should not record TTFT/TPOT/ITL: %+v", rep.Layers["tactical"])
	}
	// 流式：TTFT 与 ITL 样本被聚合。
	c.RecordCall(CallSample{
		Layer: "tactical", E2E: 500 * time.Millisecond,
		TTFT: 80 * time.Millisecond, TPOT: 30 * time.Millisecond,
		ITLs: []time.Duration{20 * time.Millisecond, 40 * time.Millisecond},
	})
	rep = c.Snapshot()
	if rep.Layers["tactical"].TTFT == nil || rep.Layers["tactical"].TTFT.P50Ms != 80 {
		t.Errorf("TTFT = %+v, want P50=80ms", rep.Layers["tactical"].TTFT)
	}
	if rep.Layers["tactical"].ITL == nil || rep.Layers["tactical"].ITL.P99Ms != 40 {
		t.Errorf("ITL = %+v, want P99=40ms", rep.Layers["tactical"].ITL)
	}
}

func TestCollector_JSONRate(t *testing.T) {
	c := New()
	c.RecordJSON("tactical", true)
	c.RecordJSON("tactical", true)
	c.RecordJSON("tactical", false)
	rep := c.Snapshot()
	lr := rep.Layers["tactical"]
	if lr.JSONOK != 2 || lr.JSONFail != 1 {
		t.Errorf("json ok/fail = %d/%d, want 2/1", lr.JSONOK, lr.JSONFail)
	}
	if lr.JSONRate == nil || *lr.JSONRate != 2.0/3.0 {
		t.Errorf("json rate = %v, want 2/3", lr.JSONRate)
	}
}

func TestReport_ToMarkdown(t *testing.T) {
	c := New()
	c.RecordCall(CallSample{Layer: "tactical", E2E: 100 * time.Millisecond, ErrClass: ErrSuccess})
	c.RecordJSON("tactical", true)
	md := c.Snapshot().ToMarkdown()
	for _, want := range []string{"# LLM 服务表现指标", "战术层", "E2E P50", "错误类型分布", "100.0"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}
