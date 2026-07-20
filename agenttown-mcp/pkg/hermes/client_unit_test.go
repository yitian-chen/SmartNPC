package hermes

import (
	"context"
	"encoding/json"
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
