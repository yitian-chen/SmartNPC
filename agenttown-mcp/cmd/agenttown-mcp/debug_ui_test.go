package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// TestHandleDebugKB_ReturnsZonesAndLocations verifies /debug/kb returns
// zones/locations/objects from the loaded KB.
func TestHandleDebugKB_ReturnsZonesAndLocations(t *testing.T) {
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
	if len(resp.Locations) == 0 {
		t.Error("Locations should not be empty")
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
	for _, l := range resp.Locations {
		if l.ID == "workbench_01" {
			foundWorkbench = true
			if l.Zone != "main_workshop" {
				t.Errorf("workbench_01 zone: got %q, want main_workshop", l.Zone)
			}
			break
		}
	}
	if !foundWorkbench {
		t.Error("Locations should contain workbench_01")
	}
	// 验证 objects 含 available_actions
	for _, o := range resp.Objects {
		if o.ID == "workbench_01" {
			foundAssemble := false
			for _, a := range o.AvailableActions {
				if a == "assemble" {
					foundAssemble = true
					break
				}
			}
			if !foundAssemble {
				t.Errorf("workbench_01 available_actions should contain assemble, got %v", o.AvailableActions)
			}
		}
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
	if len(resp.Zones) != 0 || len(resp.Locations) != 0 || len(resp.Objects) != 0 {
		t.Errorf("nil KB should return empty arrays, got zones=%d locs=%d objs=%d",
			len(resp.Zones), len(resp.Locations), len(resp.Objects))
	}
}
