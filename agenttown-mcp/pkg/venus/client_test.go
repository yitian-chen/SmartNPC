package venus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/hermes"
)

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	return New(Config{
		BaseURL:   url,
		APIKey:    "test-key",
		Model:     "qwen3.6-35b-a3b",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timeout:   5 * time.Second,
		MaxTokens: 256,
	})
}

// TestSendWithSummary_NonStreaming verifies a non-streaming Messages API call
// is parsed and converted to *hermes.Response correctly.
func TestSendWithSummary_NonStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
		}
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "qwen3.6-35b-a3b" {
			t.Errorf("model = %q", req.Model)
		}
		if req.MaxTokens != 256 {
			t.Errorf("max_tokens = %d, want 256", req.MaxTokens)
		}
		if req.Stream {
			t.Errorf("stream should be false for non-streaming")
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("messages = %+v", req.Messages)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_123",
			"model": "qwen3.6-35b-a3b",
			"content": [{"type": "text", "text": "Hello, world!"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendWithSummary(context.Background(), "hi", "")
	if err != nil {
		t.Fatalf("SendWithSummary: %v", err)
	}
	if resp.ID != "msg_123" {
		t.Errorf("ID = %q", resp.ID)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q", resp.Status)
	}
	if got := resp.ExtractText(); got != "Hello, world!" {
		t.Errorf("ExtractText = %q, want %q", got, "Hello, world!")
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

// TestSendWithSummary_SummaryUnused verifies the summary parameter is accepted
// but does not affect the request (signature compatibility with hermes.Client).
func TestSendWithSummary_SummaryUnused(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, _ = c.SendWithSummary(context.Background(), "input", "some summary")
	if len(capturedRequest.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(capturedRequest.Messages))
	}
	if capturedRequest.Messages[0].Content != "input" {
		t.Errorf("content = %q, want input (summary should not modify)", capturedRequest.Messages[0].Content)
	}
}

// TestSendWithSummary_HTTPError verifies non-200 responses return an error.
func TestSendWithSummary_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.SendWithSummary(context.Background(), "hi", "")
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status 401: %v", err)
	}
}

// TestSendWithSummary_ContextCanceled verifies context cancellation propagates.
func TestSendWithSummary_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","content":[{"type":"text","text":"ok"}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.SendWithSummary(ctx, "hi", "")
	if err == nil {
		t.Fatal("expected context deadline error")
	}
}

// TestSendStreaming_SSE verifies streaming SSE parsing assembles the full
// response and invokes onDelta for each text chunk.
func TestSendStreaming_SSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !req.Stream {
			t.Errorf("stream should be true for SendStreaming")
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// message_start
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_abc\",\"model\":\"qwen3.6-35b-a3b\",\"usage\":{\"input_tokens\":8}}}\n\n"))
		flusher.Flush()
		// content_block_delta × 2
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\", world!\"}}\n\n"))
		flusher.Flush()
		// message_delta with output token count
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"))
		flusher.Flush()
		// message_stop
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var deltas []string
	resp, err := c.SendStreaming(context.Background(), "hi", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello, world!" {
		t.Errorf("deltas = %q, want %q", got, "Hello, world!")
	}
	if got := resp.ExtractText(); got != "Hello, world!" {
		t.Errorf("ExtractText = %q", got)
	}
	if resp.ID != "msg_abc" {
		t.Errorf("ID = %q", resp.ID)
	}
	if resp.Model != "qwen3.6-35b-a3b" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.Usage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", resp.Usage.TotalTokens)
	}
}

// TestSendStreaming_KeepaliveComment verifies SSE comment lines are skipped.
func TestSendStreaming_KeepaliveComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(": keepalive\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"x\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendStreaming(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if got := resp.ExtractText(); got != "ok" {
		t.Errorf("ExtractText = %q, want ok", got)
	}
}

// TestSendStreaming_StreamError verifies the error SSE event is surfaced.
func TestSendStreaming_StreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":{\"type\":\"overloaded_error\",\"message\":\"Venus is overloaded\"}}\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.SendStreaming(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Venus is overloaded") {
		t.Errorf("error should contain message: %v", err)
	}
}

// TestResetSession_NoOp verifies ResetSession is a safe no-op.
func TestResetSession_NoOp(t *testing.T) {
	c := newTestClient(t, "http://example.invalid")
	c.ResetSession() // must not panic
	c.ResetSession()
}

// TestNew_Defaults verifies Config defaults are applied.
func TestNew_Defaults(t *testing.T) {
	c := New(Config{
		BaseURL: "http://x",
		APIKey:  "k",
		Model:   "m",
	})
	if c.cfg.Timeout != defaultHTTPTimeout {
		t.Errorf("Timeout = %v, want %v", c.cfg.Timeout, defaultHTTPTimeout)
	}
	if c.cfg.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", c.cfg.MaxTokens, defaultMaxTokens)
	}
	if c.cfg.Logger == nil {
		t.Error("Logger should default to slog.Default()")
	}
}

// TestAnthropicResponse_ToHermes verifies the response conversion handles
// multiple text content blocks and empty usage.
func TestAnthropicResponse_ToHermes(t *testing.T) {
	ar := &anthropicResponse{
		ID:    "msg_x",
		Model: "model_y",
		Content: []anthropicContentBlock{
			{Type: "text", Text: "part1"},
			{Type: "text", Text: "part2"},
			{Type: "tool_use", Text: "ignored"}, // non-text blocks skipped
		},
		Usage: anthropicUsage{InputTokens: 3, OutputTokens: 4},
	}
	hr := ar.toHermes()
	if hr.ID != "msg_x" || hr.Model != "model_y" {
		t.Errorf("ID/Model = %q/%q", hr.ID, hr.Model)
	}
	if got := hr.ExtractText(); got != "part1part2" {
		t.Errorf("ExtractText = %q, want part1part2", got)
	}
	if hr.Usage.TotalTokens != 7 {
		t.Errorf("TotalTokens = %d, want 7", hr.Usage.TotalTokens)
	}
}

// TestHermesClient_SatisfiesLLMInterface is a compile-time check that
// *venus.Client satisfies the llmClient interface expected by main.go.
// The interface is defined in package main; here we assert the method
// signatures match hermes.Client (the reference implementation).
func TestVenusClient_MatchesHermesSignatures(t *testing.T) {
	var _ interface {
		SendWithSummary(ctx context.Context, input, summary string) (*hermes.Response, error)
		SendStreaming(ctx context.Context, input string, onDelta func(string)) (*hermes.Response, error)
		ResetSession()
	} = (*Client)(nil)
}

// TestParseStream_MissingTerminalEvent verifies a stream that ends without
// message_stop but with accumulated content still returns a response
// (graceful degradation).
func TestParseStream_MissingTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"x\",\"usage\":{\"input_tokens\":1}}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
		flusher.Flush()
		// connection drops here — no message_stop
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendStreaming(context.Background(), "hi", nil)
	if err != nil {
		t.Fatalf("expected graceful partial response, got error: %v", err)
	}
	if got := resp.ExtractText(); got != "partial" {
		t.Errorf("ExtractText = %q, want partial", got)
	}
}

// TestParseStream_EmptyStream verifies an empty stream with no content
// returns an error rather than a silent empty response.
func TestParseStream_EmptyStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.SendStreaming(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected error for empty stream")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got: %v", err)
	}
}
