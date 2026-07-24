package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSendWithSummary_ResetsLocallyWithoutSecondRequest(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		requestNumber := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_` + string(rune('0'+requestNumber)) + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"total_tokens":100}}`))
	}))
	defer server.Close()

	client := New(Config{
		URL: server.URL, APIKey: "test", Model: "test",
		Logger: slog.Default(), TokenThreshold: 10,
	})
	localSummary := `{"time_of_day":"08:00","recent_actions":[{"result":"success"}]}`
	if _, err := client.SendWithSummary(context.Background(), "first", localSummary); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("threshold reset made %d HTTP requests, want 1", len(bodies))
	}
	if client.SessionID() != "" {
		t.Fatalf("session id not reset: %q", client.SessionID())
	}

	if _, err := client.SendWithSummary(context.Background(), "second", "{}"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("second Send produced total %d requests, want 2", len(bodies))
	}
	var requestBody map[string]any
	if err := json.Unmarshal(bodies[1], &requestBody); err != nil {
		t.Fatal(err)
	}
	input, _ := requestBody["input"].(string)
	if !strings.Contains(input, "[本地状态摘要] "+localSummary) || !strings.Contains(input, "second") {
		t.Fatalf("second input missing local summary: %q", input)
	}
	if _, ok := requestBody["previous_response_id"]; ok {
		t.Fatalf("reset request unexpectedly chained previous_response_id: %v", requestBody)
	}
}

// sseEvent formats one SSE event (event + data + blank-line terminator).
func sseEvent(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

// newSSEServer starts a test server that writes SSE events via the handler.
func newSSEServer(t *testing.T, handler func(w http.ResponseWriter, flusher http.Flusher)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server does not support flushing")
		}
		handler(w, flusher)
	}))
}

func TestSendStreaming_DeltaCallback(t *testing.T) {
	var deltas []string
	server := newSSEServer(t, func(w http.ResponseWriter, flusher http.Flusher) {
		fmt.Fprint(w, sseEvent("response.output_text.delta", `{"delta":"Hello "}`))
		flusher.Flush()
		fmt.Fprint(w, sseEvent("response.output_text.delta", `{"delta":"World"}`))
		flusher.Flush()
		fmt.Fprint(w, sseEvent("response.completed", `{"response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello World"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`))
		flusher.Flush()
	})
	defer server.Close()

	client := New(Config{URL: server.URL, APIKey: "test", Model: "test", Logger: slog.Default()})
	resp, err := client.SendStreaming(context.Background(), "hi", func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if len(deltas) != 2 || deltas[0] != "Hello " || deltas[1] != "World" {
		t.Fatalf("deltas = %v, want [Hello , World]", deltas)
	}
	if resp.ID != "resp_1" {
		t.Fatalf("resp.ID = %q, want resp_1", resp.ID)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("tokens = %d, want 15", resp.Usage.TotalTokens)
	}
	if resp.ExtractText() != "Hello World" {
		t.Fatalf("text = %q, want Hello World", resp.ExtractText())
	}
	if client.SessionID() != "resp_1" {
		t.Fatalf("SessionID = %q, want resp_1", client.SessionID())
	}
}

func TestSendStreaming_RequestBodyHasStreamTrue(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, sseEvent("response.completed", `{"response":{"id":"r","status":"completed","output":[],"usage":{"total_tokens":1}}}`))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	client := New(Config{URL: srv.URL, APIKey: "test", Model: "m", Logger: slog.Default()})
	if _, err := client.SendStreaming(context.Background(), "hi", nil); err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatal(err)
	}
	if stream, _ := body["stream"].(bool); !stream {
		t.Fatalf("request body missing stream:true: %s", string(captured))
	}
}

func TestSendStreaming_FailedEvent(t *testing.T) {
	server := newSSEServer(t, func(w http.ResponseWriter, flusher http.Flusher) {
		fmt.Fprint(w, sseEvent("response.failed", `{"error":{"message":"model overloaded"}}`))
		flusher.Flush()
	})
	defer server.Close()

	client := New(Config{URL: server.URL, APIKey: "test", Model: "test", Logger: slog.Default()})
	_, err := client.SendStreaming(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected error for response.failed, got nil")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("err = %v, want contains 'model overloaded'", err)
	}
}

func TestSendStreaming_Keepalive(t *testing.T) {
	var deltas []string
	server := newSSEServer(t, func(w http.ResponseWriter, flusher http.Flusher) {
		// keepalive comment before any event
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		fmt.Fprint(w, sseEvent("response.output_text.delta", `{"delta":"x"}`))
		flusher.Flush()
		// another keepalive between events
		fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		fmt.Fprint(w, sseEvent("response.completed", `{"response":{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]}],"usage":{"total_tokens":1}}}`))
		flusher.Flush()
	})
	defer server.Close()

	client := New(Config{URL: server.URL, APIKey: "test", Model: "test", Logger: slog.Default()})
	resp, err := client.SendStreaming(context.Background(), "hi", func(d string) {
		deltas = append(deltas, d)
	})
	if err != nil {
		t.Fatalf("SendStreaming: %v", err)
	}
	if len(deltas) != 1 || deltas[0] != "x" {
		t.Fatalf("deltas = %v, want [x]", deltas)
	}
	if resp.Usage.TotalTokens != 1 {
		t.Fatalf("tokens = %d, want 1", resp.Usage.TotalTokens)
	}
}

func TestSendStreaming_UnexpectedEOF(t *testing.T) {
	server := newSSEServer(t, func(w http.ResponseWriter, flusher http.Flusher) {
		// Emit a delta but no terminal event, then close the stream.
		fmt.Fprint(w, sseEvent("response.output_text.delta", `{"delta":"partial"}`))
		flusher.Flush()
	})
	defer server.Close()

	client := New(Config{URL: server.URL, APIKey: "test", Model: "test", Logger: slog.Default()})
	_, err := client.SendStreaming(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("expected error for stream without terminal event, got nil")
	}
	if !strings.Contains(err.Error(), "terminal event") {
		t.Fatalf("err = %v, want contains 'terminal event'", err)
	}
}
