// Package venus provides an HTTP client for the Venus LLM proxy's
// Anthropic-compatible /v1/messages endpoint. It is a drop-in alternative
// to pkg/hermes for the strategic and tactical layers, returning the same
// *hermes.Response shape so callers (generateDailyPlan, generateTacticalPlan,
// generateTacticalPlanStreaming) need no changes beyond accepting an interface.
//
// Unlike hermes.Client, venus.Client does not maintain a session chain
// (previous_response_id): each call is independent. This matches current
// usage where both strategic and tactical layers call ResetSession()
// immediately after every Send, so no cross-call state is required.
//
// The Venus proxy speaks the Anthropic Messages API protocol:
//
//	POST {BaseURL}/v1/messages
//	  x-api-key: {APIKey}
//	  anthropic-version: 2023-06-01
//	  Content-Type: application/json
//	  body: {"model":..., "max_tokens":..., "messages":[{"role":"user","content":...}]}
//
// Streaming adds "stream":true and Accept: text/event-stream; the SSE event
// "content_block_delta" carries text deltas, "message_stop" terminates.
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

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
)

const (
	// anthropicVersion is the required header value for the Messages API.
	anthropicVersion = "2023-06-01"

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

// Client POSTs prompts to the Venus /v1/messages endpoint.
//
// All calls are serialized via sendMu — same contract as hermes.Client,
// so callers that swap between backends do not see concurrency behavior
// changes.
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

// SendWithSummary POSTs input to Venus and returns the response.
//
// The summary parameter is accepted for hermes.Client signature compatibility
// but is currently unused: both strategic and tactical layers pass "" and
// rely on independent per-call sessions (no cross-call state to preserve).
func (c *Client) SendWithSummary(ctx context.Context, input, summary string) (*hermes.Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = summary // unused (see doc comment)
	return c.doSend(ctx, input, false, nil)
}

// SendStreaming POSTs input with stream:true and invokes onDelta for each
// text delta received. It blocks until the stream terminates (message_stop
// or error) and returns the final Response assembled from the accumulated
// deltas.
func (c *Client) SendStreaming(ctx context.Context, input string, onDelta func(delta string)) (*hermes.Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.doSend(ctx, input, true, onDelta)
}

// ResetSession is a no-op for venus.Client. Venus has no session chain
// (each /v1/messages call is independent), but the method is required to
// satisfy the llmClient interface shared with hermes.Client.
func (c *Client) ResetSession() {
	// Intentionally empty.
}

// doSend performs the HTTP POST and parses the response. For streaming
// requests, onDelta is invoked for each text delta. Caller MUST hold sendMu.
func (c *Client) doSend(ctx context.Context, input string, stream bool, onDelta func(string)) (*hermes.Response, error) {
	body := request{
		Model:     c.cfg.Model,
		MaxTokens: c.cfg.MaxTokens,
		Messages: []message{
			{Role: "user", Content: input},
		},
		Stream: stream,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	// Venus 网关用 Bearer 认证（对应 Claude Code 的 ANTHROPIC_AUTH_TOKEN 方式），
	// 而非 Anthropic 原生的 x-api-key。同时保留 x-api-key 以兼容真实 Anthropic API。
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("x-api-key", c.cfg.APIKey)
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

// parseResponse decodes a non-streaming Anthropic Messages API response and
// converts it to *hermes.Response.
func (c *Client) parseResponse(r io.Reader) (*hermes.Response, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return ar.toHermes(), nil
}

// parseStream decodes an SSE stream from the Anthropic Messages API.
//
// Relevant SSE events:
//
//	event: message_start
//	data: {"type":"message_start","message":{"id":...,"model":...,"usage":{...}}}
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
//
//	event: message_delta
//	data: {"type":"message_delta","delta":{...},"usage":{"output_tokens":...}}
//
//	event: message_stop
//	data: {"type":"message_stop"}
func (c *Client) parseStream(r io.Reader, onDelta func(string)) (*hermes.Response, error) {
	sc := bufio.NewScanner(r)
	// SSE events can carry a large message_start payload; allow up to 1MB per line.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		eventType string
		dataBuf   strings.Builder
		hr        hermes.Response
		// Accumulated text from content_block_delta events.
		textBuf strings.Builder
		// Token counts assembled across message_start (input) and message_delta (output).
		inputTokens  int
		outputTokens int
	)

	for sc.Scan() {
		line := sc.Text()

		// Blank line = event boundary → dispatch accumulated event.
		if line == "" {
			if eventType != "" && dataBuf.Len() > 0 {
				terminal, err := c.handleSSEEvent(eventType, dataBuf.String(), onDelta, &hr, &textBuf, &inputTokens, &outputTokens)
				if err != nil {
					return nil, err
				}
				if terminal {
					c.finalizeStream(&hr, &textBuf, inputTokens, outputTokens)
					return &hr, nil
				}
			}
			eventType = ""
			dataBuf.Reset()
			continue
		}

		// Comment / keepalive line (starts with ':').
		if line[0] == ':' {
			continue
		}

		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ") // SSE spec: optional single leading space
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(data)
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sse read: %w", err)
	}

	// Stream ended without a message_stop terminal event.
	c.finalizeStream(&hr, &textBuf, inputTokens, outputTokens)
	if hr.ID == "" && textBuf.Len() == 0 {
		return nil, fmt.Errorf("sse stream ended without terminal event: %w", io.ErrUnexpectedEOF)
	}
	return &hr, nil
}

// handleSSEEvent dispatches one accumulated SSE event. Returns terminal=true
// when the event is message_stop. For content_block_delta events, onDelta
// is invoked with the text delta.
func (c *Client) handleSSEEvent(eventType, data string, onDelta func(string), hr *hermes.Response, textBuf *strings.Builder, inputTokens, outputTokens *int) (bool, error) {
	switch eventType {
	case "message_start":
		var ev struct {
			Message struct {
				ID      string `json:"id"`
				Model   string `json:"model"`
				Usage   struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			c.log.Debug("sse message_start parse failed", "err", err, "data", truncate(data, 200))
			return false, nil
		}
		hr.ID = ev.Message.ID
		hr.Model = ev.Message.Model
		*inputTokens = ev.Message.Usage.InputTokens

	case "content_block_delta":
		var ev struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			c.log.Debug("sse content_block_delta parse failed", "err", err, "data", truncate(data, 200))
			return false, nil
		}
		if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			textBuf.WriteString(ev.Delta.Text)
			if onDelta != nil {
				onDelta(ev.Delta.Text)
			}
		}

	case "message_delta":
		var ev struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			c.log.Debug("sse message_delta parse failed", "err", err, "data", truncate(data, 200))
			return false, nil
		}
		*outputTokens = ev.Usage.OutputTokens

	case "message_stop":
		return true, nil

	case "error":
		var ev struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(data), &ev)
		msg := ev.Error.Message
		if msg == "" {
			msg = "unknown stream error"
		}
		return false, fmt.Errorf("venus stream error: %s", msg)
	}
	return false, nil
}

// finalizeStream populates the hermes.Response fields that are assembled
// from accumulated stream state rather than a single event.
func (c *Client) finalizeStream(hr *hermes.Response, textBuf *strings.Builder, inputTokens, outputTokens int) {
	hr.Status = "completed"
	if hr.Model == "" {
		hr.Model = c.cfg.Model
	}
	hr.Output = []hermes.Block{{
		Type: "message",
		Role: "assistant",
		Content: []hermes.Content{{
			Type: "output_text",
			Text: textBuf.String(),
		}},
	}}
	hr.Usage = hermes.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
}

// truncate returns s truncated to maxLen characters with "..." appended.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// request is the body sent to /v1/messages.
type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
}

// message is one entry in the Messages array.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the (subset of the) Anthropic Messages API response.
type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// toHermes converts the Anthropic response to *hermes.Response so callers
// can use ExtractText() and Usage.TotalTokens unchanged.
func (ar *anthropicResponse) toHermes() *hermes.Response {
	var text string
	for _, b := range ar.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return &hermes.Response{
		ID:     ar.ID,
		Status: "completed",
		Model:  ar.Model,
		Output: []hermes.Block{{
			Type: "message",
			Role: "assistant",
			Content: []hermes.Content{{
				Type: "output_text",
				Text: text,
			}},
		}},
		Usage: hermes.Usage{
			InputTokens:  ar.Usage.InputTokens,
			OutputTokens: ar.Usage.OutputTokens,
			TotalTokens:  ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
}
