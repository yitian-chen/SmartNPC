package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type benchBudget struct {
	mu        sync.Mutex
	Max, Used int64
}

func (b *benchBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Used >= b.Max {
		return false
	}
	b.Used++
	return true
}
func (b *benchBudget) count() int64 { b.mu.Lock(); defer b.mu.Unlock(); return b.Used }

// RoundTrip interception enforces the budget on actual requests, including
// retries from the production agentic loop. No headers or endpoint are stored.
type benchTransport struct {
	Base    http.RoundTripper
	Budget  *benchBudget
	Offline bool
	Replay  string
	Secrets []string
	mu      sync.Mutex
	Calls   []benchCall
}

func benchRedact(b []byte, secrets []string) []byte {
	var v any
	if json.Unmarshal(b, &v) == nil {
		var walk func(any)
		walk = func(x any) {
			switch y := x.(type) {
			case map[string]any:
				for k, v := range y {
					switch strings.ToLower(k) {
					case "api_key", "authorization", "password", "secret", "access_token":
						y[k] = "[redacted]"
					default:
						walk(v)
					}
				}
			case []any:
				for _, v := range y {
					walk(v)
				}
			}
		}
		walk(v)
		b, _ = json.Marshal(v)
	} else {
		b, _ = json.Marshal(string(b))
	}
	for _, s := range secrets {
		if s != "" {
			b = bytes.ReplaceAll(b, []byte(s), []byte("[redacted]"))
		}
	}
	return b
}
func benchSafeError(e error, secrets []string) string {
	if e == nil {
		return ""
	}
	s := e.Error()
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return regexp.MustCompile(`https?://[^\s"<>]+`).ReplaceAllString(s, "[endpoint]")
}
func (t *benchTransport) snapshot() []benchCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return benchCopy(t.Calls)
}
func (t *benchTransport) append(v benchCall) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Calls = append(t.Calls, v)
}
func (t *benchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	begin := time.Now()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var wire struct {
		Model          string `json:"model"`
		ResponseFormat struct {
			JSONSchema struct {
				Name string `json:"name"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	_ = json.Unmarshal(body, &wire)
	call := benchCall{Model: wire.Model, Request: benchRedact(body, t.Secrets)}
	if !t.Offline && !t.Budget.take() {
		call.Error = "HTTP call budget exhausted"
		call.Budget = true
		t.append(call)
		return nil, fmt.Errorf("memory bench HTTP call budget exhausted")
	}
	var resp *http.Response
	if t.Offline {
		content := `{"summary":"完成了日常活动。","episodes":[],"people_updates":[]}`
		message := map[string]any{"role": "assistant"}
		switch wire.ResponseFormat.JSONSchema.Name {
		case "conversation_commitments":
			content = t.Replay
			if content == "" {
				content = `{"new_commitments":[],"commitment_changes":[]}`
			}
			message["content"] = content
		case "compact_extract":
			message["content"] = content
		default:
			message["tool_calls"] = []any{
				map[string]any{"id": "bench_speak", "type": "function", "function": map[string]any{"name": "speak", "arguments": `{"content":"先休息一下。"}`}},
				map[string]any{"id": "bench_rest", "type": "function", "function": map[string]any{"name": "InteractSmartObject", "arguments": `{"semantic_group":"sleep_pod","interaction":"sleep","zone":"residential_quarters","duration":900}`}},
			}
		}
		b, _ := json.Marshal(map[string]any{"id": "bench-replay", "model": "offline-replay", "choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
		resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(b)), Request: req}
	} else {
		base := t.Base
		if base == nil {
			base = http.DefaultTransport
		}
		resp, err = base.RoundTrip(req)
	}
	call.DurationMS = time.Since(begin).Milliseconds()
	if err != nil {
		call.Error = benchSafeError(err, t.Secrets)
		t.append(call)
		return nil, fmt.Errorf("model transport failed: %s", call.Error)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	call.Status = resp.StatusCode
	call.Response = benchRedact(raw, t.Secrets)
	var usage struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &usage)
	call.InputTokens = usage.Usage.Prompt
	call.OutputTokens = usage.Usage.Completion
	if readErr != nil {
		call.Error = "response body read failed"
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		call.Error = fmt.Sprintf("model HTTP status %d", resp.StatusCode)
	}
	t.append(call)
	return resp, readErr
}

func benchCallProblem(calls []benchCall) string {
	for _, c := range calls {
		if c.Budget {
			return "budget_stopped"
		}
	}
	for _, c := range calls {
		if c.Error != "" || c.Status < 200 || c.Status >= 300 {
			return "service_error"
		}
	}
	return ""
}
