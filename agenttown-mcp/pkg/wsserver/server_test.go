package wsserver

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// freeAddr returns 127.0.0.1:0 to let the OS pick a free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
}

// dialAndServe starts a Server on a free port, connects a client, and
// returns the client connection. The server is stopped on test cleanup.
func dialAndServe(t *testing.T) (*websocket.Conn, *Server) {
	t.Helper()
	addr := freeAddr(t)
	srv := New(Options{Addr: addr, CallTimeout: 2 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Wait briefly for the listener to be ready.
	deadline := time.Now().Add(2 * time.Second)
	var c *websocket.Conn
	var err error
	for time.Now().Before(deadline) {
		c, _, err = websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })

	// Give the server a moment to register the conn.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if srv.IsConnected() {
			return c, srv
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never registered the connection")
	return c, srv
}

// TestServer_CallRoundTrip verifies the server sends a Request and receives
// the matching Response from the client.
func TestServer_CallRoundTrip(t *testing.T) {
	client, srv := dialAndServe(t)

	// Client read loop: respond to Requests with OK + echo.
	go func() {
		for {
			_, data, err := client.Read(context.Background())
			if err != nil {
				return
			}
			var req Request
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			if req.Type != TypeRequest {
				continue
			}
			resp := Response{
				Type: TypeResponse,
				ID:   req.ID,
				OK:   true,
				Data: mustMarshal(map[string]any{
					"action":  req.Action,
					"echo":    "ok",
				}),
			}
			payload, _ := json.Marshal(resp)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = client.Write(ctx, websocket.MessageText, payload)
			cancel()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := srv.Call(ctx, ActionMoveTo, map[string]any{"target": "main_workshop"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		Action string `json:"action"`
		Echo   string `json:"echo"`
	}
	if json.Unmarshal(raw, &got) != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Action != ActionMoveTo || got.Echo != "ok" {
		t.Fatalf("unexpected response: %+v", got)
	}
}

// TestServer_EventDispatch verifies the server dispatches Event frames from
// the client to a registered EventHandler.
func TestServer_EventDispatch(t *testing.T) {
	client, srv := dialAndServe(t)

	got := make(chan struct {
		Name string
		Data json.RawMessage
	}, 1)
	srv.SetEventHandler(func(_ context.Context, name string, data json.RawMessage) {
		got <- struct {
			Name string
			Data json.RawMessage
		}{name, data}
	})

	evt := Event{
		Type:      TypeEvent,
		Name:      EventPerceptionUpdate,
		Data:      mustMarshal(map[string]any{"zone": "main_workshop"}),
		Timestamp: time.Now().UnixMilli(),
	}
	payload, _ := json.Marshal(evt)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case e := <-got:
		if e.Name != EventPerceptionUpdate {
			t.Fatalf("event name = %q, want %q", e.Name, EventPerceptionUpdate)
		}
	case <-time.After(time.Second):
		t.Fatal("event handler not called")
	}
}

// TestServer_CallNoConnection verifies Call fails fast when no client is
// connected.
func TestServer_CallNoConnection(t *testing.T) {
	srv := New(Options{Addr: freeAddr(t), CallTimeout: time.Second})
	// Not calling Serve — no connections possible.
	_, err := srv.Call(context.Background(), ActionMoveTo, nil)
	if err == nil {
		t.Fatal("expected error when no client connected")
	}
}
