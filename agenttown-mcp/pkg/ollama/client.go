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
//
// numPredict caps the model's output token count. The reactive layer
// expects a short JSON decision (≤80 tokens); without this cap the model
// may emit hundreds of tokens of Chinese explanation before the JSON,
// blowing the latency budget. 80 is generous for the four-reaction JSON
// schema while keeping eval time under ~2s on qwen2.5:7b.
//
// numThread caps the number of CPU threads used by llama-server for
// inference. CPU inference on memory-bandwidth-bound models (7B Q4_K_M)
// scales sublinearly past ~16 threads and can regress sharply on
// high-core-count CPUs (e.g. 96 vCPU EPYC) where the default (all cores)
// causes cache thrashing and NUMA overhead. 0 means "let Ollama decide"
// (current default = physical core count). Benchmark on the target host
// to find the optimum; 16 is a safe choice for most modern x86 CPUs.
const (
	defaultHTTPTimeout = 5 * time.Second
	defaultModel       = "qwen2.5:7b-instruct-q4_K_M"
	defaultBaseURL     = "http://localhost:11434"
	defaultNumPredict  = 80
	defaultNumThread   = 16
)

// Client talks to a local Ollama instance. It is safe for concurrent use
// only if the caller serializes calls — the reactive layer does this via
// a dedicated mutex in main.go.
type Client struct {
	baseURL    string
	model      string
	numPredict int
	numThread  int
	httpClient *http.Client
	logger     *slog.Logger
}

// Options configures a Client. Zero values fall back to defaults.
type Options struct {
	BaseURL    string        // e.g. http://localhost:11434
	Model      string        // e.g. qwen2.5:7b-instruct-q4_K_M
	Timeout    time.Duration
	NumPredict int           // max output tokens; 0 → defaultNumPredict
	NumThread  int           // CPU threads for inference; 0 → defaultNumThread, -1 → omit (let Ollama decide)
	Logger     *slog.Logger
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
	numPredict := opts.NumPredict
	if numPredict == 0 {
		numPredict = defaultNumPredict
	}
	// NumThread: 0 → default, -1 → omit (let Ollama decide), >0 → explicit
	numThread := opts.NumThread
	if numThread == 0 {
		numThread = defaultNumThread
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:    baseURL,
		model:      model,
		numPredict: numPredict,
		numThread:  numThread,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		logger: logger,
	}
}

// chatRequest is the payload sent to /api/chat.
type chatRequest struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  chatOptions    `json:"options,omitempty"`
}

// chatOptions maps to Ollama's model runner options. num_predict caps the
// generated token count — critical for the reactive layer's latency budget.
// num_thread caps CPU threads; omit when numThread < 0 to let Ollama decide.
type chatOptions struct {
	NumPredict int `json:"num_predict"`
	NumThread  int `json:"num_thread,omitempty"`
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
	// numThread: -1 → omit (let Ollama decide), >=0 → explicit
	opts := chatOptions{NumPredict: c.numPredict}
	if c.numThread >= 0 {
		opts.NumThread = c.numThread
	}
	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Options: opts,
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

// NumThread returns the configured CPU thread count (-1 means "let Ollama decide").
func (c *Client) NumThread() int { return c.numThread }
