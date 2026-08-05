package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// TestHandleDebugUI_ReturnsHTML verifies /debug/ returns the embedded HTML page.
func TestHandleDebugUI_ReturnsHTML(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/debug/", nil)
	rec := httptest.NewRecorder()
	handleDebugUI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type: got %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "AgentTown Debug Console") {
		t.Error("body should contain page title")
	}
	if !strings.Contains(body, "/debug/action") {
		t.Error("body should reference /debug/action endpoint")
	}
}

// TestHandleDebugUI_RejectsNonGet verifies non-GET methods are rejected.
func TestHandleDebugUI_RejectsNonGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/debug/", nil)
	rec := httptest.NewRecorder()
	handleDebugUI(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

// TestHandleDebugKB_ReturnsZonesAndObjects verifies /debug/kb returns
// zones/objects from the loaded KB.
func TestHandleDebugKB_ReturnsZonesAndObjects(t *testing.T) {
	kb := loadTestKB(t)
	logger := slog.Default()

	req := httptest.NewRequest(http.MethodGet, "/debug/kb", nil)
	rec := httptest.NewRecorder()
	handleDebugKB(rec, req, kb, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var resp debugKBResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Zones) == 0 {
		t.Error("Zones should not be empty")
	}
	if len(resp.Objects) == 0 {
		t.Error("Objects should not be empty")
	}
	// 验证具体内容
	foundWorkshop := false
	for _, z := range resp.Zones {
		if z.ID == "main_workshop" {
			foundWorkshop = true
			break
		}
	}
	if !foundWorkshop {
		t.Error("Zones should contain main_workshop")
	}
	foundWorkbench := false
	for _, o := range resp.Objects {
		if o.ID == "workbench_01" {
			foundWorkbench = true
			if o.ZoneID != "main_workshop" {
				t.Errorf("workbench_01 zone_id: got %q, want main_workshop", o.ZoneID)
			}
			foundAssemble := false
			for _, a := range o.AvailableInteractions {
				if a == "assemble" {
					foundAssemble = true
					break
				}
			}
			if !foundAssemble {
				t.Errorf("workbench_01 available_interactions should contain assemble, got %v", o.AvailableInteractions)
			}
			break
		}
	}
	if !foundWorkbench {
		t.Error("Objects should contain workbench_01")
	}
}

// TestHandleDebugKB_NilKBReturnsEmpty verifies nil KB doesn't panic.
func TestHandleDebugKB_NilKBReturnsEmpty(t *testing.T) {
	logger := slog.Default()
	req := httptest.NewRequest(http.MethodGet, "/debug/kb", nil)
	rec := httptest.NewRecorder()
	handleDebugKB(rec, req, nil, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var resp debugKBResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Zones) != 0 || len(resp.Objects) != 0 {
		t.Errorf("nil KB should return empty arrays, got zones=%d objs=%d",
			len(resp.Zones), len(resp.Objects))
	}
}

// TestHandleDebugCap_ReturnsAgents verifies /debug/cap returns the
// registry snapshot keyed by agentID, with the global default under
// "system". Used by the e2e test to black-box-verify that mock_ue's
// capability_registry message was registered on the MCP side.
func TestHandleDebugCap_ReturnsAgents(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic", Description: "move"},
		{Cmd: protocol.CmdSpeak, Kind: "atomic", Description: "speak"},
	})
	logger := slog.Default()

	req := httptest.NewRequest(http.MethodGet, "/debug/cap", nil)
	rec := httptest.NewRecorder()
	handleDebugCap(rec, req, reg, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	var snap CapabilitySnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sys, ok := snap.Agents[protocol.SystemAgentID]
	if !ok {
		t.Fatal("Agents missing \"system\" key")
	}
	// handleDebugCap 始终追加合成 Stop 项，所以 len = 注册数 + 1
	if len(sys) != 3 {
		t.Fatalf("system actions len = %d; want 3 (2 registered + 1 synthetic Stop)", len(sys))
	}
	// 前 2 项按 Cmd 排序：MoveToLocation < Speak；末尾是合成 Stop
	if sys[0].Cmd != protocol.CmdMoveToLocation || sys[1].Cmd != protocol.CmdSpeak {
		t.Errorf("system actions order = %s, %s; want MoveToLocation, Speak", sys[0].Cmd, sys[1].Cmd)
	}
	if sys[2].Cmd != "Stop" {
		t.Errorf("system actions[2].Cmd = %q; want Stop (synthetic)", sys[2].Cmd)
	}
}

// TestHandleDebugCap_NilRegistryReturnsEmpty verifies nil registry
// doesn't panic and returns an empty snapshot.
func TestHandleDebugCap_NilRegistryReturnsEmpty(t *testing.T) {
	logger := slog.Default()
	req := httptest.NewRequest(http.MethodGet, "/debug/cap", nil)
	rec := httptest.NewRecorder()
	handleDebugCap(rec, req, nil, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var snap CapabilitySnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Agents) != 0 {
		t.Errorf("nil registry should return empty agents, got %d", len(snap.Agents))
	}
}

// TestHandleDebugCap_ToolNameField verifies /debug/cap enriches each action
// with a tool_name field derived from tools.CmdToToolName, so the frontend
// dropdown can use tool_name as the option value (matching mapDebugCmd's
// tool_name matching path and frontend cmd-specific logic like
// cmd === 'move_to_location').
func TestHandleDebugCap_ToolNameField(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},           // tool_name: move_to_location
		{Cmd: protocol.CmdInteractSmartObject, Kind: "atomic"},      // tool_name: interact (special shortening)
		{Cmd: "MoveTo", Kind: "atomic"},                             // tool_name: move_to (pascalToSnake fallback)
	})
	logger := slog.Default()
	req := httptest.NewRequest(http.MethodGet, "/debug/cap", nil)
	rec := httptest.NewRecorder()
	handleDebugCap(rec, req, reg, logger)

	var resp debugCapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sys := resp.Agents[protocol.SystemAgentID]
	// 3 registered + 1 synthetic Stop
	if len(sys) != 4 {
		t.Fatalf("system actions len = %d; want 4 (3 registered + 1 synthetic Stop)", len(sys))
	}
	wantTool := map[string]string{
		"MoveToLocation":      "move_to_location",
		"InteractSmartObject": "interact",
		"MoveTo":              "move_to",
		"Stop":                "stop",
	}
	for _, a := range sys {
		got := a.ToolName
		want, ok := wantTool[a.Cmd]
		if !ok {
			t.Errorf("unexpected cmd %q", a.Cmd)
			continue
		}
		if got != want {
			t.Errorf("tool_name for %q = %q, want %q", a.Cmd, got, want)
		}
	}
}

// TestHandleDebugCap_AlwaysIncludesStop verifies /debug/cap always appends
// a synthetic Stop capability to every agent's list, regardless of what's
// registered. This satisfies the colleague's requirement that `stop` is
// always in the cmd list, unaffected by capability_registry registration.
func TestHandleDebugCap_AlwaysIncludesStop(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	// 空注册（仅 global seed 由 Register 写入）；Stop 也应出现
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	reg.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
	})
	logger := slog.Default()
	req := httptest.NewRequest(http.MethodGet, "/debug/cap", nil)
	rec := httptest.NewRecorder()
	handleDebugCap(rec, req, reg, logger)

	var resp debugCapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 每个 agent 列表末尾都应有 Stop
	for agentID, acts := range resp.Agents {
		foundStop := false
		for _, a := range acts {
			if a.Cmd == "Stop" && a.ToolName == "stop" && a.Kind == "atomic" {
				foundStop = true
				break
			}
		}
		if !foundStop {
			t.Errorf("agent %q: synthetic Stop not found in /debug/cap response", agentID)
		}
	}
	if len(resp.Agents) == 0 {
		t.Fatal("no agents in response")
	}
}

// TestMapDebugCmd_AcceptsBothForms verifies mapDebugCmd accepts both
// the raw cmd (PascalCase, e.g. "MoveTo") and the tool_name (snake_case,
// e.g. "move_to"). This is the fix for the HTTP 400 "unknown cmd" bug
// where the frontend dropdown sent the raw cmd but mapDebugCmd only
// matched tool_name.
func TestMapDebugCmd_AcceptsBothForms(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: "MoveTo", Kind: "atomic"},
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
	})
	const agentID = "H-01"

	cases := []struct {
		input string
		want  string
	}{
		{"MoveTo", "MoveTo"},             // raw cmd form
		{"move_to", "MoveTo"},            // tool_name form (pascalToSnake fallback)
		{"MoveToLocation", "MoveToLocation"},         // raw cmd form (builtin)
		{"move_to_location", "MoveToLocation"},       // tool_name form (builtin)
	}
	for _, c := range cases {
		got, ok := mapDebugCmd(c.input, reg, agentID)
		if !ok {
			t.Errorf("mapDebugCmd(%q) = !ok, want %q", c.input, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("mapDebugCmd(%q) = %q, want %q", c.input, got, c.want)
		}
	}

	// Unknown cmd returns false
	if _, ok := mapDebugCmd("Nonexistent", reg, agentID); ok {
		t.Errorf("mapDebugCmd(Nonexistent) should return false")
	}
}

// TestMapDebugCmd_StopNotMatched verifies mapDebugCmd does NOT match "stop"
// or "Stop" — stop is a synthetic cmd handled by a special branch in
// handleDebugAction (which dispatches via ws.SendStopAction, not ws.Call).
// Stop is deliberately absent from EffectiveActions, so mapDebugCmd returns
// false; this test guards against accidental future registration of Stop in
// the registry that would route it through the action_command path.
func TestMapDebugCmd_StopNotMatched(t *testing.T) {
	reg := NewCapabilityRegistry(nil)
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveToLocation, Kind: "atomic"},
	})
	const agentID = "H-01"
	for _, input := range []string{"stop", "Stop"} {
		if _, ok := mapDebugCmd(input, reg, agentID); ok {
			t.Errorf("mapDebugCmd(%q) = ok; want false (stop is handled by special-case dispatch, not mapDebugCmd)", input)
		}
	}
}
