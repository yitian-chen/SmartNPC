package main

import (
	"reflect"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

func TestCapabilityRegistry_GlobalDefault(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "move"},
		{Cmd: protocol.CmdWait, Kind: "atomic", Description: "wait"},
	})
	if !r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = false; want true (global default)")
	}
	got := r.EffectiveActions("H-01")
	if len(got) != 2 {
		t.Fatalf("EffectiveActions(H-01) len = %d; want 2", len(got))
	}
	// Sorted by Cmd.
	if got[0].Cmd != protocol.CmdMoveTo {
		t.Errorf("EffectiveActions[0].Cmd = %q; want %q", got[0].Cmd, protocol.CmdMoveTo)
	}
}

func TestCapabilityRegistry_PerAgentOverrideWins(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "global-move"},
		{Cmd: protocol.CmdWait, Kind: "atomic", Description: "global-wait"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "h01-move"},
	})
	got := r.EffectiveActions("H-01")
	// Per-agent override REPLACES global (not augments): H-01 only has
	// what its override declares.
	if len(got) != 1 {
		t.Fatalf("EffectiveActions(H-01) len = %d; want 1 (override replaces global)", len(got))
	}
	if got[0].Cmd != protocol.CmdMoveTo || got[0].Description != "h01-move" {
		t.Errorf("EffectiveActions[0] = %+v; want {Cmd:MoveTo, Description:h01-move}", got[0])
	}
}

func TestCapabilityRegistry_PerAgentRejectsCmdAbsentEverywhere(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	if r.HasCmd("H-01", protocol.CmdStop) {
		t.Errorf("HasCmd(H-01, Stop) = true; want false (not in global or override)")
	}
}

func TestCapabilityRegistry_PerAgentOverrideReplacesGlobal(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdStop, Kind: "atomic"},
	})
	// Override REPLACES global: H-01 only has CmdStop, not CmdMoveTo.
	if r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = true; want false (override replaces global)")
	}
	if !r.HasCmd("H-01", protocol.CmdStop) {
		t.Errorf("HasCmd(H-01, Stop) = false; want true (per-agent override)")
	}
	got := r.EffectiveActions("H-01")
	if len(got) != 1 {
		t.Fatalf("EffectiveActions(H-01) len = %d; want 1 (override only)", len(got))
	}
}

func TestCapabilityRegistry_RegisterReplacesWholesale(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	// Second global Register replaces, not merges.
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdStop, Kind: "atomic"},
	})
	if r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = true; want false (global replaced wholesale)")
	}
	if r.HasCmd("H-01", protocol.CmdStop) {
		// Expected.
	} else {
		t.Errorf("HasCmd(H-01, Stop) = false; want true")
	}
}

func TestCapabilityRegistry_ClearAgent(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdStop, Kind: "atomic"},
	})
	r.Clear("H-01")
	if r.HasCmd("H-01", protocol.CmdStop) {
		t.Errorf("HasCmd(H-01, Stop) = true after Clear; want false")
	}
	// Global still applies.
	if !r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = false after clearing per-agent; want true (global)")
	}
}

func TestCapabilityRegistry_EffectiveActionsSortedByCmd(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdWait, Kind: "atomic"},
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdExecuteComposite, Kind: "composite"},
	})
	got := r.EffectiveActions("H-01")
	want := []string{protocol.CmdExecuteComposite, protocol.CmdMoveTo, protocol.CmdWait}
	gotCmds := make([]string, len(got))
	for i, a := range got {
		gotCmds[i] = a.Cmd
	}
	if !reflect.DeepEqual(gotCmds, want) {
		t.Errorf("EffectiveActions order = %v; want %v", gotCmds, want)
	}
}

// TestCapabilityRegistry_Snapshot_GlobalOnly verifies Snapshot returns
// the global default under the "system" key with actions sorted by Cmd,
// and no per-agent entries when only the global has been registered.
func TestCapabilityRegistry_Snapshot_GlobalOnly(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdWait, Kind: "atomic"},
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	snap := r.Snapshot()
	if len(snap.Agents) != 1 {
		t.Fatalf("Snapshot.Agents len = %d; want 1 (system only)", len(snap.Agents))
	}
	sys, ok := snap.Agents[protocol.SystemAgentID]
	if !ok {
		t.Fatal("Snapshot.Agents missing \"system\" key")
	}
	if len(sys) != 2 {
		t.Fatalf("system actions len = %d; want 2", len(sys))
	}
	// Sorted by Cmd.
	want := []string{protocol.CmdMoveTo, protocol.CmdWait}
	gotCmds := make([]string, len(sys))
	for i, a := range sys {
		gotCmds[i] = a.Cmd
	}
	if !reflect.DeepEqual(gotCmds, want) {
		t.Errorf("system actions order = %v; want %v", gotCmds, want)
	}
}

// TestCapabilityRegistry_Snapshot_WithPerAgent verifies Snapshot exposes
// both the global "system" key and each per-agent override as independent
// entries, each sorted by Cmd.
func TestCapabilityRegistry_Snapshot_WithPerAgent(t *testing.T) {
	r := NewCapabilityRegistry()
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdStop, Kind: "atomic"},
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	snap := r.Snapshot()
	if len(snap.Agents) != 2 {
		t.Fatalf("Snapshot.Agents len = %d; want 2 (system + H-01)", len(snap.Agents))
	}
	sys := snap.Agents[protocol.SystemAgentID]
	if len(sys) != 2 {
		t.Errorf("system actions len = %d; want 2", len(sys))
	}
	h01 := snap.Agents["H-01"]
	if len(h01) != 2 {
		t.Errorf("H-01 actions len = %d; want 2", len(h01))
	}
	// H-01 sorted by Cmd: Speak < Stop
	wantH01 := []string{protocol.CmdSpeak, protocol.CmdStop}
	gotCmds := make([]string, len(h01))
	for i, a := range h01 {
		gotCmds[i] = a.Cmd
	}
	if !reflect.DeepEqual(gotCmds, wantH01) {
		t.Errorf("H-01 actions order = %v; want %v", gotCmds, wantH01)
	}
}
