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
	// Phase 2 Module C: outbound dialogue messages must survive reconnect
	// so a dropped chat_invite_rsp / chat_turn is re-delivered rather than
	// silently lost (the peer would otherwise hang in Inviting/Active).
	protocol.TypeChatInviteRsp:     true,
	protocol.TypeChatTurn:          true,
}

// MessageHandler receives inbound envelopes from Mock UE (UE → Agent).
// Called from the read loop, so long work should be offloaded to a goroutine.
// It receives the message type, agent_id, and raw payload.
type MessageHandler func(ctx context.Context, msgType, agentID string, payload json.RawMessage)

// DisconnectHandler is invoked once when the current UE WebSocket connection
// closes (read loop returned). It is NOT called when an existing connection
// is replaced by a newer one — only when the active connection itself ends.
// Used by main to stop all agentContexts so workers/reactive-layer goroutines
// don't keep running against a dead UE.
type DisconnectHandler func()

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

	// startedAt is when New was called; used for heartbeat uptime_sec
	// (协议 §2.3 heartbeat payload). 与 lastHeartbeatAt 不同：后者是最近
	// 收到 UE 心跳的时间，前者是进程自身的启动时间戳。
	startedAt time.Time

	seq             int64 // outbound sequence counter (atomic)
	lastReceivedSeq int64 // highest inbound seq seen (atomic)

	// heartbeatsReceived 累计收到的 UE 心跳数（atomic）。用于超时关闭时
	// 诊断：若是 0，说明 UE 从未发心跳；若是 N>0 后停发，说明中途断流。
	heartbeatsReceived int64

	mu              sync.RWMutex
	conn            *websocket.Conn
	lastHeartbeatAt time.Time               // 最近收到 UE 心跳的时间（mu 保护），用于 15s 超时检测
	pending         map[string]*pendingCall // keyed by action_id (msg correlation)

	writeMu sync.Mutex // 串行化 conn.Write，防止流式叙事推送与动作分发并发写坏帧

	bufMu   sync.Mutex
	sendBuf []bufferedMsg // rolling buffer of discrete outbound messages

	handlerMu sync.RWMutex
	handler   MessageHandler

	// onDisconnect is invoked from handleWS's defer when the active
	// connection ends. Guarded by handlerMu (same pattern as handler).
	onDisconnect DisconnectHandler
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
		startedAt:   time.Now(),
		pending:     make(map[string]*pendingCall),
	}
}

// SetMessageHandler registers the callback for inbound envelopes.
func (s *Server) SetMessageHandler(h MessageHandler) {
	s.handlerMu.Lock()
	s.handler = h
	s.handlerMu.Unlock()
}

// SetDisconnectHandler registers a callback invoked when the active UE
// WebSocket connection ends (read loop returned). The handler runs inline
// in the defer so it must return promptly — main uses it to stop agents
// (cancel contexts, stop timers), which is fast.
//
// Not called when an existing connection is replaced by a new one — only
// when the currently-active connection itself closes.
func (s *Server) SetDisconnectHandler(h DisconnectHandler) {
	s.handlerMu.Lock()
	s.onDisconnect = h
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

	s.log.Info("ue connected", "remote", r.RemoteAddr)

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
		// 仅当前活跃连接断开才清 conn + 触发回调。若 s.conn 已被新连接
		// replace（handleWS 顶部把旧 conn Close），此处 s.conn != c，
		// 不应清掉新连接，也不应触发 onDisconnect（新连接仍活着）。
		s.mu.Lock()
		isCurrent := s.conn == c
		if isCurrent {
			s.conn = nil
		}
		s.mu.Unlock()
		_ = c.CloseNow()
		s.log.Info("ue disconnected", "remote", r.RemoteAddr, "is_current", isCurrent)

		if isCurrent {
			s.handlerMu.RLock()
			h := s.onDisconnect
			s.handlerMu.RUnlock()
			if h != nil {
				h()
			}
		}
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
			// 解析 uptime_sec 用于日志诊断（协议 §2.3）。解析失败也不影响
			// 心跳时间戳更新——心跳本质是 liveness 信号，payload 是辅助。
			var hb protocol.HeartbeatPayload
			_ = json.Unmarshal(env.Payload, &hb)
			count := atomic.AddInt64(&s.heartbeatsReceived, 1)
			// 心跳静默是历史遗留：sim.log 已经被 perception_update 刷屏。
			// 但同事 UE 联调时心跳是关键诊断信号——超时关闭后若日志里
			// 看不到任何 heartbeat received，就能直接定位"UE 没发心跳"。
			// 用 Debug 级别避免污染 INFO 日志，开 -log-level debug 即可见。
			s.log.Debug("[UE→MCP] heartbeat received",
				"agent_id", env.AgentID, "seq", env.Seq, "uptime_sec", hb.UptimeSec, "count", count)
			// 首条心跳打 INFO：标志 UE 心跳链路确实建立。后续靠 Debug。
			if count == 1 {
				s.log.Info("[UE→MCP] first heartbeat received", "agent_id", env.AgentID, "seq", env.Seq)
			}
			// 更新心跳时间戳（用于 15s 超时检测）
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
	// by the tactical/strategic layers as [LLM→MCP/...-RESPONSE]).
	switch msgType {
	case protocol.TypeHeartbeat, protocol.TypeResync:
		s.log.Debug("[MCP→UE]", "type", msgType, "seq", seq, "agent_id", agentID)
	default:
		s.log.Info("[MCP→UE]", "type", msgType, "seq", seq, "agent_id", agentID, "payload", string(raw))
	}

	return s.writeFrame(frame)
}

// writeFrame writes a pre-marshaled envelope frame to the current connection.
// writeMu 串行化所有 conn.Write 调用，避免流式叙事推送与动作分发/重放
// 等并发写造成 WebSocket 帧交错损坏。
func (s *Server) writeFrame(frame []byte) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return errors.New("no ue connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWriteWait)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
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
func (s *Server) Call(ctx context.Context, agentID, cmd string, params map[string]any, autoQueue bool) (*protocol.ActionStartedPayload, error) {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("no ue connected")
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
		ActionID:  actionID,
		Cmd:       cmd,
		Params:    params,
		AutoQueue: autoQueue,
	}
	s.log.Info("[MCP→UE/CMD]", "cmd", cmd, "action_id", actionID, "agent_id", agentID, "params", fmt.Sprint(params))
	if err := s.SendEnvelope(agentID, protocol.TypeActionCommand, cmdPayload); err != nil {
		return nil, fmt.Errorf("send action_command: %w", err)
	}

	// ACK must arrive within 10s. Complex actions (pathfinding + state machine
	// transitions) can take several seconds on UE side; 2s was too aggressive
	// and caused frequent ACK timeouts + retry loops.
	ackTimeout := 10 * time.Second
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
func (s *Server) SendAction(ctx context.Context, agentID, cmd string, params map[string]any, autoQueue bool) (*protocol.ActionStartedPayload, error) {
	return s.Call(ctx, agentID, cmd, params, autoQueue)
}

// RequestScan asks Mock UE to emit an immediate perception_update for the
// given agent. scanID is echoed by that response to correlate a one-shot
// scan follow-up decision. Fire-and-forget (no ACK expected).
func (s *Server) RequestScan(ctx context.Context, agentID, scanID string) error {
	return s.SendEnvelope(agentID, protocol.TypeScanArea, protocol.ScanAreaPayload{ScanID: scanID})
}

// SendStopAction 发送 stop_action 控制消息停止指定 action（约定9）。
// UE 侧应比对 action_id 与当前执行的 action_id，不匹配回 error{code:STOP_ID_MISMATCH}。
// fire-and-forget：stop_action 是控制消息，不等 ACK。
// TypeStopAction 已在 discreteReplayTypes 中，会进 sendBuf 重放。
func (s *Server) SendStopAction(agentID, actionID string) error {
	return s.SendEnvelope(agentID, protocol.TypeStopAction, protocol.StopActionPayload{
		ActionID: actionID,
	})
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
			// 发出站心跳（agent_id="system"），uptime_sec 填进程实际运行时长
			// （协议 §2.3 heartbeat payload 示例字段）。
			uptime := int64(time.Since(s.startedAt).Seconds())
			if err := s.SendEnvelope(protocol.SystemAgentID, protocol.TypeHeartbeat, protocol.HeartbeatPayload{
				UptimeSec: uptime,
			}); err != nil {
				s.log.Debug("heartbeat send failed", "err", err)
			}
			// 检测 UE 心跳响应超时（15s）
			s.mu.RLock()
			last := s.lastHeartbeatAt
			s.mu.RUnlock()
			if time.Since(last) > 15*time.Second {
				// 详细诊断：连接建立了多久、共收到多少条心跳、最后心跳何时。
				// 这三个字段能区分"UE 从未发心跳"(count=0)、"发了几条就停"(count>0)
				// 和"心跳间隔配置错误"(since_last 接近 15s)。
				count := atomic.LoadInt64(&s.heartbeatsReceived)
				s.log.Warn("UE heartbeat timeout (>15s), closing connection",
					"last_heartbeat", last,
					"since_last_sec", time.Since(last).Seconds(),
					"heartbeats_received", count,
					"uptime_sec", uptime,
				)
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
