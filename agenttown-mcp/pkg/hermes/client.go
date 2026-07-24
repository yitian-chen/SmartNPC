// Package hermes provides an HTTP client for the Hermes Gateway
// /v1/responses endpoint. It maintains a per-game-day session via the
// `previous_response_id` field, with automatic summarization + reset
// when the token count exceeds a configurable threshold.
//
// All Send calls are serialized via a mutex — no two Hermes calls
// can be in flight simultaneously. This prevents race conditions
// where concurrent calls chain to the same prevResponseID and
// defeat the session-reset mechanism.
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultTokenThreshold is the point at which the client auto-summarizes
// and resets the session.
const DefaultTokenThreshold = 80000

// ErrUpstreamError indicates Hermes returned a response that wraps an
// upstream LLM API error (e.g., HTTP 400 from the model provider). The
// response body is HTTP 200 but the narrative contains the error text
// and token usage is zero. The session is automatically reset to break
// the corrupted conversation chain.
var ErrUpstreamError = errors.New("hermes upstream error")

// Config configures the Client.
type Config struct {
	URL    string
	APIKey string
	Model  string
	Logger *slog.Logger

	// TokenThreshold: when TotalTokens exceeds this, the client
	// auto-summarizes and resets. 0 = use DefaultTokenThreshold.
	TokenThreshold int
}

// Client POSTs perception text to Hermes /v1/responses.
//
// All calls are serialized via sendMu — no two Hermes requests can
// be in flight at the same time. This is critical because concurrent
// calls would chain to the same prevResponseID, causing token
// explosion and defeating session resets.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	// sendMu serializes the entire Send → doSend cycle. Held for the full
	// Hermes round-trip so previous_response_id cannot race.
	sendMu         sync.Mutex
	prevResponseID string
	pendingSummary string
	turnCount      int
}

// New creates a Client.
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.TokenThreshold <= 0 {
		cfg.TokenThreshold = DefaultTokenThreshold
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 120 * time.Second},
		log:  cfg.Logger,
	}
}

// Send posts input without a caller-provided local summary. Production
// decision workers should use SendWithSummary so token resets preserve the
// latest authoritative world state without another LLM request.
func (c *Client) Send(ctx context.Context, input string) (*Response, error) {
	return c.SendWithSummary(ctx, input, "")
}

// SendWithSummary POSTs input to Hermes and uses localSummary if the token
// threshold requires a session reset. The reset is local and immediate: it
// never makes a second LLM request, so the current response is not delayed by
// summarization and assistant narratives cannot leak into authoritative state.
func (c *Client) SendWithSummary(ctx context.Context, input, localSummary string) (*Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fullInput := input
	summary := c.pendingSummary
	c.pendingSummary = ""
	c.turnCount++
	if summary != "" {
		fullInput = "[本地状态摘要] " + summary + "\n\n" + input
	}

	resp, err := c.doSend(ctx, fullInput)
	if err != nil {
		return resp, err
	}

	if resp.Usage.TotalTokens > c.cfg.TokenThreshold {
		c.prevResponseID = ""
		c.pendingSummary = localSummary
		c.turnCount = 0
		c.log.Info("token threshold exceeded, session reset with local structured summary",
			"tokens", resp.Usage.TotalTokens,
			"threshold", c.cfg.TokenThreshold,
			"summary_bytes", len(localSummary),
		)
	}
	return resp, nil
}

// SendStreaming POSTs input with stream:true and invokes onDelta for each
// text delta received. It blocks until the stream terminates (response.completed
// or response.failed) and returns the final Response.
//
// sendMu is held for the entire streaming cycle — same serialization as
// SendWithSummary, so previous_response_id cannot race. Token-threshold reset
// is applied after response.completed; no localSummary is carried because the
// streaming path is used by the tactical layer, which resets the session
// immediately after each call anyway.
func (c *Client) SendStreaming(ctx context.Context, input string, onDelta func(delta string)) (*Response, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fullInput := input
	if summary := c.pendingSummary; summary != "" {
		c.pendingSummary = ""
		fullInput = "[本地状态摘要] " + summary + "\n\n" + input
	}
	c.turnCount++

	resp, err := c.doSendStreaming(ctx, fullInput, onDelta)
	if err != nil {
		return resp, err
	}

	if resp.Usage.TotalTokens > c.cfg.TokenThreshold {
		c.prevResponseID = ""
		c.pendingSummary = ""
		c.turnCount = 0
		c.log.Info("token threshold exceeded, session reset (streaming)",
			"tokens", resp.Usage.TotalTokens,
			"threshold", c.cfg.TokenThreshold,
		)
	}
	return resp, nil
}

// doSend performs the HTTP POST and updates prevResponseID.
// Caller MUST hold sendMu.
func (c *Client) doSend(ctx context.Context, input string) (*Response, error) {
	prev := c.prevResponseID

	body := request{
		Model:              c.cfg.Model,
		Input:              input,
		PreviousResponseID: prev,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Derive the request timeout from the caller's context so unregistering an
	// agent can cancel its in-flight Hermes request immediately.
	postCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	url := strings.TrimRight(c.cfg.URL, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	// The perception text is logged at main.go:498 as
	// [MCP→Hermes/PERCEPTION] with agent_epoch/decision_epoch context.
	// Don't duplicate here.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hermes status %d: %s", resp.StatusCode, string(raw))
	}

	var hr Response
	if err := json.Unmarshal(raw, &hr); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Detect upstream LLM errors wrapped in a 200 response. Hermes catches
	// upstream API errors (e.g., HTTP 400 "Invalid 'tool_calls': empty
	// array") and returns them as 200 with the error text as the narrative
	// and zero token usage. If we chain to such a response, every subsequent
	// call inherits the corrupted conversation history and keeps failing.
	if isUpstreamError(&hr) {
		narrative := hr.ExtractText()
		c.log.Warn("[Hermes→MCP] upstream error detected, resetting session",
			"narrative", truncate(narrative, 200),
			"prev_id", c.prevResponseID)
		c.prevResponseID = "" // break the chain
		c.pendingSummary = "" // don't carry error text into the next turn
		c.turnCount = 0
		return &hr, ErrUpstreamError
	}

	if hr.ID != "" {
		c.prevResponseID = hr.ID
	}

	// The response (tokens + narrative preview) is logged at main.go:525
	// as [Hermes→MCP/RESPONSE]. Don't duplicate here.
	return &hr, nil
}

// doSendStreaming performs the streaming HTTP POST and parses the SSE event
// stream. Caller MUST hold sendMu.
//
// SSE format (Hermes /v1/responses with stream:true):
//
//	event: response.output_text.delta
//	data: {"delta":"文本块"}
//
//	event: response.completed
//	data: {"response":{"id":...,"status":...,"output":[...],"usage":{...}}}
//
//	event: response.failed
//	data: {"error":{"message":"..."}}
//
//	: keepalive
//
// Events are separated by blank lines. A stream that ends without a terminal
// event (response.completed/response.failed) returns io.ErrUnexpectedEOF.
func (c *Client) doSendStreaming(ctx context.Context, input string, onDelta func(delta string)) (*Response, error) {
	body := request{
		Model:              c.cfg.Model,
		Input:              input,
		PreviousResponseID: c.prevResponseID,
		Stream:             true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	postCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	url := strings.TrimRight(c.cfg.URL, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hermes status %d: %s", resp.StatusCode, string(raw))
	}

	sc := bufio.NewScanner(resp.Body)
	// SSE events can carry a large response.completed payload; allow up to 1MB per line.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		eventType string
		dataBuf   strings.Builder
		finalResp *Response
	)

	for sc.Scan() {
		line := sc.Text()

		// Blank line = event boundary → dispatch accumulated event.
		if line == "" {
			if eventType != "" && dataBuf.Len() > 0 {
				fr, terminal, herr := c.handleSSEEvent(eventType, dataBuf.String(), onDelta)
				if herr != nil {
					return nil, herr
				}
				if terminal {
					finalResp = fr
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
	if finalResp == nil {
		return nil, fmt.Errorf("sse stream ended without terminal event: %w", io.ErrUnexpectedEOF)
	}

	// Upstream error detection — same as doSend.
	if isUpstreamError(finalResp) {
		narrative := finalResp.ExtractText()
		c.log.Warn("[Hermes→MCP] upstream error detected (streaming), resetting session",
			"narrative", truncate(narrative, 200),
			"prev_id", c.prevResponseID)
		c.prevResponseID = ""
		c.pendingSummary = ""
		c.turnCount = 0
		return finalResp, ErrUpstreamError
	}

	if finalResp.ID != "" {
		c.prevResponseID = finalResp.ID
	}
	return finalResp, nil
}

// handleSSEEvent dispatches one accumulated SSE event. Returns the parsed
// Response and terminal=true when the event is response.completed, and a
// non-nil error for response.failed. For delta events, onDelta is invoked.
func (c *Client) handleSSEEvent(eventType, data string, onDelta func(delta string)) (*Response, bool, error) {
	switch eventType {
	case "response.output_text.delta":
		if onDelta == nil {
			return nil, false, nil
		}
		var d sseDelta
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			c.log.Debug("sse delta parse failed", "err", err, "data", truncate(data, 200))
			return nil, false, nil
		}
		if d.Delta != "" {
			onDelta(d.Delta)
		}
		return nil, false, nil
	case "response.completed":
		var comp sseCompleted
		if err := json.Unmarshal([]byte(data), &comp); err != nil {
			return nil, false, fmt.Errorf("parse response.completed: %w", err)
		}
		return &comp.Response, true, nil
	case "response.failed":
		var f sseFailed
		_ = json.Unmarshal([]byte(data), &f)
		msg := f.Error.Message
		if msg == "" {
			msg = "response.failed (no message)"
		}
		return nil, false, fmt.Errorf("hermes stream failed: %s", msg)
	}
	return nil, false, nil
}

// isUpstreamError checks whether the Hermes response wraps an upstream
// LLM API error rather than a genuine assistant narrative.
func isUpstreamError(r *Response) bool {
	// Error responses have zero token usage (no LLM call was made).
	if r.Usage.TotalTokens == 0 && r.Status == "completed" {
		text := r.ExtractText()
		if text == "" {
			return false
		}
		// Common upstream error patterns returned by Hermes as narrative.
		for _, prefix := range []string{"HTTP 4", "HTTP 5", "Invalid '", "Error:", "error:"} {
			if strings.HasPrefix(text, prefix) {
				return true
			}
		}
	}
	return false
}

// truncate returns s truncated to maxLen characters with "..." appended.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ResetSession clears the session state. Safe to call from any goroutine.
func (c *Client) ResetSession() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.prevResponseID = ""
	c.pendingSummary = ""
	c.turnCount = 0
	c.log.Info("hermes session reset (new game day)")
}

// SessionID returns the current previous_response_id (for /status).
func (c *Client) SessionID() string {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.prevResponseID
}

// request is the body sent to /v1/responses.
type request struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	Stream             bool   `json:"stream,omitempty"`
}

// SSE event payloads emitted by Hermes /v1/responses with stream:true.
type sseDelta struct {
	Delta string `json:"delta"`
}
type sseCompleted struct {
	Response Response `json:"response"`
}
type sseFailed struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Response is the (subset of the) OpenAI Responses shape Hermes returns.
type Response struct {
	ID     string  `json:"id"`
	Status string  `json:"status"`
	Model  string  `json:"model"`
	Output []Block `json:"output"`
	Usage  Usage   `json:"usage"`
}

// Block is one entry in Response.Output.
type Block struct {
	Type    string    `json:"type"`
	Role    string    `json:"role,omitempty"`
	Content []Content `json:"content,omitempty"`
}

// Content is one piece of a message Block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Usage reports token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ExtractText returns the assistant's narrative text from the response.
func (r *Response) ExtractText() string {
	if r == nil {
		return ""
	}
	for _, b := range r.Output {
		if b.Type == "message" && b.Role == "assistant" {
			for _, c := range b.Content {
				if c.Type == "output_text" {
					return c.Text
				}
			}
		}
	}
	return ""
}

// ErrNoHermesResponse is returned when the gateway returns an empty body.
var ErrNoHermesResponse = errors.New("hermes returned no response")
