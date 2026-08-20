package main

import (
	"reflect"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

func TestCapabilityRegistry_GlobalDefault(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic", Description: "move"},
		{Cmd: protocol.CmdWait, Kind: "atomic", Description: "wait"},
	})
	if !r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = false; want true (global default)")
	}
	// SocialChat is auto-injected as MCP-side fallback when UE doesn't declare it.
	if !r.HasCmd("H-01", protocol.CmdSocialChat) {
		t.Errorf("HasCmd(H-01, SocialChat) = false; want true (MCP-side fallback)")
	}
	got := r.EffectiveActions("H-01")
	if len(got) != 3 {
		t.Fatalf("EffectiveActions(H-01) len = %d; want 3 (2 declared + SocialChat fallback)", len(got))
	}
	// Sorted by Cmd: MoveTo < SocialChat < Wait.
	if got[0].Cmd != protocol.CmdMoveTo {
		t.Errorf("EffectiveActions[0].Cmd = %q; want %q", got[0].Cmd, protocol.CmdMoveTo)
	}
	if got[1].Cmd != protocol.CmdSocialChat {
		t.Errorf("EffectiveActions[1].Cmd = %q; want %q", got[1].Cmd, protocol.CmdSocialChat)
	}
	if got[2].Cmd != protocol.CmdWait {
		t.Errorf("EffectiveActions[2].Cmd = %q; want %q", got[2].Cmd, protocol.CmdWait)
	}
}

func TestCapabilityRegistry_PerAgentOverrideWins(t *testing.T) {
	r := NewCapabilityRegistry(nil)
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
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	// CmdGenericAct is not declared anywhere — HasCmd must return false.
	if r.HasCmd("H-01", protocol.CmdGenericAct) {
		t.Errorf("HasCmd(H-01, GenericAct) = true; want false (not in global or override)")
	}
}

func TestCapabilityRegistry_PerAgentOverrideReplacesGlobal(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	// Override REPLACES global: H-01 only has CmdSpeak, not CmdMoveTo.
	if r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = true; want false (override replaces global)")
	}
	if !r.HasCmd("H-01", protocol.CmdSpeak) {
		t.Errorf("HasCmd(H-01, Speak) = false; want true (per-agent override)")
	}
	got := r.EffectiveActions("H-01")
	if len(got) != 1 {
		t.Fatalf("EffectiveActions(H-01) len = %d; want 1 (override only)", len(got))
	}
}

func TestCapabilityRegistry_RegisterReplacesWholesale(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	// Second global Register replaces, not merges.
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	if r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = true; want false (global replaced wholesale)")
	}
	if r.HasCmd("H-01", protocol.CmdSpeak) {
		// Expected.
	} else {
		t.Errorf("HasCmd(H-01, Speak) = false; want true")
	}
}

func TestCapabilityRegistry_ClearAgent(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	r.Clear("H-01")
	if r.HasCmd("H-01", protocol.CmdSpeak) {
		t.Errorf("HasCmd(H-01, Speak) = true after Clear; want false")
	}
	// Global still applies.
	if !r.HasCmd("H-01", protocol.CmdMoveTo) {
		t.Errorf("HasCmd(H-01, MoveTo) = false after clearing per-agent; want true (global)")
	}
}

func TestCapabilityRegistry_EffectiveActionsSortedByCmd(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdWait, Kind: "atomic"},
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWorkShift, Kind: "composite"},
	})
	got := r.EffectiveActions("H-01")
	// Sorted by Cmd alphabetically: MoveTo < SocialChat < Wait < WorkShift
	// (SocialChat auto-injected as MCP-side fallback).
	want := []string{protocol.CmdMoveTo, protocol.CmdSocialChat, protocol.CmdWait, protocol.CmdWorkShift}
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
	r := NewCapabilityRegistry(nil)
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
	if len(sys) != 3 {
		t.Fatalf("system actions len = %d; want 3 (2 declared + SocialChat fallback)", len(sys))
	}
	// Sorted by Cmd: MoveTo < SocialChat < Wait
	want := []string{protocol.CmdMoveTo, protocol.CmdSocialChat, protocol.CmdWait}
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
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: protocol.CmdMoveTo, Kind: "atomic"},
		{Cmd: protocol.CmdWait, Kind: "atomic"},
	})
	r.Register("H-01", []protocol.CapabilityAction{
		{Cmd: protocol.CmdTurnTo, Kind: "atomic"},
		{Cmd: protocol.CmdSpeak, Kind: "atomic"},
	})
	snap := r.Snapshot()
	if len(snap.Agents) != 2 {
		t.Fatalf("Snapshot.Agents len = %d; want 2 (system + H-01)", len(snap.Agents))
	}
	sys := snap.Agents[protocol.SystemAgentID]
	if len(sys) != 3 {
		t.Errorf("system actions len = %d; want 3 (2 declared + SocialChat fallback)", len(sys))
	}
	h01 := snap.Agents["H-01"]
	if len(h01) != 2 {
		t.Errorf("H-01 actions len = %d; want 2", len(h01))
	}
	// H-01 sorted by Cmd: Speak < TurnTo
	wantH01 := []string{protocol.CmdSpeak, protocol.CmdTurnTo}
	gotCmds := make([]string, len(h01))
	for i, a := range h01 {
		gotCmds[i] = a.Cmd
	}
	if !reflect.DeepEqual(gotCmds, wantH01) {
		t.Errorf("H-01 actions order = %v; want %v", gotCmds, wantH01)
	}
}

// TestCapabilityRegistry_NormalizesWhitespace 验证 UE 推送的 capability_registry
// 中 cmd/kind/param.name 带前导或尾随空格时，Register 会做 TrimSpace 兜底，
// 避免污染下游 map key、工具名（AddTool 拒绝含空格名）和 HasCmd 校验。
// 复现场景：stable 端日志 2026-08-05 显示 UE 发来 "InteractSmartObject " 带
// 尾随空格，导致 AddTool 报 "invalid tool name"。
func TestCapabilityRegistry_NormalizesWhitespace(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{
			Cmd:       " InteractSmartObject ",
			Kind:      " atomic ",
			Params:    []protocol.CapabilityParam{{Name: " smart_object ", Type: "string"}},
			UsageHint: " 保留描述空格不动 ",
		},
		{Cmd: " MoveTo ", Kind: "atomic"},
	})

	// Cmd 应被 trim，HasCmd 用规范化后的 key 校验通过。
	if !r.HasCmd("H-01", "InteractSmartObject") {
		t.Errorf("HasCmd(InteractSmartObject) = false; want true (cmd should be trimmed)")
	}
	if !r.HasCmd("H-01", "MoveTo") {
		t.Errorf("HasCmd(MoveTo) = false; want true (cmd should be trimmed)")
	}
	// 带空格的原值不应能查到（map key 是规范化后的）。
	if r.HasCmd("H-01", " InteractSmartObject ") {
		t.Errorf("HasCmd(' InteractSmartObject ') = true; want false (untrimmed key should not exist)")
	}

	acts := r.EffectiveActions("H-01")
	if len(acts) != 3 {
		t.Fatalf("EffectiveActions len = %d; want 3 (2 declared + SocialChat fallback)", len(acts))
	}
	// 找到 InteractSmartObject 那条，验证 Kind 和 Param.Name 都被 trim。
	var got protocol.CapabilityAction
	for _, a := range acts {
		if a.Cmd == "InteractSmartObject" {
			got = a
			break
		}
	}
	if got.Cmd != "InteractSmartObject" {
		t.Fatalf("InteractSmartObject action not found after normalization")
	}
	if got.Kind != "atomic" {
		t.Errorf("Kind = %q; want %q (should be trimmed)", got.Kind, "atomic")
	}
	if len(got.Params) != 1 || got.Params[0].Name != "smart_object" {
		t.Errorf("Param.Name = %q; want %q (should be trimmed)", got.Params[0].Name, "smart_object")
	}
	// 描述性字段不应被 trim（保留 UE 原文）。
	if got.UsageHint != " 保留描述空格不动 " {
		t.Errorf("UsageHint = %q; want %q (descriptive fields should NOT be trimmed)",
			got.UsageHint, " 保留描述空格不动 ")
	}
}

// TestCapabilityRegistry_NormalizesAgentID 验证 agentID 也被 trim，
// 避免 " system " 这样的输入被当作 per-agent override 而非 global default。
func TestCapabilityRegistry_NormalizesAgentID(t *testing.T) {
	r := NewCapabilityRegistry(nil)
	// " system " 带空格应被规范化为 "system"，写入 global default。
	r.Register(" system ", []protocol.CapabilityAction{{Cmd: "MoveTo", Kind: "atomic"}})
	if !r.HasCmd("H-01", "MoveTo") {
		t.Errorf("HasCmd(H-01, MoveTo) = false; want true (agentID ' system ' should normalize to global default)")
	}
}

// TestIsCompositeCmdDynamic 验证动态复合 cmd 判断：
//   - 硬编码的 5 个内置复合 cmd 始终识别（向后兼容）
//   - registry 兜底识别 UE5 新推送的复合 cmd
//   - registry==nil 退化为仅查硬编码列表
//   - atomic cmd 不被误判为复合
func TestIsCompositeCmdDynamic(t *testing.T) {
	// 内置硬编码复合 cmd（无需 registry）
	if !isCompositeCmdDynamic(protocol.CmdWorkShift, nil) {
		t.Errorf("isCompositeCmdDynamic(WorkShift, nil) = false; want true (builtin composite)")
	}
	if !isCompositeCmdDynamic(protocol.CmdChargeAtStation, nil) {
		t.Errorf("isCompositeCmdDynamic(ChargeAtStation, nil) = false; want true (builtin composite)")
	}
	// 原子 cmd 不应是复合
	if isCompositeCmdDynamic(protocol.CmdMoveTo, nil) {
		t.Errorf("isCompositeCmdDynamic(MoveTo, nil) = true; want false (atomic)")
	}

	// registry 兜底：UE5 新推送的复合 cmd（不在硬编码列表里）
	r := NewCapabilityRegistry(nil)
	r.Register(protocol.SystemAgentID, []protocol.CapabilityAction{
		{Cmd: "CustomWork", Kind: "composite"},
		{Cmd: "CustomMaintain", Kind: "composite"},
		{Cmd: "MoveTo", Kind: "atomic"},
	})
	if !isCompositeCmdDynamic("CustomWork", r) {
		t.Errorf("isCompositeCmdDynamic(CustomWork, registry) = false; want true (registry kind=composite)")
	}
	if !isCompositeCmdDynamic("CustomMaintain", r) {
		t.Errorf("isCompositeCmdDynamic(CustomMaintain, registry) = false; want true (registry kind=composite)")
	}
	// registry 里标记为 atomic 的 cmd 不应是复合
	if isCompositeCmdDynamic("MoveTo", r) {
		t.Errorf("isCompositeCmdDynamic(MoveTo, registry) = true; want false (registry kind=atomic)")
	}
	// 不在 registry 也不在硬编码列表的 cmd
	if isCompositeCmdDynamic("UnknownCmd", r) {
		t.Errorf("isCompositeCmdDynamic(UnknownCmd, registry) = true; want false")
	}
}
