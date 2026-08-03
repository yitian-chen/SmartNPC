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
	reg := NewCapabilityRegistry()
	reg.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "move"},
		{Cmd: protocol.CmdStop, Kind: "atomic", Description: "stop"},
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
	if len(sys) != 2 {
		t.Fatalf("system actions len = %d; want 2", len(sys))
	}
	// Sorted by Cmd: MoveTo < Stop
	if sys[0].Cmd != protocol.CmdMoveTo || sys[1].Cmd != protocol.CmdStop {
		t.Errorf("system actions order = %s, %s; want MoveTo, Stop", sys[0].Cmd, sys[1].Cmd)
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
