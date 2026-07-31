package main

import (
	"sort"
	"sync"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
)

// CapabilityRegistry stores NPC capability declarations received via
// capability_registry messages. agent_id="system" writes the global
// default; a specific agent_id overrides the default for that agent
// only.
//
// Effective actions for an agent = global default overlaid with the
// agent's per-agent entries (per-agent wins on a per-cmd basis).
//
// All methods are goroutine-safe.
type CapabilityRegistry struct {
	mu       sync.RWMutex
	global   map[string]protocol.CapabilityAction            // cmd -> action
	perAgent map[string]map[string]protocol.CapabilityAction // agent_id -> (cmd -> action)
}

// NewCapabilityRegistry returns an empty registry.
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		global:   make(map[string]protocol.CapabilityAction),
		perAgent: make(map[string]map[string]protocol.CapabilityAction),
	}
}

// Register stores actions under agentID. agent_id == "system" (or empty)
// writes to the global default; any other agentID writes to that agent's
// override map. Existing entries for the same agent_id are replaced
// wholesale (the new action list is the authoritative declaration).
func (r *CapabilityRegistry) Register(agentID string, actions []protocol.CapabilityAction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if agentID == "" || agentID == protocol.SystemAgentID {
		// Replace global wholesale.
		r.global = make(map[string]protocol.CapabilityAction, len(actions))
		for _, a := range actions {
			r.global[a.Cmd] = a
		}
		return
	}
	// Per-agent override: replace this agent's map wholesale.
	m := make(map[string]protocol.CapabilityAction, len(actions))
	for _, a := range actions {
		m[a.Cmd] = a
	}
	r.perAgent[agentID] = m
}

// EffectiveActions returns the effective action set for agentID:
// global defaults overlaid with the agent's per-agent overrides, with
// per-agent winning on a per-cmd basis. The returned slice is sorted
// by Cmd for deterministic prompt generation.
func (r *CapabilityRegistry) EffectiveActions(agentID string) []protocol.CapabilityAction {
	r.mu.RLock()
	defer r.mu.RUnlock()
	merged := make(map[string]protocol.CapabilityAction, len(r.global))
	for cmd, a := range r.global {
		merged[cmd] = a
	}
	if override, ok := r.perAgent[agentID]; ok {
		for cmd, a := range override {
			merged[cmd] = a
		}
	}
	out := make([]protocol.CapabilityAction, 0, len(merged))
	for _, a := range merged {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cmd < out[j].Cmd })
	return out
}

// HasCmd reports whether agentID can execute cmd. Per-agent override
// wins; absent an override, the global default applies.
func (r *CapabilityRegistry) HasCmd(agentID, cmd string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if override, ok := r.perAgent[agentID]; ok {
		if _, has := override[cmd]; has {
			return true
		}
		// Fall through to global if the override doesn't list cmd —
		// an override is not exhaustive, it augments.
	}
	_, has := r.global[cmd]
	return has
}

// Clear removes an agent's per-agent override (or the global default
// when agentID == "system"). Used on agent_unregistered to drop stale
// per-agent state.
func (r *CapabilityRegistry) Clear(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if agentID == "" || agentID == protocol.SystemAgentID {
		r.global = make(map[string]protocol.CapabilityAction)
		return
	}
	delete(r.perAgent, agentID)
}
