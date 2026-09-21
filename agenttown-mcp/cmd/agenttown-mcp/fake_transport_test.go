package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/AgentTown/agenttown-mcp/contract"
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// fakeTransport is a contract.Transport test double. It records every
// outbound call so tests can assert on what the decision layer dispatched,
// and can be toggled connected/disconnected to exercise the guards in
// guardedExecutor / dialogueRunner / reactiveRunner without a real WS server.
//
// Recording is mutex-guarded: the world_event force path dispatches async
// goroutines (replan / stop retries) that write concurrently with the test
// goroutine. Recorded slices are still read directly by tests — do so only
// after synchronizing (e.g. waiting for the goroutine's terminal state).
type fakeTransport struct {
	connected bool

	mu            sync.Mutex
	sentActions   []sentAction
	stopActionIDs []string
	scanRequests  []string
	envelopes     []string

	msgHandler  contract.MessageHandler
	discHandler contract.DisconnectHandler

	sendActionErr error
	sendActionAck *protocol.ActionStartedPayload
}

type sentAction struct {
	agentID   string
	cmd       string
	params    map[string]any
	autoQueue bool
}

func (f *fakeTransport) SendAction(_ context.Context, agentID, cmd string, params map[string]any, autoQueue bool) (*protocol.ActionStartedPayload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentActions = append(f.sentActions, sentAction{agentID: agentID, cmd: cmd, params: params, autoQueue: autoQueue})
	if f.sendActionErr != nil {
		return nil, f.sendActionErr
	}
	return f.sendActionAck, nil
}

func (f *fakeTransport) RequestScan(_ context.Context, _ string, scanID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scanRequests = append(f.scanRequests, scanID)
	return nil
}

func (f *fakeTransport) SendStopAction(_ string, actionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopActionIDs = append(f.stopActionIDs, actionID)
	return nil
}

func (f *fakeTransport) SendEnvelope(_ string, msgType string, payload any) error {
	b, _ := json.Marshal(payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envelopes = append(f.envelopes, msgType+":"+string(b))
	return nil
}

func (f *fakeTransport) IsConnected() bool { return f.connected }

func (f *fakeTransport) SetMessageHandler(h contract.MessageHandler) { f.msgHandler = h }
func (f *fakeTransport) SetDisconnectHandler(h contract.DisconnectHandler) {
	f.discHandler = h
}

// Compile-time assertion: fakeTransport satisfies the Transport boundary.
var _ contract.Transport = (*fakeTransport)(nil)
