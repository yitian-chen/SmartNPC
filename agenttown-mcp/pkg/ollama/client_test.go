package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNew_Defaults verifies that zero-value Options produces a Client
// with the documented defaults.
func TestNew_Defaults(t *testing.T) {
	c := New(Options{})
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL: got %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.model != defaultModel {
		t.Errorf("model: got %q, want %q", c.model, defaultModel)
	}
	if c.httpClient.Timeout != defaultHTTPTimeout {
		t.Errorf("timeout: got %v, want %v", c.httpClient.Timeout, defaultHTTPTimeout)
	}
	if c.numPredict != defaultNumPredict {
		t.Errorf("numPredict: got %d, want %d", c.numPredict, defaultNumPredict)
	}
	if c.numThread != defaultNumThread {
		t.Errorf("numThread: got %d, want %d", c.numThread, defaultNumThread)
	}
}

// TestNew_CustomOptions verifies that explicit Options are honored,
// including trailing-slash trimming on BaseURL.
func TestNew_CustomOptions(t *testing.T) {
	c := New(Options{
		BaseURL:    "http://127.0.0.1:11434/",
		Model:      "qwen2.5:14b",
		Timeout:    2 * time.Second,
		NumPredict: 200,
		NumThread:  8,
	})
	if c.baseURL != "http://127.0.0.1:11434" {
		t.Errorf("baseURL: got %q, want trimmed %q", c.baseURL, "http://127.0.0.1:11434")
	}
	if c.model != "qwen2.5:14b" {
		t.Errorf("model: got %q, want %q", c.model, "qwen2.5:14b")
	}
	if c.httpClient.Timeout != 2*time.Second {
		t.Errorf("timeout: got %v, want 2s", c.httpClient.Timeout)
	}
	if c.numPredict != 200 {
		t.Errorf("numPredict: got %d, want 200", c.numPredict)
	}
	if c.numThread != 8 {
		t.Errorf("numThread: got %d, want 8", c.numThread)
	}
}

// TestNew_NumThreadOmit verifies that NumThread=-1 produces a Client that
// omits num_thread from the request (letting Ollama decide).
func TestNew_NumThreadOmit(t *testing.T) {
	c := New(Options{NumThread: -1})
	if c.numThread != -1 {
		t.Errorf("numThread: got %d, want -1", c.numThread)
	}
}

// TestChat_Success sends a prompt to a mock Ollama server and verifies
// the request shape + response parsing.
func TestChat_Success(t *testing.T) {
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path: got %q, want /api/chat", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: got %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Model:   gotReq.Model,
			Message: chatMessage{Role: "assistant", Content: `{"reaction":"continue","reason":"ok"}`},
			Done:    true,
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Model: "test-model"})
	out, err := c.Chat(context.Background(), "test prompt")
	if err != nil {
		t.Fatalf("Chat err: %v", err)
	}
	if out != `{"reaction":"continue","reason":"ok"}` {
		t.Errorf("output: got %q, want JSON", out)
	}
	if gotReq.Model != "test-model" {
		t.Errorf("request model: got %q, want test-model", gotReq.Model)
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" {
		t.Errorf("request messages: %+v, want single user message", gotReq.Messages)
	}
	if gotReq.Messages[0].Content != "test prompt" {
		t.Errorf("prompt: got %q, want %q", gotReq.Messages[0].Content, "test prompt")
	}
	if gotReq.Stream != false {
		t.Errorf("stream: got %v, want false", gotReq.Stream)
	}
	if gotReq.Options.NumPredict != defaultNumPredict {
		t.Errorf("options.num_predict: got %d, want %d", gotReq.Options.NumPredict, defaultNumPredict)
	}
	if gotReq.Options.NumThread != defaultNumThread {
		t.Errorf("options.num_thread: got %d, want %d", gotReq.Options.NumThread, defaultNumThread)
	}
}

// TestChat_NumThreadOmit verifies that a Client with NumThread=-1 omits
// num_thread from the request body, letting Ollama choose its default.
func TestChat_NumThreadOmit(t *testing.T) {
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(chatResponse{
			Message: chatMessage{Role: "assistant", Content: "{}"},
			Done:    true,
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, NumThread: -1})
	if _, err := c.Chat(context.Background(), "x"); err != nil {
		t.Fatalf("Chat err: %v", err)
	}
	if gotReq.Options.NumThread != 0 {
		t.Errorf("num_thread should be omitted (0 in struct), got %d", gotReq.Options.NumThread)
	}
}

// TestChat_Non200 verifies that a non-200 response produces an error
// containing the status code and a body snippet.
func TestChat_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "model not loaded")
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL})
	_, err := c.Chat(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err should mention status 500: %v", err)
	}
	if !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("err should include body snippet: %v", err)
	}
}

// TestChat_MalformedJSON verifies that a malformed response body produces
// a decode error.
func TestChat_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL})
	_, err := c.Chat(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected decode error, got: %v", err)
	}
}

// TestChat_ContextCanceled verifies that a canceled context propagates
// promptly without waiting for the server.
func TestChat_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response; caller should give up first.
		time.Sleep(500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(chatResponse{})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, Timeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Chat(ctx, "x")
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
	// Could be "context deadline exceeded" directly or wrapped as "ollama: http do: ..."
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "ollama") {
		t.Errorf("err should mention context or ollama: %v", err)
	}
}

// TestChat_ConnectionRefused verifies that an unreachable server produces
// a network error (not a panic or hang).
func TestChat_ConnectionRefused(t *testing.T) {
	// Use a port that's almost certainly closed.
	c := New(Options{BaseURL: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	_, err := c.Chat(context.Background(), "x")
	if err == nil {
		t.Fatal("expected connection error, got nil")
	}
	if !strings.Contains(err.Error(), "ollama") {
		t.Errorf("err should be wrapped with ollama prefix: %v", err)
	}
}

// TestChat_Accessors verifies Model() and BaseURL() return configured values.
func TestChat_Accessors(t *testing.T) {
	c := New(Options{BaseURL: "http://example:11434", Model: "custom-model"})
	if c.Model() != "custom-model" {
		t.Errorf("Model(): got %q, want custom-model", c.Model())
	}
	if c.BaseURL() != "http://example:11434" {
		t.Errorf("BaseURL(): got %q, want http://example:11434", c.BaseURL())
	}
}
