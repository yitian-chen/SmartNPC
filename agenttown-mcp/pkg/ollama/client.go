// Package ollama provides a minimal HTTP client for the Ollama `/api/chat`
// endpoint. It is used by the reactive layer for low-latency local inference.
//
// Unlike the hermes client, this client maintains NO session chain — each
// Chat call is an independent prompt with a fixed token budget. Failures
// are fast and non-retried: the reactive layer should silently fall back
// to "continue" when Ollama is unavailable.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"log/slog"
)

// Default timeouts. The reactive layer needs decisions in ≤3s; we give
// the HTTP call a 5s ceiling so a slow first-token doesn't block the
// worker loop, but rely on the caller's ctx for the hard deadline.
const (
	defaultHTTPTimeout = 5 * time.Second
	defaultModel       = "qwen2.5:7b-instruct-q4_K_M"
	defaultBaseURL     = "http://localhost:11434"
)

// Client talks to a local Ollama instance. It is safe for concurrent use
// only if the caller serializes calls — the reactive layer does this via
// a dedicated mutex in main.go.
type Client struct {
	baseURL    string
	model      string
	httpClient *http.Client
	logger     *slog.Logger
}

// Options configures a Client. Zero values fall back to defaults.
type Options struct {
	BaseURL string // e.g. http://localhost:11434
	Model   string // e.g. qwen2.5:7b-instruct-q4_K_M
	Timeout time.Duration
	Logger  *slog.Logger
}

// New creates a Client. If opts is incomplete, defaults are applied.
// If logger is nil, slog.Default() is used.
func New(opts Options) *Client {
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := opts.Model
	if model == "" {
		model = defaultModel
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultHTTPTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL: baseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		logger: logger,
	}
}

// chatRequest is the payload sent to /api/chat.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the payload returned by /api/chat (non-streaming).
type chatResponse struct {
	Model     string `json:"model"`
	Message   chatMessage `json:"message"`
	Done      bool   `json:"done"`
}

// Chat sends a single-turn prompt to the Ollama model and returns the
// model's text output. The prompt is wrapped as a user message; no
// system message is prepended (callers bake instructions into the prompt).
//
// The caller's ctx is respected — if it cancels before the HTTP call
// completes, Chat returns ctx.Err() promptly.
//
// On any error (network, non-200, malformed JSON), Chat returns an error
// and the caller should treat it as "decision unavailable, fall back to continue".
func (c *Client) Chat(ctx context.Context, prompt string) (string, error) {
	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("ollama: marshal request: %w", err)
	}

	url := c.baseURL + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("ollama: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read a small snippet for the error message.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf("ollama: non-200 status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("ollama: decode response: %w", err)
	}

	return chatResp.Message.Content, nil
}

// Model returns the configured model name (for logging / status endpoints).
func (c *Client) Model() string { return c.model }

// BaseURL returns the configured base URL (for logging / status endpoints).
func (c *Client) BaseURL() string { return c.baseURL }
