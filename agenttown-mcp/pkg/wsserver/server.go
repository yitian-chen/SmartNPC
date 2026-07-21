// Package wsserver hosts the WebSocket server that Mock UE connects to.
package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// Default timeouts.
const (
	defaultCallTimeout = 30 * time.Second
	defaultWriteWait   = 5 * time.Second
)

// Send-buffer retention for reconnect replay (约定11, §4.2).
const (
	sendBufferMaxLen = 200              // keep at most 200 discrete messages
	sendBufferMaxAge = 60 * time.Second // or messages from the last 60s
)

// discreteReplayTypes are the outbound message types eligible for seq-replay
// after a reconnect. Continuous state (perception_update/state_report) and
// pure liveness (heartbeat) are NOT replayed — the peer uses the latest
// snapshot instead (约定11).
var discreteReplayTypes = map[string]bool{
	protocol.TypeActionCommand:     true,
	protocol.TypeStopAction:        true,
	protocol.TypeEventNotification: true,
	protocol.TypeError:             true,
}

// MessageHandler receives inbound envelopes from Mock UE (UE → Agent).
// Called from the read loop, so long work should be offloaded to a goroutine.
// It receives the message type, agent_id, and raw payload.
type MessageHandler func(ctx context.Context, msgType, agentID string, payload json.RawMessage)

// Options configures New.
type Options struct {
	Addr        string
	Logger      *slog.Logger
	CallTimeout time.Duration
}

// bufferedMsg is a discrete outbound message retained for reconnect replay.
type bufferedMsg struct {
	seq   int64
	frame []byte
	at    time.Time
}

// Server is a WebSocket server accepting a connection from Mock UE.
//
// Phase 1: single connection (one NPC). agent_id routing is threaded through
// so multi-NPC is a natural extension. seq is a per-server monotonic counter
// for outbound messages.
//
// Phase 7: outbound discrete messages are kept in a rolling send buffer;
// lastReceivedSeq tracks the highest inbound seq. On reconnect the two sides
// exchange resync{last_received_seq} and replay the discrete messages the
// peer missed (约定11).
type Server struct {
	addr        string
	log         *slog.Logger
	callTimeout time.Duration

	seq             int64 // outbound sequence counter (atomic)
	lastReceivedSeq int64 // highest inbound seq seen (atomic)

	mu      sync.RWMutex
	conn    *websocket.Conn
	lastHeartbeatAt time.Time // 最近收到 UE 心跳的时间（mu 保护），用于 15s 超时检测
	pending map[string]*pendingCall // keyed by action_id (msg correlation)

	bufMu   sync.Mutex
	sendBuf []bufferedMsg // rolling buffer of discrete outbound messages

	handlerMu sync.RWMutex
	handler   MessageHandler
}

type pendingCall struct {
	agentID string
	ch      chan *pendingResult
}

// pendingResult carries a correlated response back to a waiting Call.
// Phase 1 uses action_started as the correlation signal; on ACK we deliver
// the ActionStartedPayload.
type pendingResult struct {
	started *protocol.ActionStartedPayload
	errMsg  string
}

// New creates an unstarted server. Call Serve to begin accepting connections.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	return &Server{
		addr:        opts.Addr,
		log:         opts.Logger,
		callTimeout: opts.CallTimeout,
		pending:     make(map[string]*pendingCall),
	}
}

// SetMessageHandler registers the callback for inbound envelopes.
func (s *Server) SetMessageHandler(h MessageHandler) {
	s.handlerMu.Lock()
	s.handler = h
	s.handlerMu.Unlock()
}

// nextSeq returns the next outbound sequence number.
func (s *Server) nextSeq() int64 {
	return atomic.AddInt64(&s.seq, 1)
}

// Serve starts the HTTP server with the WebSocket endpoint at /ws and a
// health endpoint at /healthz. Blocks until ctx is canceled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"ws_connected":%v}`, s.IsConnected())))
	})
	mux.HandleFunc("/ws", s.handleWS)

	httpServer := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	s.log.Info("ws server listening", "addr", s.addr, "endpoint", "/ws")
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// handleWS upgrades the HTTP connection to WebSocket and runs the read loop.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Warn("ws accept failed", "err", err)
		return
	}
	c.SetReadLimit(1 << 20) // 1 MiB

	s.mu.Lock()
	if s.conn != nil {
		s.log.Info("replacing existing ws connection")
		_ = s.conn.Close(websocket.StatusNormalClosure, "replaced")
	}
	s.conn = c
	s.mu.Unlock()

	s.log.Info("mock ue connected", "remote", r.RemoteAddr)

	// 初始化心跳时间戳，启出站心跳 ticker（约定 §5.2：双向每 5s + 15s 超时）
	s.mu.Lock()
	s.lastHeartbeatAt = time.Now()
	s.mu.Unlock()

	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go s.heartbeatLoop(hbCtx)

	// Phase 7: on (re)connect, tell the peer the last inbound seq we saw so
	// it can replay anything we missed. Also proactively replay our own
	// buffered discrete messages once the peer sends its resync.
	if err := s.SendEnvelope(protocol.SystemAgentID, protocol.TypeResync, protocol.ResyncPayload{
		LastReceivedSeq: atomic.LoadInt64(&s.lastReceivedSeq),
	}); err != nil {
		s.log.Warn("resync send failed", "err", err)
	}

	readCtx := context.Background()
	defer func() {
		s.mu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		s.mu.Unlock()
		_ = c.CloseNow()
		s.log.Info("mock ue disconnected", "remote", r.RemoteAddr)
	}()

	s.readLoop(readCtx, c)
}

// readLoop reads envelopes and dispatches by type.
func (s *Server) readLoop(ctx context.Context, c *websocket.Conn) {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
				return
			}
			s.log.Debug("ws read ended", "err", err)
			return
		}

		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			s.log.Warn("envelope parse failed", "raw", string(data), "err", err)
			continue
		}

		// Log inbound messages from UE. Heartbeat is silent; all other
		// types (including high-frequency perception_update / state_report)
		// log the full payload so scan_area results and state deltas are
		// fully visible in sim.log.
		switch env.Type {
		case protocol.TypeHeartbeat:
			// 更新心跳时间戳（用于 15s 超时检测），保持静默不日志
			s.mu.Lock()
			s.lastHeartbeatAt = time.Now()
			s.mu.Unlock()
		default:
			s.log.Info("[UE→MCP]", "type", env.Type, "seq", env.Seq, "agent_id", env.AgentID, "payload", string(env.Payload))
		}

		// Track the highest inbound seq for reconnect replay (约定11).
		// resync/event_lost are control messages and don't advance it.
		if env.Type != protocol.TypeResync && env.Type != protocol.TypeEventLost {
			s.observeInboundSeq(env.Seq)
		}

		switch env.Type {
		case protocol.TypeActionStarted:
			// ACK for an action_command — deliver to the waiting Call.
			var ack protocol.ActionStartedPayload
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				s.log.Warn("action_started parse failed", "err", err)
				continue
			}
			s.deliverACK(env.AgentID, &ack)
		case protocol.TypeResync:
			// Peer told us the last seq it received — replay what it missed.
			var rs protocol.ResyncPayload
			if err := json.Unmarshal(env.Payload, &rs); err != nil {
				s.log.Warn("resync parse failed", "err", err)
				continue
			}
			s.replayFrom(rs.LastReceivedSeq)
		case protocol.TypeEventLost:
			// Peer couldn't replay some discrete messages we sent it.
			var el protocol.EventLostPayload
			_ = json.Unmarshal(env.Payload, &el)
			s.log.Warn("peer reported event_lost",
				"from_seq", el.FromSeq, "to_seq", el.ToSeq, "count", el.Count, "reason", el.Reason)
		default:
			// All other inbound messages go to the registered handler:
			// perception_update, action_completed, state_report,
			// agent_registered, agent_unregistered, heartbeat, error.
			s.dispatch(ctx, env.Type, env.AgentID, env.Payload)
		}
	}
}

// observeInboundSeq records the highest inbound seq seen so far.
func (s *Server) observeInboundSeq(seq int64) {
	for {
		cur := atomic.LoadInt64(&s.lastReceivedSeq)
		if seq <= cur {
			return
		}
		if atomic.CompareAndSwapInt64(&s.lastReceivedSeq, cur, seq) {
			return
		}
	}
}

// deliverACK hands an action_started to the goroutine waiting on its action_id.
func (s *Server) deliverACK(agentID string, ack *protocol.ActionStartedPayload) {
	s.mu.RLock()
	pending, ok := s.pending[ack.ActionID]
	s.mu.RUnlock()
	if !ok {
		s.log.Warn("[UE→MCP/ACK] action_started with no pending caller", "action_id", ack.ActionID)
		return
	}
	if pending.agentID != agentID {
		s.log.Warn("[UE→MCP/ACK] agent_id mismatch",
			"action_id", ack.ActionID, "expected_agent_id", pending.agentID, "actual_agent_id", agentID)
		return
	}
	ch := pending.ch
	s.log.Info("[UE→MCP/ACK]", "agent_id", agentID, "action_id", ack.ActionID, "accepted", ack.Accepted, "est_duration_sec", ack.EstimatedDurationSec)
	result := &pendingResult{started: ack}
	if !ack.Accepted {
		result.errMsg = ack.RejectReason
		if result.errMsg == "" {
			result.errMsg = "action rejected"
		}
	}
	select {
	case ch <- result:
	default:
		s.log.Warn("ack channel full, dropping", "action_id", ack.ActionID)
	}
}

// dispatch invokes the registered handler for an inbound envelope.
func (s *Server) dispatch(ctx context.Context, msgType, agentID string, payload json.RawMessage) {
	s.handlerMu.RLock()
	h := s.handler
	s.handlerMu.RUnlock()
	if h == nil {
		s.log.Debug("envelope dropped (no handler)", "type", msgType)
		return
	}
	h(ctx, msgType, agentID, payload)
}

// SendEnvelope sends an arbitrary envelope to the connected Mock UE.
// Discrete message types are retained in the send buffer for reconnect
// replay (约定11).
func (s *Server) SendEnvelope(agentID, msgType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	seq := s.nextSeq()
	env := protocol.Envelope{
		Version:   protocol.Version,
		MsgID:     uuid.NewString(),
		Seq:       seq,
		Timestamp: time.Now().UnixMilli(),
		Type:      msgType,
		AgentID:   agentID,
		Payload:   raw,
	}
	frame, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	// Retain discrete messages for potential replay before attempting the
	// write, so a message that fails mid-send is still replayable.
	if discreteReplayTypes[msgType] {
		s.bufferOutbound(seq, frame)
	}

	// Log outbound envelopes. Full payload for low-frequency types;
	// compact for heartbeat/resync (narrative text is logged separately
	// at main.go:525 as [Hermes→MCP/RESPONSE]).
	switch msgType {
	case protocol.TypeHeartbeat, protocol.TypeResync:
		s.log.Debug("[MCP→UE]", "type", msgType, "seq", seq, "agent_id", agentID)
	default:
		s.log.Info("[MCP→UE]", "type", msgType, "seq", seq, "agent_id", agentID, "payload", string(raw))
	}

	return s.writeFrame(frame)
}

// writeFrame writes a pre-marshaled envelope frame to the current connection.
func (s *Server) writeFrame(frame []byte) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return errors.New("no mock ue connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWriteWait)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, frame)
}

// bufferOutbound appends a discrete message to the rolling send buffer and
// evicts entries beyond the length/age retention window.
func (s *Server) bufferOutbound(seq int64, frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)

	s.bufMu.Lock()
	defer s.bufMu.Unlock()
	s.sendBuf = append(s.sendBuf, bufferedMsg{seq: seq, frame: cp, at: time.Now()})
	s.evictLocked()
}

// evictLocked trims the send buffer to the retention window. Caller holds bufMu.
func (s *Server) evictLocked() {
	cutoff := time.Now().Add(-sendBufferMaxAge)
	// Drop by age.
	i := 0
	for i < len(s.sendBuf) && s.sendBuf[i].at.Before(cutoff) {
		i++
	}
	// Drop by length.
	if overflow := len(s.sendBuf) - i - sendBufferMaxLen; overflow > 0 {
		i += overflow
	}
	if i > 0 {
		s.sendBuf = append(s.sendBuf[:0], s.sendBuf[i:]...)
	}
}

// replayFrom re-sends buffered discrete messages with seq > lastReceivedSeq.
// If the oldest buffered message is newer than lastReceivedSeq+1, some
// messages were lost to buffer rollover — emit an event_lost warning (约定11).
func (s *Server) replayFrom(peerLastSeq int64) {
	s.bufMu.Lock()
	s.evictLocked()
	var toReplay []bufferedMsg
	var oldestSeq int64 = -1
	for _, m := range s.sendBuf {
		if oldestSeq == -1 {
			oldestSeq = m.seq
		}
		if m.seq > peerLastSeq {
			toReplay = append(toReplay, m)
		}
	}
	s.bufMu.Unlock()

	// Detect rollover gap: peer wants everything after peerLastSeq, but our
	// oldest retained seq is already beyond peerLastSeq+1.
	if oldestSeq > 0 && oldestSeq > peerLastSeq+1 {
		lost := oldestSeq - (peerLastSeq + 1)
		s.log.Warn("event_lost: send buffer rolled past peer resume point",
			"peer_last_seq", peerLastSeq, "oldest_buffered_seq", oldestSeq, "lost", lost)
		_ = s.SendEnvelope(protocol.SystemAgentID, protocol.TypeEventLost, protocol.EventLostPayload{
			FromSeq: peerLastSeq + 1,
			ToSeq:   oldestSeq,
			Count:   lost,
			Reason:  "send buffer rollover",
		})
	}

	if len(toReplay) == 0 {
		return
	}
	s.log.Info("replaying discrete messages after reconnect",
		"peer_last_seq", peerLastSeq, "count", len(toReplay))
	for _, m := range toReplay {
		if err := s.writeFrame(m.frame); err != nil {
			s.log.Warn("replay write failed", "seq", m.seq, "err", err)
			return
		}
	}
}

// Call sends an action_command to Mock UE and waits for the action_started
// ACK. Returns the ACK payload (with estimated_duration_sec). This is the
// Phase 1/5 bridge: tools call this and return after ACK; action_completed
// arrives asynchronously and is handled by the message handler.
//
// The `cmd` and `params` form the action_command payload. action_id is
// generated here and returned via the ACK.
func (s *Server) Call(ctx context.Context, agentID, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error) {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("no mock ue connected")
	}

	actionID := "act_" + uuid.NewString()[:12]
	ch := make(chan *pendingResult, 1)

	s.mu.Lock()
	s.pending[actionID] = &pendingCall{agentID: agentID, ch: ch}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, actionID)
		s.mu.Unlock()
	}()

	cmdPayload := protocol.ActionCommandPayload{
		ActionID: actionID,
		Cmd:      cmd,
		Params:   params,
	}
	s.log.Info("[MCP→UE/CMD]", "cmd", cmd, "action_id", actionID, "agent_id", agentID, "params", fmt.Sprint(params))
	if err := s.SendEnvelope(agentID, protocol.TypeActionCommand, cmdPayload); err != nil {
		return nil, fmt.Errorf("send action_command: %w", err)
	}

	// ACK must arrive within 2s (约定8).
	ackTimeout := 2 * time.Second
	timer := time.NewTimer(ackTimeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		if res.errMsg != "" {
			return res.started, fmt.Errorf("action rejected: %s", res.errMsg)
		}
		return res.started, nil
	case <-timer.C:
		return nil, fmt.Errorf("action_started ACK timeout after %s (action_id=%s)", ackTimeout, actionID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// IsConnected reports whether a Mock UE is currently connected.
func (s *Server) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn != nil
}

// SendAction implements the tools.Executor interface: sends an
// action_command and waits for the ACK. It's an alias for Call with the
// signature the tools layer expects.
func (s *Server) SendAction(ctx context.Context, agentID, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error) {
	return s.Call(ctx, agentID, cmd, params)
}

// RequestScan asks Mock UE to emit an immediate perception_update for the
// given agent. scanID is echoed by that response to correlate a one-shot
// scan follow-up decision. Fire-and-forget (no ACK expected).
func (s *Server) RequestScan(ctx context.Context, agentID, scanID string) error {
	return s.SendEnvelope(agentID, protocol.TypeScanArea, protocol.ScanAreaPayload{ScanID: scanID})
}

// heartbeatLoop 每 5s 发出站心跳，并检测 UE 心跳是否超时（约定 §5.2）。
// 15s 未收到 UE 心跳则主动关闭连接，触发 UE 侧重连。
// ctx 取消时退出（连接关闭时通过 hbCancel 触发）。
func (s *Server) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 发出站心跳（agent_id="system"）
			if err := s.SendEnvelope(protocol.SystemAgentID, protocol.TypeHeartbeat, protocol.HeartbeatPayload{}); err != nil {
				s.log.Debug("heartbeat send failed", "err", err)
			}
			// 检测 UE 心跳响应超时（15s）
			s.mu.RLock()
			last := s.lastHeartbeatAt
			s.mu.RUnlock()
			if time.Since(last) > 15*time.Second {
				s.log.Warn("UE heartbeat timeout (>15s), closing connection", "last_heartbeat", last)
				s.mu.Lock()
				if s.conn != nil {
					_ = s.conn.Close(websocket.StatusPolicyViolation, "heartbeat timeout")
				}
				s.mu.Unlock()
				return
			}
		}
	}
}
