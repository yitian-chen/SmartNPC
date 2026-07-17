// Package hermes provides an HTTP client for the Hermes Gateway
// /v1/responses endpoint. It maintains a per-game-day session via the
// `previous_response_id` field, with automatic summarization + reset
// when the token count exceeds a configurable threshold.
package hermes

import (
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
// and resets the session. 50K tokens is roughly 4-5 game hours of
// perception + tool calls with DeepSeek.
const DefaultTokenThreshold = 50000

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
// It owns the `previous_response_id` session state so that one game day
// chains into a single Hermes conversation. When the token count exceeds
// TokenThreshold, it automatically:
//  1. Asks Hermes to summarize the day so far
//  2. Resets the session (clears previous_response_id)
//  3. Prepends the summary to the next perception
//
// This prevents the exponential token growth that would otherwise make
// a full 16-hour day infeasible.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	mu              sync.Mutex
	prevResponseID  string
	pendingSummary  string // summary from a reset, prepended to next Send
	turnCount       int    // perceptions sent since last reset
}

// New creates a Client. Defaults: 120s timeout, 50K token threshold.
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

// Send POSTs the given input to Hermes /v1/responses and returns the
// response. If the token count exceeds the threshold after this call,
// it automatically triggers a summarize-and-reset cycle.
func (c *Client) Send(ctx context.Context, input string) (*Response, error) {
	// If there's a pending summary from a previous reset, prepend it.
	fullInput := input
	c.mu.Lock()
	summary := c.pendingSummary
	c.pendingSummary = ""
	c.turnCount++
	c.mu.Unlock()

	if summary != "" {
		fullInput = "[今日纪要] " + summary + "\n\n" + input
	}

	resp, err := c.doSend(ctx, fullInput)
	if err != nil {
		return resp, err
	}

	// Check if we need to summarize and reset.
	if resp.Usage.TotalTokens > c.cfg.TokenThreshold {
		c.log.Info("token threshold exceeded, auto-summarizing",
			"tokens", resp.Usage.TotalTokens,
			"threshold", c.cfg.TokenThreshold,
			"turn", c.turnCount,
		)
		if err := c.summarizeAndReset(ctx); err != nil {
			c.log.Warn("auto-summarize failed, continuing with current session", "err", err)
		}
	}

	return resp, nil
}

// doSend performs the actual HTTP POST, updating prevResponseID.
func (c *Client) doSend(ctx context.Context, input string) (*Response, error) {
	c.mu.Lock()
	prev := c.prevResponseID
	c.mu.Unlock()

	body := request{
		Model:              c.cfg.Model,
		Input:              input,
		PreviousResponseID: prev,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	postCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	url := strings.TrimRight(c.cfg.URL, "/") + "/v1/responses"
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	c.log.Debug("hermes POST", "url", url, "input_len", len(input), "prev_id", prev)

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

	if hr.ID != "" {
		c.mu.Lock()
		c.prevResponseID = hr.ID
		c.mu.Unlock()
	}

	c.log.Info("hermes turn", "id", hr.ID, "tokens", hr.Usage.TotalTokens, "status", hr.Status, "turn", c.turnCount)
	return &hr, nil
}

// summarizeAndReset asks Hermes for a brief summary of the day so far,
// then resets the session. The summary is stored and prepended to the
// next Send() call so the LLM retains day context without the full
// conversation history.
func (c *Client) summarizeAndReset(ctx context.Context) error {
	// Ask for a concise summary.
	resp, err := c.doSend(ctx, "请用100字以内总结今天到目前为止发生的事，包括你做了什么、见了谁、有什么重要发现。只输出总结，不要其他内容。")
	if err != nil {
		return fmt.Errorf("summarize send: %w", err)
	}

	summary := resp.ExtractText()
	if summary == "" {
		return errors.New("summary was empty")
	}

	// Truncate to a reasonable length to avoid the summary itself
	// being too large.
	if len(summary) > 500 {
		summary = summary[:500]
	}

	// Reset the session — next Send will start a fresh conversation.
	c.mu.Lock()
	c.prevResponseID = ""
	c.pendingSummary = summary
	c.turnCount = 0
	c.mu.Unlock()

	c.log.Info("session reset with summary", "summary_len", len(summary), "summary", summary)
	return nil
}

// ResetSession clears the stored previous_response_id and summary.
// Call on day_started so a new game day begins a fresh conversation.
func (c *Client) ResetSession() {
	c.mu.Lock()
	c.prevResponseID = ""
	c.pendingSummary = ""
	c.turnCount = 0
	c.mu.Unlock()
	c.log.Info("hermes session reset (new game day)")
}

// SessionID returns the current previous_response_id (for /status).
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prevResponseID
}

// request is the body sent to /v1/responses.
type request struct {
	Model              string `json:"model"`
	Input              string `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
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
