// Package hermes provides an HTTP client for the Hermes Gateway
// /v1/responses endpoint. It maintains a per-game-day session via the
// `previous_response_id` field, mirroring the chaining logic used by the
// Python Mock UE before the MCP layer took over Hermes communication.
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

// Config configures the Client.
type Config struct {
	// URL is the Hermes Gateway base URL, e.g. "http://localhost:8642".
	URL string

	// APIKey is the Bearer token sent in the Authorization header. Must
	// match the profile's API_SERVER_KEY.
	APIKey string

	// Model is the LLM model name, e.g. "deepseek-v4-flash".
	Model string

	// Logger. Defaults to slog.Default() if nil.
	Logger *slog.Logger
}

// Client POSTs perception text to Hermes /v1/responses.
//
// It owns the `previous_response_id` session state so that one game day
// chains into a single Hermes conversation. ResetSession clears the id —
// call it on day_started events from Mock UE.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	mu              sync.Mutex
	prevResponseID string
}

// New creates a Client. Defaults: 120s timeout.
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 120 * time.Second},
		log:  cfg.Logger,
	}
}

// Send POSTs the given input (natural-language perception) to Hermes
// /v1/responses and returns the response body. Stores resp.ID for the
// next call's `previous_response_id`. Thread-safe.
func (c *Client) Send(ctx context.Context, input string) (*Response, error) {
	// Snapshot the session id under lock; release before the HTTP call so
	// concurrent Sends don't serialize on the network.
	c.mu.Lock()
	prev := c.prevResponseID
	c.mu.Unlock()

	body := request{
		Model:               c.cfg.Model,
		Input:               input,
		PreviousResponseID:  prev,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Detach from the caller's context so a WS reconnect or Mock UE event
	// cancellation doesn't abort an in-flight LLM call. We still honor a
	// per-call timeout via the http.Client.
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

	// Persist the id for the next call.
	if hr.ID != "" {
		c.mu.Lock()
		c.prevResponseID = hr.ID
		c.mu.Unlock()
	}

	c.log.Info("hermes turn", "id", hr.ID, "tokens", hr.Usage.TotalTokens, "status", hr.Status)
	return &hr, nil
}

// ResetSession clears the stored previous_response_id. Call on day_started
// so a new game day begins a fresh Hermes conversation.
func (c *Client) ResetSession() {
	c.mu.Lock()
	c.prevResponseID = ""
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
	Type    string   `json:"type"`
	Role    string   `json:"role,omitempty"`
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

// ExtractText returns the assistant's narrative text from the response, if any.
// Mirrors mock_ue.py's extract_text.
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
