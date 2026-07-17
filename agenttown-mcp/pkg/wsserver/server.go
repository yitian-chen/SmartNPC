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

// Server is a WebSocket server accepting a connection from Mock UE.
//
// Phase 1: single connection (one NPC). agent_id routing is threaded through
// so multi-NPC is a natural extension. seq is a per-server monotonic counter
// for outbound messages.
type Server struct {
	addr        string
	log         *slog.Logger
	callTimeout time.Duration

	seq int64 // outbound sequence counter (atomic)

	mu      sync.RWMutex
	conn    *websocket.Conn
	pending map[string]chan *pendingResult // keyed by action_id (msg correlation)

	handlerMu sync.RWMutex
	handler   MessageHandler
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
		pending:     make(map[string]chan *pendingResult),
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

		switch env.Type {
		case protocol.TypeActionStarted:
			// ACK for an action_command — deliver to the waiting Call.
			var ack protocol.ActionStartedPayload
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				s.log.Warn("action_started parse failed", "err", err)
				continue
			}
			s.deliverACK(&ack)
		default:
			// All other inbound messages go to the registered handler:
			// perception_update, action_completed, state_report,
			// agent_registered, agent_unregistered, heartbeat, error.
			s.dispatch(ctx, env.Type, env.AgentID, env.Payload)
		}
	}
}

// deliverACK hands an action_started to the goroutine waiting on its action_id.
func (s *Server) deliverACK(ack *protocol.ActionStartedPayload) {
	s.mu.RLock()
	ch, ok := s.pending[ack.ActionID]
	s.mu.RUnlock()
	if !ok {
		s.log.Warn("action_started with no pending caller", "action_id", ack.ActionID)
		return
	}
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
func (s *Server) SendEnvelope(agentID, msgType string, payload any) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return errors.New("no mock ue connected")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	env := protocol.Envelope{
		Version:   protocol.Version,
		MsgID:     uuid.NewString(),
		Seq:       s.nextSeq(),
		Timestamp: time.Now().UnixMilli(),
		Type:      msgType,
		AgentID:   agentID,
		Payload:   raw,
	}
	frame, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWriteWait)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, frame)
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
	s.pending[actionID] = ch
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
// given agent. Backs the scan_area tool. Fire-and-forget (no ACK expected).
func (s *Server) RequestScan(ctx context.Context, agentID string) error {
	return s.SendEnvelope(agentID, protocol.TypeScanArea, map[string]any{})
}
