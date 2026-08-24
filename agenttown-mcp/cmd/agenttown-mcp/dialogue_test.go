package main

import (
	"context"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
	"log/slog"
)

// fakeDialogueLLM implements llmClient for dialogue runner tests. Returns a
// canned response (or error) for SendWithSummary; SendStreaming is unused.
type fakeDialogueLLM struct {
	resp       *llmtypes.Response
	err        error
	resetCount int
}

func (f *fakeDialogueLLM) SendWithSummary(_ context.Context, _, _ string) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeDialogueLLM) SendStreaming(_ context.Context, _, _ string, _ func(string)) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeDialogueLLM) SendWithSchema(_ context.Context, _, _, _ string, _ []byte) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeDialogueLLM) SendWithSummaryTools(_ context.Context, _, _ string, _ []venus.Tool) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeDialogueLLM) SendStreamingTools(_ context.Context, _, _ string, _ []venus.Tool, _ func(string)) (*llmtypes.Response, error) {
	return f.resp, f.err
}

func (f *fakeDialogueLLM) ResetSession() { f.resetCount++ }

// makeDialogueResponse builds an llmtypes.Response whose ExtractText returns
// the given raw JSON string.
func makeDialogueResponse(text string) *llmtypes.Response {
	return &llmtypes.Response{
		Status: "completed",
		Output: []llmtypes.Block{{
			Type: "message",
			Role: "assistant",
			Content: []llmtypes.Content{{
				Type: "output_text",
				Text: text,
			}},
		}},
	}
}

// newTestDialogueRunner constructs a dialogueRunner wired to a minimal
// agentContext (NoopStore, unconnected ws, fake LLM). Returns the runner,
// the agentContext (for assertions), and the fake LLM (to swap responses).
func newTestDialogueRunner(agentID string) (*dialogueRunner, *agentContext, *fakeDialogueLLM) {
	ws := wsserver.New(wsserver.Options{}) // unconnected — SendEnvelope errors but doesn't panic
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{
			{ID: "H-01", DisplayName: "老陈"},
			{ID: "H-02", DisplayName: "老王"},
		},
	}
	as := agentstate.New()
	as.SetIdentity(agentID, storage.NoopStore{})
	fake := &fakeDialogueLLM{}
	ac := &agentContext{
		as:         as,
		tacticalHc: fake,
	}
	d := newDialogueRunner(ac, ws, kb, nil, slog.Default())
	ac.dialogue = d
	return d, ac, fake
}

func TestDialogueRunner_ActiveInitiallyFalse(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-02")
	if d.active() {
		t.Error("new runner should not be active")
	}
}

func TestDialogueRunner_HandleInvite_Accept(t *testing.T) {
	d, ac, fake := newTestDialogueRunner("H-02")
	fake.resp = makeDialogueResponse(`{"accept": true, "reply": "好啊，歇会儿"}`)

	d.handleInvite(context.Background(), protocol.ChatInvitePayload{
		ConvID: "conv-1", FromAgentID: "H-01", Content: "最近怎么样？",
	})

	if !d.active() {
		t.Fatal("after accept invite, runner should be active")
	}
	d.mu.Lock()
	conv := d.convID
	peer := d.peerID
	role := d.role
	phase := d.phase
	ctx := d.shortTermContext
	d.mu.Unlock()
	if conv != "conv-1" {
		t.Errorf("convID: got %q, want conv-1", conv)
	}
	if peer != "H-01" {
		t.Errorf("peerID: got %q, want H-01", peer)
	}
	if role != roleTarget {
		t.Errorf("role: got %q, want target", role)
	}
	if phase != phaseActive {
		t.Errorf("phase: got %q, want active", phase)
	}
	// Context should have A's opening + B's reply (2 entries).
	if len(ctx) != 2 {
		t.Fatalf("shortTermContext len: got %d, want 2", len(ctx))
	}
	if ctx[0].SpeakerID != "H-01" || ctx[0].Content != "最近怎么样？" {
		t.Errorf("ctx[0]: got %+v", ctx[0])
	}
	if ctx[1].SpeakerID != "H-02" || ctx[1].Content != "好啊，歇会儿" {
		t.Errorf("ctx[1]: got %+v", ctx[1])
	}
	if fake.resetCount == 0 {
		t.Error("ResetSession should be called after LLM call")
	}
	_ = ac // keep ac referenced
}

func TestDialogueRunner_HandleInvite_Reject(t *testing.T) {
	d, _, fake := newTestDialogueRunner("H-02")
	fake.resp = makeDialogueResponse(`{"accept": false, "reply": ""}`)

	d.handleInvite(context.Background(), protocol.ChatInvitePayload{
		ConvID: "conv-2", FromAgentID: "H-01", Content: "聊聊？",
	})

	if d.active() {
		t.Error("after reject invite, runner should not be active")
	}
	d.mu.Lock()
	phase := d.phase
	conv := d.convID
	d.mu.Unlock()
	if phase != phaseNone {
		t.Errorf("phase: got %q, want none", phase)
	}
	if conv != "" {
		t.Errorf("convID should be cleared, got %q", conv)
	}
}

func TestDialogueRunner_HandleInvite_LLMError_DefaultsReject(t *testing.T) {
	d, _, fake := newTestDialogueRunner("H-02")
	fake.err = errFakeLLM

	d.handleInvite(context.Background(), protocol.ChatInvitePayload{
		ConvID: "conv-3", FromAgentID: "H-01", Content: "聊？",
	})

	if d.active() {
		t.Error("on LLM error, runner should reject and not be active")
	}
}

func TestDialogueRunner_HandleInviteRsp_Reject(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-01")
	// Seed as initiator waiting for response.
	d.mu.Lock()
	d.convID = "conv-4"
	d.peerID = "H-02"
	d.role = roleInitiator
	d.phase = phaseInviting
	d.mu.Unlock()

	d.handleInviteRsp(context.Background(), protocol.ChatInviteRspPayload{
		ConvID: "conv-4", Accept: false,
	})

	if d.active() {
		t.Error("after reject rsp, runner should not be active")
	}
}

func TestDialogueRunner_HandleInviteRsp_Accept(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-01")
	d.mu.Lock()
	d.convID = "conv-5"
	d.peerID = "H-02"
	d.role = roleInitiator
	d.phase = phaseInviting
	d.mu.Unlock()

	d.handleInviteRsp(context.Background(), protocol.ChatInviteRspPayload{
		ConvID: "conv-5", Accept: true,
	})

	d.mu.Lock()
	phase := d.phase
	d.mu.Unlock()
	if phase != phaseActive {
		t.Errorf("after accept rsp, phase: got %q, want active", phase)
	}
}

func TestDialogueRunner_HandleInviteRsp_UnexpectedConv_Ignored(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-01")
	d.mu.Lock()
	d.convID = "conv-expected"
	d.peerID = "H-02"
	d.role = roleInitiator
	d.phase = phaseInviting
	d.mu.Unlock()

	d.handleInviteRsp(context.Background(), protocol.ChatInviteRspPayload{
		ConvID: "conv-wrong", Accept: true,
	})

	d.mu.Lock()
	phase := d.phase
	d.mu.Unlock()
	if phase != phaseInviting {
		t.Errorf("unexpected conv should be ignored, phase: got %q, want inviting", phase)
	}
}

func TestDialogueRunner_HandleTurn_PeerEnd_Cleanup(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-6"
	d.peerID = "H-01"
	d.role = roleTarget
	d.phase = phaseActive
	d.shortTermContext = []prompt.DialogueTurnEntry{{SpeakerID: "H-01", Content: "hi"}}
	d.mu.Unlock()

	d.handleTurn(context.Background(), protocol.ChatTurnPayload{
		ConvID: "conv-6", Content: "回头聊", End: true,
	})

	if d.active() {
		t.Error("after peer end turn, runner should not be active")
	}
}

func TestDialogueRunner_HandleTurn_UnexpectedConv_Ignored(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-mine"
	d.peerID = "H-01"
	d.phase = phaseActive
	d.mu.Unlock()

	// Turn for a different conv → ignored, no LLM call, state unchanged.
	d.handleTurn(context.Background(), protocol.ChatTurnPayload{
		ConvID: "conv-other", Content: "hello",
	})

	d.mu.Lock()
	conv := d.convID
	phase := d.phase
	d.mu.Unlock()
	if conv != "conv-mine" {
		t.Errorf("unexpected conv turn should not change convID: got %q", conv)
	}
	if phase != phaseActive {
		t.Errorf("phase should stay active: got %q", phase)
	}
}

func TestDialogueRunner_HandleTurn_NormalReply(t *testing.T) {
	d, _, fake := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-7"
	d.peerID = "H-01"
	d.role = roleTarget
	d.phase = phaseActive
	d.mu.Unlock()
	fake.resp = makeDialogueResponse(`{"content": "还行，就是有点累", "end": false}`)

	d.handleTurn(context.Background(), protocol.ChatTurnPayload{
		ConvID: "conv-7", Content: "最近忙啥呢？",
	})

	d.mu.Lock()
	phase := d.phase
	ctx := d.shortTermContext
	d.mu.Unlock()
	if phase != phaseActive {
		t.Errorf("after normal reply, phase: got %q, want active", phase)
	}
	// Context should have peer's turn + our reply.
	if len(ctx) < 2 {
		t.Fatalf("context should have ≥2 entries, got %d", len(ctx))
	}
	last := ctx[len(ctx)-1]
	if last.SpeakerID != "H-02" || last.Content != "还行，就是有点累" {
		t.Errorf("last context entry should be our reply: got %+v", last)
	}
}

func TestDialogueRunner_HandleTurn_LLMEnds(t *testing.T) {
	d, _, fake := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-8"
	d.peerID = "H-01"
	d.phase = phaseActive
	d.mu.Unlock()
	fake.resp = makeDialogueResponse(`{"content": "先这样吧", "end": true}`)

	d.handleTurn(context.Background(), protocol.ChatTurnPayload{
		ConvID: "conv-8", Content: "嗯",
	})

	if d.active() {
		t.Error("after LLM ends, runner should not be active")
	}
}

func TestDialogueRunner_OnActionCompleted_Interrupted(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-9"
	d.peerID = "H-01"
	d.phase = phaseActive
	d.mu.Unlock()

	d.onActionCompleted(protocol.ActionCompletedPayload{Result: "interrupted"},
		agentstate.CompletionResult{WasInFlight: true, Cmd: protocol.CmdSocialChat})

	if d.active() {
		t.Error("after interrupted completion, runner should not be active")
	}
}

func TestDialogueRunner_OnActionCompleted_NonSocialChat_Ignored(t *testing.T) {
	d, _, _ := newTestDialogueRunner("H-02")
	d.mu.Lock()
	d.convID = "conv-10"
	d.peerID = "H-01"
	d.phase = phaseActive
	d.mu.Unlock()

	d.onActionCompleted(protocol.ActionCompletedPayload{Result: "success"},
		agentstate.CompletionResult{WasInFlight: true, Cmd: "MoveTo"})

	if !d.active() {
		t.Error("non-social_chat completion should not affect dialogue state")
	}
}

func TestBuildTranscript_TruncatesAndJoins(t *testing.T) {
	ctx := []prompt.DialogueTurnEntry{
		{SpeakerID: "H-01", SpeakerName: "老陈", Content: "你好"},
		{SpeakerID: "H-02", SpeakerName: "老王", Content: "你也好"},
	}
	got := buildTranscript(ctx, 10)
	want := "老陈：你好 | 老王：你也好"
	if got != want {
		t.Errorf("buildTranscript: got %q, want %q", got, want)
	}

	// Truncation: only keep last 1.
	got = buildTranscript(ctx, 1)
	if got != "老王：你也好" {
		t.Errorf("truncated: got %q, want 老王：你也好", got)
	}

	// Empty.
	if buildTranscript(nil, 10) != "（无内容）" {
		t.Error("empty transcript should be 无内容")
	}
}

func TestOpeningContent(t *testing.T) {
	if openingContent(nil) != "" {
		t.Error("nil params should return empty")
	}
	if openingContent(map[string]any{}) != "" {
		t.Error("no content key should return empty")
	}
	if got := openingContent(map[string]any{"content": "开场白"}); got != "开场白" {
		t.Errorf("got %q, want 开场白", got)
	}
	if got := openingContent(map[string]any{"content": 123}); got != "" {
		t.Errorf("non-string content should return empty, got %q", got)
	}
}

// errFakeLLM is a sentinel error for fakeDialogueLLM.err.
var errFakeLLM = fakeLLMErr("llm unavailable")

type fakeLLMErr string

func (e fakeLLMErr) Error() string { return string(e) }
