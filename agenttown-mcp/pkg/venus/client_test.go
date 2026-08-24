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

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
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

// TestSendWithSummary_NonStreaming verifies a non-streaming OpenAI Chat
// Completions call is parsed and converted to *llmtypes.Response correctly.
func TestSendWithSummary_NonStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		// Venus 用 Bearer 认证
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want 'Bearer test-key'", got)
		}
		// Venus 自定义头
		if got := r.Header.Get("Venus-Sticky-Routing"); got != "token" {
			t.Errorf("Venus-Sticky-Routing = %q, want token", got)
		}
		// OpenAI 协议不需要 anthropic-version 头
		if got := r.Header.Get("anthropic-version"); got != "" {
			t.Errorf("anthropic-version should be absent, got %q", got)
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
			"id": "chatcmpl-123",
			"model": "default",
			"choices": [{"message": {"role": "assistant", "content": "Hello, world!"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendWithSummary(context.Background(), "", "hi")
	if err != nil {
		t.Fatalf("SendWithSummary: %v", err)
	}
	if resp.ID != "chatcmpl-123" {
		t.Errorf("ID = %q", resp.ID)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q", resp.Status)
	}
	// Model "default" should fall back to cfg.Model
	if resp.Model != "qwen3.6-35b-a3b" {
		t.Errorf("Model = %q, want qwen3.6-35b-a3b (fallback from default)", resp.Model)
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

// TestSendWithSummary_SystemMessage verifies a non-empty system prompt is
// sent as a leading role:"system" message before the user message.
func TestSendWithSummary_SystemMessage(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, _ = c.SendWithSummary(context.Background(), "you are an NPC", "input")
	if len(capturedRequest.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(capturedRequest.Messages))
	}
	if capturedRequest.Messages[0].Role != "system" || capturedRequest.Messages[0].Content != "you are an NPC" {
		t.Errorf("messages[0] = %+v, want system message", capturedRequest.Messages[0])
	}
	if capturedRequest.Messages[1].Role != "user" || capturedRequest.Messages[1].Content != "input" {
		t.Errorf("messages[1] = %+v, want user message", capturedRequest.Messages[1])
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
	_, err := c.SendWithSummary(context.Background(), "", "hi")
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
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.SendWithSummary(ctx, "", "hi")
	if err == nil {
		t.Fatal("expected context deadline error")
	}
}

// TestSendStreaming_SSE verifies streaming OpenAI SSE parsing assembles
// the full response and invokes onDelta for each text chunk.
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
		// 流式路径也应带 Bearer 认证和 Venus 自定义头
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want 'Bearer test-key'", got)
		}
		if got := r.Header.Get("Venus-Sticky-Routing"); got != "token" {
			t.Errorf("Venus-Sticky-Routing = %q, want token", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// first chunk with role + first content
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-abc\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}\n\n"))
		flusher.Flush()
		// second content chunk
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-abc\",\"choices\":[{\"delta\":{\"content\":\", world!\"}}]}\n\n"))
		flusher.Flush()
		// terminal chunk with finish_reason
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-abc\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		flusher.Flush()
		// DONE marker
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var deltas []string
	resp, err := c.SendStreaming(context.Background(), "", "hi", func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if got := strings.Join(deltas, ""); got != "Hello, world!" {
		t.Errorf("deltas = %q, want %q", got, "Hello, world!")
	}
	if got := resp.ExtractText(); got != "Hello, world!" {
		t.Errorf("ExtractText = %q", got)
	}
	if resp.ID != "chatcmpl-abc" {
		t.Errorf("ID = %q", resp.ID)
	}
	// Model falls back to cfg.Model (OpenAI streaming chunks don't carry model)
	if resp.Model != "qwen3.6-35b-a3b" {
		t.Errorf("Model = %q", resp.Model)
	}
}

// TestSendStreaming_WithUsage verifies usage info from a chunk is captured
// when present (e.g. via stream_options.include_usage).
func TestSendStreaming_WithUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"id\":\"m1\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":3,\"total_tokens\":11}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendStreaming(context.Background(), "", "hi", nil)
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if resp.Usage.TotalTokens != 11 {
		t.Errorf("TotalTokens = %d, want 11", resp.Usage.TotalTokens)
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

// TestSendStreaming_KeepaliveComment verifies SSE comment lines are skipped.
func TestSendStreaming_KeepaliveComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(": keepalive\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"id\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendStreaming(context.Background(), "", "hi", nil)
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if got := resp.ExtractText(); got != "ok" {
		t.Errorf("ExtractText = %q, want ok", got)
	}
}

// TestSendStreaming_MissingDoneMarker verifies a stream that ends without
// [DONE] but with accumulated content still returns a response
// (graceful degradation).
func TestSendStreaming_MissingDoneMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"id\":\"m1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		flusher.Flush()
		// connection drops here — no [DONE]
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendStreaming(context.Background(), "", "hi", nil)
	if err != nil {
		t.Fatalf("expected graceful partial response, got error: %v", err)
	}
	if got := resp.ExtractText(); got != "partial" {
		t.Errorf("ExtractText = %q, want partial", got)
	}
}

// TestSendStreaming_EmptyStream verifies an empty stream with no content
// returns an error rather than a silent empty response.
func TestSendStreaming_EmptyStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	_, err := c.SendStreaming(context.Background(), "", "hi", nil)
	if err == nil {
		t.Fatal("expected error for empty stream")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got: %v", err)
	}
}

// TestSendWithSchema_RequestIncludesResponseFormat verifies SendWithSchema
// adds response_format (json_schema, strict) to the request body and the
// schema document round-trips.
func TestSendWithSchema_RequestIncludesResponseFormat(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"[]"}}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	schema := []byte(`{"type":"array","items":{"type":"object","properties":{"goal":{"type":"string"}}}}`)
	if _, err := c.SendWithSchema(context.Background(), "sys", "user", "daily_plan", schema); err != nil {
		t.Fatalf("SendWithSchema: %v", err)
	}
	if capturedRequest.ResponseFormat == nil {
		t.Fatal("response_format should be present for SendWithSchema")
	}
	if capturedRequest.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want json_schema", capturedRequest.ResponseFormat.Type)
	}
	js := capturedRequest.ResponseFormat.JSONSchema
	if js == nil {
		t.Fatal("json_schema should be present")
	}
	if js.Name != "daily_plan" || !js.Strict {
		t.Errorf("json_schema name/strict = %q/%v", js.Name, js.Strict)
	}
	if string(js.Schema) != string(schema) {
		t.Errorf("schema = %s, want %s", js.Schema, schema)
	}
}

// TestSendWithSummary_NoResponseFormat verifies plain SendWithSummary does
// not attach response_format (schema mode is opt-in per call).
func TestSendWithSummary_NoResponseFormat(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	if _, err := c.SendWithSummary(context.Background(), "", "hi"); err != nil {
		t.Fatalf("SendWithSummary: %v", err)
	}
	if capturedRequest.ResponseFormat != nil {
		t.Errorf("response_format should be absent, got %+v", capturedRequest.ResponseFormat)
	}
	if capturedRequest.Tools != nil {
		t.Errorf("tools should be absent for plain SendWithSummary, got %+v", capturedRequest.Tools)
	}
}

// TestSendWithSummaryTools_RequestIncludesTools verifies SendWithSummaryTools
// serializes the `tools` array (function calling) into the request body.
func TestSendWithSummaryTools_RequestIncludesTools(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "work_shift",
			Description: "去指定设施执行工作",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"semantic_group":{"type":"string"}},"required":["semantic_group"]}`),
		},
	}}
	if _, err := c.SendWithSummaryTools(context.Background(), "sys", "user", tools); err != nil {
		t.Fatalf("SendWithSummaryTools: %v", err)
	}
	if len(capturedRequest.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(capturedRequest.Tools))
	}
	got := capturedRequest.Tools[0]
	if got.Type != "function" {
		t.Errorf("tools[0].type = %q, want function", got.Type)
	}
	if got.Function.Name != "work_shift" {
		t.Errorf("tools[0].function.name = %q, want work_shift", got.Function.Name)
	}
	if string(got.Function.Parameters) != `{"type":"object","properties":{"semantic_group":{"type":"string"}},"required":["semantic_group"]}` {
		t.Errorf("tools[0].function.parameters = %s", got.Function.Parameters)
	}
	// tools 非空时自动设置 tool_choice="required"（function calling 强制调用）。
	if capturedRequest.ToolChoice != "required" {
		t.Errorf("tool_choice = %v, want required", capturedRequest.ToolChoice)
	}
}

// TestSendWithSummaryTools_ParsesToolCalls verifies a non-streaming response
// with tool_calls is converted into llmtypes.Response.ToolCalls.
func TestSendWithSummaryTools_ParsesToolCalls(t *testing.T) {
	body := `{"id":"x","choices":[{"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"speak","arguments":"{\"content\":\"hi\"}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"move_to","arguments":"{\"target_id\":\"main_workshop\"}"}}` +
		`]}}],"usage":{}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	resp, err := c.SendWithSummaryTools(context.Background(), "sys", "user", []Tool{{Type: "function", Function: ToolFunction{Name: "speak"}}})
	if err != nil {
		t.Fatalf("SendWithSummaryTools: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "speak" || resp.ToolCalls[0].Function.Arguments != `{"content":"hi"}` {
		t.Errorf("ToolCalls[0] = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].Function.Name != "move_to" || resp.ToolCalls[1].Function.Arguments != `{"target_id":"main_workshop"}` {
		t.Errorf("ToolCalls[1] = %+v", resp.ToolCalls[1])
	}
}

// TestSendMessagesTools_SerializesMultiTurn verifies SendMessagesTools sends
// the full messages array (including assistant tool_calls and tool role with
// tool_call_id) in order.
func TestSendMessagesTools_SerializesMultiTurn(t *testing.T) {
	var capturedRequest request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_9","type":"function","function":{"name":"speak","arguments":"{}"}}]}}],"usage":{}}`))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	messages := []llmtypes.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "round1"},
		{Role: "assistant", Content: "", ToolCalls: []llmtypes.ToolCall{
			{ID: "call_1", Type: "function", Function: llmtypes.ToolFunction{Name: "move_to", Arguments: `{"target_id":"z"}`}},
		}},
		{Role: "tool", Content: "result=success", ToolCallID: "call_1"},
		{Role: "user", Content: "round2"},
	}
	resp, err := c.SendMessagesTools(context.Background(), messages, []Tool{{Type: "function", Function: ToolFunction{Name: "speak"}}})
	if err != nil {
		t.Fatalf("SendMessagesTools: %v", err)
	}
	if len(capturedRequest.Messages) != 5 {
		t.Fatalf("messages len = %d, want 5", len(capturedRequest.Messages))
	}
	if capturedRequest.Messages[0].Role != "system" || capturedRequest.Messages[0].Content != "sys" {
		t.Errorf("messages[0] = %+v", capturedRequest.Messages[0])
	}
	asst := capturedRequest.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Errorf("messages[2] assistant tool_calls = %+v", asst)
	}
	toolMsg := capturedRequest.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "result=success" {
		t.Errorf("messages[3] tool = %+v", toolMsg)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_9" {
		t.Errorf("resp.ToolCalls = %+v", resp.ToolCalls)
	}
}

// TestSendStreamingTools_AccumulatesToolCalls verifies streamed delta.tool_calls
// are accumulated by index and delivered via onToolCall once complete.
func TestSendStreamingTools_AccumulatesToolCalls(t *testing.T) {
	// 用 json.Marshal 构造 SSE chunk，避免手写多层转义。
	mkChunk := func(delta map[string]any, finish string) string {
		chunk := map[string]any{
			"id":      "s1",
			"choices": []any{map[string]any{"delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(chunk)
		return "data: " + string(b) + "\n\n"
	}
	sse := "" +
		mkChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "speak", "arguments": `{"content":"`},
		}}}, "") +
		mkChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    0,
			"function": map[string]any{"arguments": `hi"}`},
		}}}, "") +
		mkChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": 1, "id": "call_2", "type": "function",
			"function": map[string]any{"name": "move_to", "arguments": `{"target_id":"main_workshop"}`},
		}}}, "") +
		mkChunk(map[string]any{}, "tool_calls") +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var completed []llmtypes.ToolCall
	resp, err := c.SendStreamingTools(context.Background(), "sys", "user", []Tool{{Type: "function", Function: ToolFunction{Name: "speak"}}}, nil, func(tc llmtypes.ToolCall) {
		completed = append(completed, tc)
	})
	if err != nil {
		t.Fatalf("SendStreamingTools: %v", err)
	}
	if len(completed) != 2 {
		t.Fatalf("onToolCall count = %d, want 2", len(completed))
	}
	if completed[0].Function.Name != "speak" || completed[0].Function.Arguments != `{"content":"hi"}` {
		t.Errorf("completed[0] = %+v, want speak with content hi", completed[0])
	}
	if completed[1].Function.Name != "move_to" || completed[1].Function.Arguments != `{"target_id":"main_workshop"}` {
		t.Errorf("completed[1] = %+v, want move_to", completed[1])
	}
	// 最终 Response 也应携带累积后的 ToolCalls。
	if len(resp.ToolCalls) != 2 {
		t.Errorf("resp.ToolCalls len = %d, want 2", len(resp.ToolCalls))
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

// TestOpenaiResponse_ToHermes verifies the response conversion handles
// multiple choices, empty model (fallback), and usage fields.
func TestOpenaiResponse_ToHermes(t *testing.T) {
	or := &openaiResponse{
		ID:    "chat_x",
		Model: "default",
		Choices: []openaiChoice{
			{Message: openaiMessage{Role: "assistant", Content: "part1"}},
			{Message: openaiMessage{Role: "assistant", Content: "part2"}},
		},
		Usage: openaiUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}
	hr := or.toLlmTypes("fallback-model")
	if hr.ID != "chat_x" {
		t.Errorf("ID = %q", hr.ID)
	}
	if hr.Model != "fallback-model" {
		t.Errorf("Model = %q, want fallback-model", hr.Model)
	}
	if got := hr.ExtractText(); got != "part1part2" {
		t.Errorf("ExtractText = %q, want part1part2", got)
	}
	if hr.Usage.TotalTokens != 7 {
		t.Errorf("TotalTokens = %d, want 7", hr.Usage.TotalTokens)
	}
	if hr.Usage.InputTokens != 3 || hr.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v", hr.Usage)
	}
}

// TestVenusClient_MatchesLLMClientSignatures is a compile-time check that
// *venus.Client satisfies the llmClient interface expected by main.go.
func TestVenusClient_MatchesLLMClientSignatures(t *testing.T) {
	var _ interface {
		SendWithSummary(ctx context.Context, system, user string) (*llmtypes.Response, error)
		SendStreaming(ctx context.Context, system, user string, onDelta func(string)) (*llmtypes.Response, error)
		SendWithSchema(ctx context.Context, system, user, schemaName string, schema []byte) (*llmtypes.Response, error)
		ResetSession()
	} = (*Client)(nil)
}
