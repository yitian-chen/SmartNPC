// Package wsserver hosts the WebSocket server that Mock UE connects to.
//
// Mock UE is the client; agenttown-mcp is the server. This is the inverse of
// the SmartNPC layout (where mcp was the WS client to a SMAPI mod server).
package wsserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Default timeouts.
const (
	defaultCallTimeout = 30 * time.Second
	defaultWriteWait   = 5 * time.Second
)

// EventHandler receives Mock-UE-pushed events. Called from the read loop, so
// long work should be offloaded to a goroutine.
type EventHandler func(ctx context.Context, name string, data json.RawMessage)

// Options configures New.
type Options struct {
	Addr        string
	Logger      *slog.Logger
	CallTimeout time.Duration
}

// Server is a WebSocket server accepting connections from Mock UE.
//
// Phase 1: tracks a single connection (one NPC). Multi-NPC is a Phase 2
// extension — the conn/pending map already keys by connection, so the
// shape extends naturally.
type Server struct {
	addr        string
	log         *slog.Logger
	callTimeout time.Duration

	mu      sync.RWMutex
	conn    *websocket.Conn
	pending map[string]chan *Response

	eventMu sync.RWMutex
	handler EventHandler
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
		pending:     make(map[string]chan *Response),
	}
}

// SetEventHandler registers the callback invoked when an Event frame arrives
// from Mock UE. Safe to call before or during Serve.
func (s *Server) SetEventHandler(h EventHandler) {
	s.eventMu.Lock()
	s.handler = h
	s.eventMu.Unlock()
}

// Serve starts the HTTP server with the WebSocket endpoint at /ws and a
// health endpoint at /healthz. Blocks until ctx is canceled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		connected := s.IsConnected()
		_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"ws_connected":%v}`, connected)))
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
		// Allow cross-origin (Mock UE on host, MCP on host — same origin in
		// Phase 1, but be permissive for dev).
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.log.Warn("ws accept failed", "err", err)
		return
	}
	// Disable the server-side read limit so large perception snapshots
	// don't trip it. Mock UE payloads are < 10KB; default is 32KB which
	// is fine, but we bump it defensively.
	c.SetReadLimit(1 << 20) // 1 MiB

	s.mu.Lock()
	// Phase 1: single connection. If a new one arrives while the old is
	// still open, close the old one.
	if s.conn != nil {
		s.log.Info("replacing existing ws connection")
		_ = s.conn.Close(websocket.StatusNormalClosure, "replaced")
	}
	s.conn = c
	s.mu.Unlock()

	s.log.Info("mock ue connected", "remote", r.RemoteAddr)

	// Detach the request context — the read loop should outlive the HTTP
	// request that upgraded it.
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

// readLoop reads frames and dispatches Response → pending callers, Event → handler.
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

		// Detect frame type via the "type" field without fully unmarshaling.
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &probe) != nil {
			s.log.Warn("ws frame parse failed", "raw", string(data))
			continue
		}

		switch probe.Type {
		case TypeResponse:
			var resp Response
			if err := json.Unmarshal(data, &resp); err != nil {
				s.log.Warn("ws response parse failed", "err", err)
				continue
			}
			s.deliverResponse(&resp)
		case TypeEvent:
			var evt Event
			if err := json.Unmarshal(data, &evt); err != nil {
				s.log.Warn("ws event parse failed", "err", err)
				continue
			}
			s.dispatchEvent(ctx, &evt)
		case TypeRequest:
			s.log.Warn("ignoring inbound Request frame (MCP is the request originator)", "raw", string(data))
		default:
			s.log.Warn("unknown ws frame type", "type", probe.Type)
		}
	}
}

// deliverResponse hands a Response to the goroutine waiting on its ID.
func (s *Server) deliverResponse(resp *Response) {
	s.mu.RLock()
	ch, ok := s.pending[resp.ID]
	s.mu.RUnlock()
	if !ok {
		s.log.Warn("ws response with no pending caller", "id", resp.ID)
		return
	}
	select {
	case ch <- resp:
	default:
		s.log.Warn("ws response channel full, dropping", "id", resp.ID)
	}
}

// dispatchEvent invokes the registered handler for an Event.
func (s *Server) dispatchEvent(ctx context.Context, evt *Event) {
	s.eventMu.RLock()
	h := s.handler
	s.eventMu.RUnlock()
	if h == nil {
		s.log.Debug("ws event dropped (no handler)", "name", evt.Name)
		return
	}
	h(ctx, evt.Name, evt.Data)
}

// Call sends a Request to Mock UE and awaits the matching Response.
//
// Returns an error if no Mock UE is connected, the call times out, or Mock UE
// returns an error response. Thread-safe; multiple goroutines may Call
// concurrently.
func (s *Server) Call(ctx context.Context, action string, params any) (json.RawMessage, error) {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return nil, errors.New("no mock ue connected")
	}

	id := uuid.NewString()
	ch := make(chan *Response, 1)

	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	req := Request{
		Type:   TypeRequest,
		ID:     id,
		Action: action,
		Params: params,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, defaultWriteWait)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, payload); err != nil {
		return nil, fmt.Errorf("ws write: %w", err)
	}

	timeout := s.callTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d < timeout {
			timeout = d
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error != nil {
				return nil, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
			}
			return nil, errors.New("mock ue returned ok=false with no error detail")
		}
		return resp.Data, nil
	case <-timer.C:
		return nil, fmt.Errorf("ws call %s timeout after %s", action, timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Broadcast sends an Event frame to the connected Mock UE. Fire-and-forget.
func (s *Server) Broadcast(name string, data any) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()
	if conn == nil {
		return errors.New("no mock ue connected")
	}

	payload, err := json.Marshal(Event{
		Type:      TypeEvent,
		Name:      name,
		Data:      mustMarshal(data),
		Timestamp: time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultWriteWait)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, payload)
}

// IsConnected reports whether a Mock UE is currently connected.
func (s *Server) IsConnected() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn != nil
}

// mustMarshal is a helper for Broadcast; panics on marshal failure (only
// happens with non-serializable input, which is a programming bug).
func mustMarshal(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
