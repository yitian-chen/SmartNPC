// Package llmmetrics aggregates LLM call performance samples for the
// three decision layers (strategic / tactical / dialogue). It collects
// end-to-end latency plus — when the caller runs streaming — TTFT, TPOT
// and inter-token latency, and derives P50/P99 percentiles, error-class
// distribution, retry rate and JSON correctness rate. The package is a
// pure in-memory aggregator: no I/O, no logging, no external deps.
package llmmetrics

import (
	"math"
	"sort"
	"time"
)

// percentileSorted computes the p-th percentile (0-100) of a pre-sorted
// slice using the nearest-rank method, matching the existing latency
// scripts' pct() (scripts/venus_latency_test.py:191). The caller must sort
// first; the input is read-only.
func percentileSorted(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := int(math.RoundToEven(p / 100 * float64(len(sorted)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// percentiles returns P50 and P99 for an unsorted slice, copying before
// sorting so the caller's slice is untouched. Empty input → (0, 0).
func percentiles(samples []time.Duration) (p50, p99 time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return percentileSorted(sorted, 50), percentileSorted(sorted, 99)
}
