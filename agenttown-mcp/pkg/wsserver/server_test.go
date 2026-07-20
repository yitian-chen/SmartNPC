package wsserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// newTestServer builds a Server without starting the HTTP listener.
func newTestServer() *Server {
	return New(Options{Addr: ":0"})
}

// TestBufferOutbound_LengthEviction verifies the send buffer keeps at most
// sendBufferMaxLen discrete messages, dropping the oldest.
func TestBufferOutbound_LengthEviction(t *testing.T) {
	s := newTestServer()
	total := sendBufferMaxLen + 50
	for i := 1; i <= total; i++ {
		s.bufferOutbound(int64(i), []byte("frame"))
	}
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if len(s.sendBuf) != sendBufferMaxLen {
		t.Fatalf("buffer len = %d, want %d", len(s.sendBuf), sendBufferMaxLen)
	}
	wantOldest := int64(total - sendBufferMaxLen + 1)
	if s.sendBuf[0].seq != wantOldest {
		t.Fatalf("oldest seq = %d, want %d", s.sendBuf[0].seq, wantOldest)
	}
	if s.sendBuf[len(s.sendBuf)-1].seq != int64(total) {
		t.Fatalf("newest seq = %d, want %d", s.sendBuf[len(s.sendBuf)-1].seq, total)
	}
}

// TestBufferOutbound_AgeEviction verifies age-based eviction.
func TestBufferOutbound_AgeEviction(t *testing.T) {
	s := newTestServer()
	// Inject two stale entries and one fresh one directly.
	old := time.Now().Add(-2 * sendBufferMaxAge)
	s.sendBuf = []bufferedMsg{
		{seq: 1, frame: []byte("a"), at: old},
		{seq: 2, frame: []byte("b"), at: old},
	}
	s.bufferOutbound(3, []byte("c")) // fresh; triggers evictLocked
	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	if len(s.sendBuf) != 1 || s.sendBuf[0].seq != 3 {
		t.Fatalf("after age eviction got %+v, want only seq 3", s.sendBuf)
	}
}

// TestReplayFrom_SelectsNewer verifies replayFrom re-sends only discrete
// messages with seq > peerLastSeq, in order, over a live connection.
func TestReplayFrom_SelectsNewer(t *testing.T) {
	s := newTestServer()

	srvConn, cliConn := wsPipe(t, s)
	defer srvConn.Close(websocket.StatusNormalClosure, "")
	defer cliConn.Close(websocket.StatusNormalClosure, "")

	// Send four discrete action_commands (seq 1..4 via SendEnvelope).
	for i := 0; i < 4; i++ {
		if err := s.SendEnvelope("H-01", protocol.TypeActionCommand, protocol.ActionCommandPayload{
			ActionID: "act", Cmd: protocol.CmdWait, Params: map[string]any{"n": i},
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	// Drain the four live sends from the client side.
	for i := 0; i < 4; i++ {
		readEnvelope(t, cliConn)
	}

	// Peer says it last received seq 2 → expect replay of seq 3 and 4.
	s.replayFrom(2)

	got := []int64{}
	for i := 0; i < 2; i++ {
		env := readEnvelope(t, cliConn)
		got = append(got, env.Seq)
	}
	if len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("replayed seqs = %v, want [3 4]", got)
	}
}

// TestReplayFrom_Rollover verifies an event_lost is emitted when the buffer
// has rolled past the peer's resume point.
func TestReplayFrom_Rollover(t *testing.T) {
	s := newTestServer()

	srvConn, cliConn := wsPipe(t, s)
	defer srvConn.Close(websocket.StatusNormalClosure, "")
	defer cliConn.Close(websocket.StatusNormalClosure, "")

	// Simulate a buffer whose oldest retained seq is 100 (everything before
	// was evicted). Bump the outbound counter so event_lost gets a fresh seq.
	s.seq = 105
	s.bufMu.Lock()
	s.sendBuf = []bufferedMsg{
		{seq: 100, frame: []byte(`{"seq":100}`), at: time.Now()},
		{seq: 101, frame: []byte(`{"seq":101}`), at: time.Now()},
	}
	s.bufMu.Unlock()

	// Peer only got up to seq 10 → 11..99 are lost.
	s.replayFrom(10)

	// First message out should be an event_lost warning.
	env := readEnvelope(t, cliConn)
	if env.Type != protocol.TypeEventLost {
		t.Fatalf("first replayed type = %q, want event_lost", env.Type)
	}
	var el protocol.EventLostPayload
	if err := json.Unmarshal(env.Payload, &el); err != nil {
		t.Fatalf("event_lost parse: %v", err)
	}
	if el.FromSeq != 11 || el.ToSeq != 100 {
		t.Fatalf("event_lost range = [%d,%d], want [11,100]", el.FromSeq, el.ToSeq)
	}
	if el.Count != 89 { // 100 - 11
		t.Fatalf("event_lost count = %d, want 89", el.Count)
	}
}

func TestRequestScan_SendsScanID(t *testing.T) {
	s := newTestServer()
	srvConn, cliConn := wsPipe(t, s)
	defer srvConn.Close(websocket.StatusNormalClosure, "")
	defer cliConn.Close(websocket.StatusNormalClosure, "")

	if err := s.RequestScan(context.Background(), "H-01", "scan_123"); err != nil {
		t.Fatal(err)
	}
	env := readEnvelope(t, cliConn)
	if env.Type != protocol.TypeScanArea || env.AgentID != "H-01" {
		t.Fatalf("scan envelope=%+v", env)
	}
	var payload protocol.ScanAreaPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.ScanID != "scan_123" {
		t.Fatalf("scan payload=%+v err=%v", payload, err)
	}
}

func TestDeliverACK_RejectsWrongAgent(t *testing.T) {
	s := newTestServer()
	ch := make(chan *pendingResult, 1)
	s.pending["act_1"] = &pendingCall{agentID: "H-01", ch: ch}
	ack := &protocol.ActionStartedPayload{ActionID: "act_1", Accepted: true}

	s.deliverACK("H-02", ack)
	select {
	case <-ch:
		t.Fatal("ACK from wrong agent was delivered")
	default:
	}

	s.deliverACK("H-01", ack)
	select {
	case got := <-ch:
		if got.started.ActionID != "act_1" {
			t.Fatalf("wrong ACK delivered: %+v", got.started)
		}
	default:
		t.Fatal("ACK from expected agent was not delivered")
	}
}

// TestObserveInboundSeq verifies only-increasing tracking.
func TestObserveInboundSeq(t *testing.T) {
	s := newTestServer()
	s.observeInboundSeq(5)
	s.observeInboundSeq(3) // lower, ignored
	s.observeInboundSeq(9)
	if got := s.lastReceivedSeq; got != 9 {
		t.Fatalf("lastReceivedSeq = %d, want 9", got)
	}
}

// ─── test helpers ───────────────────────────────────────────────

// wsPipe wires a websocket pair via an httptest server whose handler
// registers the accepted connection on s (mirroring handleWS) and sends the
// initial resync, but does NOT run the read loop — the test controls reads
// from the client side.
func wsPipe(t *testing.T, s *Server) (srv *websocket.Conn, cli *websocket.Conn) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		s.mu.Lock()
		s.conn = c
		s.mu.Unlock()
		// Mirror handleWS: send the reconnect resync on connect.
		_ = s.SendEnvelope(protocol.SystemAgentID, protocol.TypeResync, protocol.ResyncPayload{
			LastReceivedSeq: s.lastReceivedSeq,
		})
		// Hold the handler open so the connection stays alive for the test.
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.IsConnected() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !s.IsConnected() {
		t.Fatal("server never registered connection")
	}
	s.mu.RLock()
	srv = s.conn
	s.mu.RUnlock()

	// Drain the auto-sent resync frame.
	readEnvelope(t, c)
	return srv, c
}

func readEnvelope(t *testing.T, c *websocket.Conn) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}
