//go:build integration

// Integration test against a live Hermes Gateway at localhost:8642.
// Run with: go test -tags integration ./pkg/hermes/ -v -count=1 -timeout 120s
//
// Requires Hermes Gateway running with API key "agenttown-test-key" and
// model "deepseek-v4-flash" configured.
package hermes

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestClient_SendSessionChaining(t *testing.T) {
	c := New(Config{
		URL:    "http://localhost:8642",
		APIKey: "agenttown-test-key",
		Model:  "deepseek-v4-flash",
		Logger: slog.Default(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// First call — no session id yet.
	r1, err := c.Send(ctx, "Hello, who are you?")
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	id1 := c.SessionID()
	if id1 == "" {
		t.Fatal("SessionID empty after first Send")
	}
	t.Logf("1: id=%s tokens=%d text=%q", r1.ID, r1.Usage.TotalTokens, trunc(r1.ExtractText(), 80))

	// Second call — should chain via previous_response_id.
	r2, err := c.Send(ctx, "Reply OK")
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if c.SessionID() == id1 {
		t.Fatal("SessionID did not advance after second Send")
	}
	t.Logf("2: id=%s tokens=%d text=%q", r2.ID, r2.Usage.TotalTokens, trunc(r2.ExtractText(), 80))

	// Reset.
	c.ResetSession()
	if c.SessionID() != "" {
		t.Fatalf("SessionID not cleared after reset: %q", c.SessionID())
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
