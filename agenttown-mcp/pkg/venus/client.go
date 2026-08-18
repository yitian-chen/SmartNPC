// Package venus provides an HTTP client for the Venus LLM proxy's
// OpenAI-compatible /v1/chat/completions endpoint. It is the strategic
// and tactical layer LLM backend, returning the shared *llmtypes.Response
// shape so callers (generateDailyPlan, generateTacticalPlan,
// generateTacticalPlanStreaming) need no changes beyond accepting an
// interface.
//
// venus.Client does not maintain a session chain (previous_response_id):
// each call is independent. This matches current usage where both
// strategic and tactical layers call ResetSession() immediately after
// every Send, so no cross-call state is required.
//
// The Venus proxy speaks the OpenAI Chat Completions API protocol:
//
//	POST {BaseURL}/v1/chat/completions
//	  Authorization: Bearer {APIKey}
//	  Venus-Sticky-Routing: token
//	  Content-Type: application/json
//	  body: {"model":..., "max_tokens":..., "messages":[{"role":"system",...},{"role":"user",...}]}
//
// Streaming adds "stream":true; the SSE response is a sequence of
// "data: {json chunk}" lines terminated by "data: [DONE]".
package venus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
)

const (
	defaultHTTPTimeout = 60 * time.Second
	defaultMaxTokens   = 4096
)

// Config configures the Client.
type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	Logger    *slog.Logger
	Timeout   time.Duration // 0 = defaultHTTPTimeout
	MaxTokens int           // 0 = defaultMaxTokens
}

// Client POSTs prompts to the Venus /v1/chat/completions endpoint.
//
// All calls are serialized via sendMu — same contract as a session-chain
// backend, so callers that swap between backends do not see concurrency
// behavior changes.
type Client struct {
	cfg    Config
	http   *http.Client
	log    *slog.Logger
	sendMu sync.Mutex
}

// New creates a Client.
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultHTTPTimeout
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultMaxTokens
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
		log:  cfg.Logger,
	}
}

// SendWithSummary POSTs a (system, user) message pair to Venus and returns
// the response. system carries mechanism/instruction text (rules, output
// format); user carries the per-call context/data. system == "" sends a
// single user message (backward compatible).
func (c *Client) SendWithSummary(ctx context.Context, system, user string) (*llmtypes.Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.doSend(ctx, system, user, false, nil, nil)
}

// SendStreaming POSTs a (system, user) message pair with stream:true and
// invokes onDelta for each text delta received. It blocks until the stream
// terminates (data: [DONE] or error) and returns the final Response
// assembled from the accumulated deltas.
func (c *Client) SendStreaming(ctx context.Context, system, user string, onDelta func(delta string)) (*llmtypes.Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.doSend(ctx, system, user, true, onDelta, nil)
}

// SendWithSchema POSTs a (system, user) message pair with OpenAI
// Structured Outputs constraints (response_format json_schema, strict).
// The model is constrained at decoding time to emit only JSON matching
// schema — field-type violations (e.g. a nested object where a string is
// required) become impossible instead of merely discouraged.
// schemaName labels the schema for the gateway; schema is the raw JSON
// Schema document (root may be any type). Gateways that ignore
// response_format still accept the request — the schema is best-effort.
func (c *Client) SendWithSchema(ctx context.Context, system, user, schemaName string, schema []byte) (*llmtypes.Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	def := &JSONSchemaDef{Name: schemaName, Strict: true, Schema: json.RawMessage(schema)}
	return c.doSend(ctx, system, user, false, nil, def)
}

// ResetSession is a no-op for venus.Client. Venus has no session chain
// (each /v1/chat/completions call is independent), but the method is
// required to satisfy the llmClient interface shared with backends that
// do maintain session state.
func (c *Client) ResetSession() {
	// Intentionally empty.
}

// doSend performs the HTTP POST and parses the response. For streaming
// requests, onDelta is invoked for each text delta. A non-nil schema adds
// response_format (Structured Outputs) to the request body. Caller MUST
// hold sendMu.
func (c *Client) doSend(ctx context.Context, system, user string, stream bool, onDelta func(string), schema *JSONSchemaDef) (*llmtypes.Response, error) {
	msgs := make([]message, 0, 2)
	if system != "" {
		msgs = append(msgs, message{Role: "system", Content: system})
	}
	msgs = append(msgs, message{Role: "user", Content: user})
	body := request{
		Model:     c.cfg.Model,
		MaxTokens: c.cfg.MaxTokens,
		Messages:  msgs,
		Stream:    stream,
	}
	if schema != nil {
		body.ResponseFormat = &ResponseFormat{Type: "json_schema", JSONSchema: schema}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Venus 网关用 Bearer 认证（对应 Claude Code 的 ANTHROPIC_AUTH_TOKEN 方式）。
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	// Venus 自定义头：会话粘性路由（sticky routing），值固定为 "token"。
	// 来自 Venus 平台示例配置的 ANTHROPIC_CUSTOM_HEADERS。
	req.Header.Set("Venus-Sticky-Routing", "token")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("venus status %d: %s", resp.StatusCode, string(raw))
	}

	if stream {
		return c.parseStream(resp.Body, onDelta)
	}
	return c.parseResponse(resp.Body)
}

// parseResponse decodes a non-streaming OpenAI Chat Completions response
// and converts it to *llmtypes.Response.
func (c *Client) parseResponse(r io.Reader) (*llmtypes.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var or openaiResponse
	if err := json.Unmarshal(raw, &or); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return or.toLlmTypes(c.cfg.Model), nil
}

// parseStream decodes an SSE stream from the OpenAI Chat Completions API.
//
// OpenAI SSE format (no event: lines, only data: lines):
//
//	data: {"id":"...","choices":[{"delta":{"content":"文本"}}]}
//	data: {"id":"...","choices":[{"delta":{},"finish_reason":"stop"}]}
//	data: [DONE]
//
// Comment / keepalive lines start with ':'.
func (c *Client) parseStream(r io.Reader, onDelta func(string)) (*llmtypes.Response, error) {
	sc := bufio.NewScanner(r)
	// SSE events can carry a large chunk payload; allow up to 1MB per line.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		hr      llmtypes.Response
		textBuf strings.Builder
		respID  string
		usage   *openaiUsage
		gotDone bool
	)

	for sc.Scan() {
		line := sc.Text()

		// Blank line = event boundary (OpenAI doesn't use it but tolerate).
		if line == "" {
			continue
		}

		// Comment / keepalive line (starts with ':').
		if line[0] == ':' {
			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimPrefix(data, " ") // SSE spec: optional single leading space

		// Terminal marker.
		if data == "[DONE]" {
			gotDone = true
			break
		}

		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			c.log.Debug("sse chunk parse failed", "err", err, "data", truncate(data, 200))
			continue
		}
		if chunk.ID != "" {
			respID = chunk.ID
		}
		// Extract text delta from first choice.
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				textBuf.WriteString(delta)
				if onDelta != nil {
					onDelta(delta)
				}
			}
		}
		// Usage may appear in the final chunk (if stream_options.include_usage=true).
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sse read: %w", err)
	}

	// Stream ended without [DONE] but with content — graceful degradation.
	if !gotDone && textBuf.Len() == 0 && respID == "" {
		return nil, fmt.Errorf("sse stream ended without terminal event: %w", io.ErrUnexpectedEOF)
	}

	c.finalizeStream(&hr, &textBuf, respID, usage)
	return &hr, nil
}

// finalizeStream populates the llmtypes.Response fields from accumulated
// stream state. If usage is nil (stream_options not supported), token
// counts are left as zero — they're only used for logging, not for
// session reset logic.
func (c *Client) finalizeStream(hr *llmtypes.Response, textBuf *strings.Builder, respID string, usage *openaiUsage) {
	hr.ID = respID
	hr.Status = "completed"
	hr.Model = c.cfg.Model
	hr.Output = []llmtypes.Block{{
		Type: "message",
		Role: "assistant",
		Content: []llmtypes.Content{{
			Type: "output_text",
			Text: textBuf.String(),
		}},
	}}
	if usage != nil {
		hr.Usage = llmtypes.Usage{
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
			TotalTokens:  usage.TotalTokens,
		}
	}
}

// truncate returns s truncated to maxLen characters with "..." appended.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// request is the body sent to /v1/chat/completions.
type request struct {
	Model          string          `json:"model"`
	MaxTokens      int             `json:"max_tokens"`
	Messages       []message       `json:"messages"`
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ResponseFormat is the OpenAI response_format field (Structured Outputs).
// Type "json_schema" with a strict JSONSchemaDef constrains decoding so the
// output is guaranteed to match the schema.
type ResponseFormat struct {
	Type       string         `json:"type"` // "json_schema"
	JSONSchema *JSONSchemaDef `json:"json_schema,omitempty"`
}

// JSONSchemaDef is the named strict JSON Schema sent inside response_format.
type JSONSchemaDef struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// message is one entry in the Messages array.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiResponse is the (subset of the) OpenAI Chat Completions response.
type openaiResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Message      openaiMessage `json:"message"`
	Delta        openaiMessage `json:"delta,omitempty"`
	FinishReason string        `json:"finish_reason"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// openaiChunk is one SSE chunk in a streaming response.
type openaiChunk struct {
	ID      string         `json:"id"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage,omitempty"`
}

// toLlmTypes converts the OpenAI response to *llmtypes.Response so callers
// can use ExtractText() and Usage.TotalTokens unchanged.
// modelFallback is used when the response Model is empty or "default"
// (Venus returns "default" rather than the actual model name).
func (or *openaiResponse) toLlmTypes(modelFallback string) *llmtypes.Response {
	var text string
	for _, ch := range or.Choices {
		text += ch.Message.Content
	}
	model := or.Model
	if model == "" || model == "default" {
		model = modelFallback
	}
	return &llmtypes.Response{
		ID:     or.ID,
		Status: "completed",
		Model:  model,
		Output: []llmtypes.Block{{
			Type: "message",
			Role: "assistant",
			Content: []llmtypes.Content{{
				Type: "output_text",
				Text: text,
			}},
		}},
		Usage: llmtypes.Usage{
			InputTokens:  or.Usage.PromptTokens,
			OutputTokens: or.Usage.CompletionTokens,
			TotalTokens:  or.Usage.TotalTokens,
		},
	}
}
