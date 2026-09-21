// Package jev is a client for the Venus judgment-model API (POST /v1/systemone).
//
// Unlike the chat-completions models, the judgment model does not generate
// text: the caller submits a state (free-form context text) plus a fixed set
// of typed questions, and gets back calibrated answers — noul (probability
// that an instruction holds), choice (one of the given options with the full
// probability distribution), and score (a value anchored by two endpoint
// descriptions). It is a natural fit for the event router's single verdict
// (interrupt or enqueue): ~1s latency, no JSON-in-prose parsing, and the
// judgment criteria travel inside the questions themselves.
//
// The API shares the Venus gateway and Bearer credentials with chat
// completions (same BaseURL / APIKey), but is a separate API surface: the
// /v1/models listing does not include jev models.
package jev

import (
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
)

// defaultTimeout bounds one Judge call. Live measurements sit around 1s
// regardless of question count; 10s is a generous ceiling.
const defaultTimeout = 10 * time.Second

// Question types accepted by the API ("question.type 仅支持 noul、choice、score").
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
	TypeScore  = "score"
)

// Config configures a Client.
type Config struct {
	BaseURL string        // Venus proxy base URL (same gateway as chat completions)
	APIKey  string        // Bearer credential (reuse the venus key)
	Model   string        // judgment model ID, e.g. "jev-1.13.0"
	Logger  *slog.Logger  // nil → slog.Default()
	Timeout time.Duration // 0 → defaultTimeout
}

// Client talks to the judgment API. It is stateless between calls; the only
// mutable member is lastRequestBody (for the actual-prompts doc dump), guarded
// by bodyMu. Unlike venus.Client there is no call-serializing mutex: judgment
// calls are independent one-shots and concurrent calls are safe.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	bodyMu          sync.Mutex
	lastRequestBody []byte
}

// New builds a Client from cfg (applies defaults for Logger and Timeout).
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
		log:  cfg.Logger,
	}
}

// Question is one typed question in a Judge request.
type Question struct {
	Type         string          `json:"type"`               // TypeNoul / TypeChoice / TypeScore
	Instructions string          `json:"instructions"`       // the question itself, including judgment criteria
	Criteria     json.RawMessage `json:"criteria,omitempty"` // noul: omitted; choice: {"key": "desc"|null}; score: ["low","high"]
}

// NoulQuestion builds a yes/probability question: the answer's Noul value is
// the probability that Instructions holds (e.g. "should the NPC interrupt?").
func NoulQuestion(instructions string) Question {
	return Question{Type: TypeNoul, Instructions: instructions}
}

// ChoiceQuestion builds a multiple-choice question. options maps the option
// key to its description; an empty description serializes as null (the key
// list alone is a valid criteria object).
func ChoiceQuestion(instructions string, options map[string]string) Question {
	obj := make(map[string]any, len(options))
	for k, v := range options {
		if v == "" {
			obj[k] = nil
		} else {
			obj[k] = v
		}
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		// map[string]any with string/nil values always marshals.
		return Question{Type: TypeChoice, Instructions: instructions}
	}
	return Question{Type: TypeChoice, Instructions: instructions, Criteria: raw}
}

// ScoreQuestion builds an anchored 0-1 score question: lowAnchor describes the
// 0 end, highAnchor the 1 end. The returned Score may slightly exceed 1 (the
// API does not hard-clamp), so callers clamp.
func ScoreQuestion(instructions, lowAnchor, highAnchor string) Question {
	raw, err := json.Marshal([]string{lowAnchor, highAnchor})
	if err != nil {
		return Question{Type: TypeScore, Instructions: instructions}
	}
	return Question{Type: TypeScore, Instructions: instructions, Criteria: raw}
}

// Request is one judgment call: state plus the questions to answer.
type Request struct {
	Model     string              `json:"model"` // empty → filled from Config.Model
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Answer is one question's answer. Only the field matching Type is set;
// Probabilities is the full distribution for choice/score answers.
// (The API's "legend" field on score answers is a map, not an array; we do
// not use it and deliberately leave it unparsed.)
type Answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Usage mirrors the API's token accounting.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the judgment result for every question in the request.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Judge submits one request and parses the answers. The HTTP status error
// wraps the raw body, which carries the Venus error envelope ("code":"4030"
// etc.) so isVenusErrorCode's string matching keeps working.
func (c *Client) Judge(ctx context.Context, req *Request) (*Response, error) {
	body := *req // copy: filling Model must not mutate the caller's request
	if body.Model == "" {
		body.Model = c.cfg.Model
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// 记录完整请求体（供 docs/actual_prompts.md 留存最新一次判决输入）。
	c.bodyMu.Lock()
	c.lastRequestBody = payload
	c.bodyMu.Unlock()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/v1/systemone"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 与 chat completions 同网关的 Bearer 认证；粘性路由头是会话型提示，
	// 判决 API 无会话概念，不发。
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jev status %d: %s", resp.StatusCode, string(raw))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &out, nil
}

// LastRequestBody returns a copy of the JSON body of the most recent request
// sent via this client. Empty if none yet. Same contract as venus.Client —
// the actual-prompts doc dump relies on it.
func (c *Client) LastRequestBody() []byte {
	c.bodyMu.Lock()
	defer c.bodyMu.Unlock()
	return append([]byte(nil), c.lastRequestBody...)
}
