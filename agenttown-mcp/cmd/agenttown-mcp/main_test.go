package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAgentContext_PerceptionLatestWins(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	first := json.RawMessage(`{"sequence":"first"}`)
	latest := json.RawMessage(`{"sequence":"latest"}`)

	if replaced := ac.enqueuePerception(first); replaced {
		t.Fatal("first enqueue unexpectedly replaced a pending perception")
	}
	if replaced := ac.enqueuePerception(latest); !replaced {
		t.Fatal("second enqueue did not report replacing the pending perception")
	}

	got := ac.takePerception()
	if string(got) != string(latest) {
		t.Fatalf("takePerception = %s, want latest %s", got, latest)
	}
	if got := ac.takePerception(); got != nil {
		t.Fatalf("second takePerception = %s, want nil", got)
	}
}

func TestAgentContext_StopClearsPendingPerception(t *testing.T) {
	ac, ctx := newAgentContext(context.Background())
	ac.enqueuePerception(json.RawMessage(`{"sequence":"pending"}`))

	ac.stop()
	if ctx.Err() == nil {
		t.Fatal("worker context was not canceled")
	}
	if got := ac.takePerception(); got != nil {
		t.Fatalf("pending perception survived stop: %s", got)
	}
	if replaced := ac.enqueuePerception(json.RawMessage(`{"sequence":"after-stop"}`)); replaced {
		t.Fatal("enqueue after stop reported a replacement")
	}
	if got := ac.takePerception(); got != nil {
		t.Fatalf("enqueue after stop was accepted: %s", got)
	}
}
