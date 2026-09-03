package llmmetrics

import (
	"sync"
	"time"
)

// Error classes recorded per LLM call. ErrClass is set by the caller from
// the call's final error (reusing the tactical layer's isVenusErrorCode /
// isRateLimited classification, plus a timeout discriminator).
const (
	ErrSuccess     = "success"
	ErrBadJSON4001 = "bad_json_4001" // venus rejected malformed tool_calls JSON
	ErrRateLimited = "rate_limited"  // 429 / code 4029
	ErrTimeout     = "timeout"       // context deadline exceeded / client timeout
	ErrHTTPError   = "http_error"    // non-200 status that isn't 4001/429
	ErrNetwork     = "network"       // connection refused / reset / dial error
	ErrOther       = "other"
)

// CallSample is one LLM call's measured metrics. Zero-valued TTFT/TPOT and
// an empty ITLs slice mean the call was non-streaming (those metrics are
// only obtainable from a stream); they are simply skipped, not recorded as 0.
type CallSample struct {
	Layer        string          // "strategic" | "tactical" | "dialogue"
	E2E          time.Duration   // wall-clock from send start to parse finish
	TTFT         time.Duration   // time to first token (streaming only)
	TPOT         time.Duration   // time per output token (streaming only)
	ITLs         []time.Duration // inter-token latencies (streaming only)
	OutputTokens int             // completion tokens (informational)
	ErrClass     string          // one of the Err* constants; "" → ErrSuccess
	Retried      bool            // at least one retry attempt occurred
	RetryCount   int             // total retry attempts (0 = none)
}

// Collector aggregates samples per layer. All methods are safe for
// concurrent use; it holds unbounded in-memory slices (bounded in practice
// by the low call rate of the decision loop — tens of calls per game day).
type Collector struct {
	mu sync.Mutex

	e2e      map[string][]time.Duration
	ttft     map[string][]time.Duration
	tpot     map[string][]time.Duration
	itl      map[string][]time.Duration
	calls    map[string]int64
	errDist  map[string]map[string]int64
	retried  map[string]int64 // calls that had ≥1 retry
	retries  map[string]int64 // cumulative retry attempts
	jsonOK   map[string]int64
	jsonFail map[string]int64
}

// New returns an empty Collector.
func New() *Collector {
	return &Collector{
		e2e:      map[string][]time.Duration{},
		ttft:     map[string][]time.Duration{},
		tpot:     map[string][]time.Duration{},
		itl:      map[string][]time.Duration{},
		calls:    map[string]int64{},
		errDist:  map[string]map[string]int64{},
		retried:  map[string]int64{},
		retries:  map[string]int64{},
		jsonOK:   map[string]int64{},
		jsonFail: map[string]int64{},
	}
}

func defaultLayer(layer string) string {
	if layer == "" {
		return "unknown"
	}
	return layer
}

// RecordCall appends one call sample under its layer. It increments the
// call count and error-class distribution unconditionally; latency samples
// are only appended when non-zero (a zero TTFT/TPOT or empty ITLs from a
// non-streaming call is skipped, not recorded as a 0ms sample).
func (c *Collector) RecordCall(s CallSample) {
	layer := defaultLayer(s.Layer)
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls[layer]++
	if s.E2E > 0 {
		c.e2e[layer] = append(c.e2e[layer], s.E2E)
	}
	if s.TTFT > 0 {
		c.ttft[layer] = append(c.ttft[layer], s.TTFT)
	}
	if s.TPOT > 0 {
		c.tpot[layer] = append(c.tpot[layer], s.TPOT)
	}
	if len(s.ITLs) > 0 {
		c.itl[layer] = append(c.itl[layer], s.ITLs...)
	}
	if s.ErrClass == "" {
		s.ErrClass = ErrSuccess
	}
	if c.errDist[layer] == nil {
		c.errDist[layer] = map[string]int64{}
	}
	c.errDist[layer][s.ErrClass]++
	if s.Retried {
		c.retried[layer]++
	}
	c.retries[layer] += int64(s.RetryCount)
}

// RecordJSON records one JSON-parse outcome for a layer (used by the
// tactical layer to report whether parseToolCalls succeeded). ok=true means
// the LLM output parsed into valid actions.
func (c *Collector) RecordJSON(layer string, ok bool) {
	layer = defaultLayer(layer)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.jsonOK[layer]++
	} else {
		c.jsonFail[layer]++
	}
}
