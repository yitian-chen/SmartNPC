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

// EffectiveActions returns the effective action set for agentID.
// If a per-agent override exists for agentID, it REPLACES the global
// default (override is authoritative, not augmentative); otherwise
// the global default applies. The returned slice is sorted by Cmd
// for deterministic prompt generation.
func (r *CapabilityRegistry) EffectiveActions(agentID string) []protocol.CapabilityAction {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if override, ok := r.perAgent[agentID]; ok {
		out := make([]protocol.CapabilityAction, 0, len(override))
		for _, a := range override {
			out = append(out, a)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Cmd < out[j].Cmd })
		return out
	}
	out := make([]protocol.CapabilityAction, 0, len(r.global))
	for _, a := range r.global {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cmd < out[j].Cmd })
	return out
}

// HasCmd reports whether agentID can execute cmd. If a per-agent
// override exists for agentID, it is authoritative (replaces global);
// otherwise the global default applies.
func (r *CapabilityRegistry) HasCmd(agentID, cmd string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if override, ok := r.perAgent[agentID]; ok {
		_, has := override[cmd]
		return has
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

// BuiltinCmdCapabilities is the default capability set seeded at startup
// so the system works even if UE never sends a capability_registry
// message. It lists all 9 cmds the protocol defines with the same
// descriptions the tactical layer previously hardcoded.
//
// UE that implements every cmd (e.g. mock_ue) is expected to send a
// capability_registry message on connect that mirrors this list; doing
// so overwrites the seed and becomes the authoritative declaration.
var BuiltinCmdCapabilities = []protocol.CapabilityAction{
	{
		Cmd:                  protocol.CmdMoveTo,
		Kind:                 "atomic",
		Description:          "移动到指定目标位置或语义目标",
		UsageHint:            "target 可填 zones/objects 中的 ID 或语义名称",
		EstimatedDurationSec: 30,
		Params: []protocol.CapabilityParam{
			{Name: "target", Type: "string", Description: "目标位置或语义目标 ID", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdTurnTo,
		Kind:                 "atomic",
		Description:          "转身面向指定目标",
		EstimatedDurationSec: 5,
		Params: []protocol.CapabilityParam{
			{Name: "target", Type: "string", Description: "目标朝向 ID", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdPlayAnimation,
		Kind:                 "atomic",
		Description:          "播放一段动画",
		EstimatedDurationSec: 10,
		Params: []protocol.CapabilityParam{
			{Name: "animation", Type: "string", Description: "动画名称", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdSpeak,
		Kind:                 "atomic",
		Description:          "对目标说话",
		EstimatedDurationSec: 10,
		Params: []protocol.CapabilityParam{
			{Name: "content", Type: "string", Description: "说话内容", Required: true},
			{Name: "target", Type: "string", Description: "对话目标 ID"},
		},
	},
	{
		Cmd:                  protocol.CmdEmote,
		Kind:                 "atomic",
		Description:          "表现情绪表情",
		EstimatedDurationSec: 5,
		Params: []protocol.CapabilityParam{
			{Name: "emotion", Type: "string", Description: "情绪类型", Required: true},
			{Name: "mode", Type: "string", Description: "表现模式"},
		},
	},
	{
		Cmd:                  protocol.CmdWait,
		Kind:                 "atomic",
		Description:          "原地等待一段时间",
		EstimatedDurationSec: 60,
		Params: []protocol.CapabilityParam{
			{Name: "duration_sec", Type: "integer", Description: "等待秒数", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdInteractSmartObject,
		Kind:                 "atomic",
		Description:          "与智能对象交互",
		EstimatedDurationSec: 15,
		Params: []protocol.CapabilityParam{
			{Name: "object_id", Type: "string", Description: "智能对象 ID", Required: true},
			{Name: "action", Type: "string", Description: "交互动作", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdExecuteComposite,
		Kind:                 "composite",
		Description:          "执行复合行为（封装一段时长内的多步骤活动）",
		UsageHint:            "duration_min 内部 ×60 转 duration_sec",
		EstimatedDurationSec: 600,
		Params: []protocol.CapabilityParam{
			{Name: "action", Type: "string", Description: "复合行为类型", Required: true},
			{Name: "target", Type: "string", Description: "目标 ID"},
			{Name: "duration_min", Type: "integer", Description: "持续分钟数"},
		},
	},
	{
		Cmd:                  protocol.CmdStop,
		Kind:                 "atomic",
		Description:          "停止当前在途动作",
		EstimatedDurationSec: 1,
	},
}
