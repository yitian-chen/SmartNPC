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
