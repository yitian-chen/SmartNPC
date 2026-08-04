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

// CapabilitySnapshot is the JSON-friendly view of the registry state,
// returned by /debug/cap for black-box e2e verification. The global
// default is keyed under protocol.SystemAgentID ("system"); every
// per-agent override is keyed under its agentID.
type CapabilitySnapshot struct {
	Agents map[string][]protocol.CapabilityAction `json:"agents"`
}

// Snapshot returns a deep-copy view of the current registry: global
// default under the "system" key plus every per-agent override. Actions
// are sorted by Cmd for deterministic output. Safe for concurrent use.
func (r *CapabilityRegistry) Snapshot() CapabilitySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := CapabilitySnapshot{Agents: make(map[string][]protocol.CapabilityAction, 1+len(r.perAgent))}
	appendSorted := func(m map[string]protocol.CapabilityAction) []protocol.CapabilityAction {
		acts := make([]protocol.CapabilityAction, 0, len(m))
		for _, a := range m {
			acts = append(acts, a)
		}
		sort.Slice(acts, func(i, j int) bool { return acts[i].Cmd < acts[j].Cmd })
		return acts
	}
	out.Agents[protocol.SystemAgentID] = appendSorted(r.global)
	for agentID, m := range r.perAgent {
		out.Agents[agentID] = appendSorted(m)
	}
	return out
}

// BuiltinCmdCapabilities is the default capability set seeded at startup
// so the system works even if UE never sends a capability_registry
// message. It lists all 14 cmds the protocol defines (§2.3) with params
// schemas aligned to docs/AgentTown_CommProtocol_Values.md.
//
// UE that implements every cmd (e.g. mock_ue) is expected to send a
// capability_registry message on connect that mirrors this list; doing
// so overwrites the seed and becomes the authoritative declaration.
var BuiltinCmdCapabilities = []protocol.CapabilityAction{
	// ─── Atomic cmds (8) ─────────────────────────────────────────
	{
		Cmd:                  protocol.CmdMoveToLocation,
		Kind:                 "atomic",
		Description:          "移动到静态坐标",
		UsageHint:            "需要到达某个位置时使用",
		EstimatedDurationSec: 10,
		Params: []protocol.CapabilityParam{
			{Name: "dest", Type: "vector", Description: "目标世界坐标 [x,y,z]，单位为厘米", Required: true},
			{Name: "speed", Type: "enum", Description: "移动速度档位", DefaultValue: "walk", EnumValues: []string{"walk", "run"}},
		},
	},
	{
		Cmd:                  protocol.CmdMoveToAgent,
		Kind:                 "atomic",
		Description:          "跟随动态目标 Agent",
		UsageHint:            "目标可能移动时使用；UE 侧运行时查 Actor",
		EstimatedDurationSec: 15,
		Params: []protocol.CapabilityParam{
			{Name: "target_agent_id", Type: "string", Description: "目标 Agent ID", Required: true},
			{Name: "speed", Type: "enum", Description: "移动速度档位", DefaultValue: "walk", EnumValues: []string{"walk", "run"}},
			{Name: "stop_distance", Type: "number", Description: "停止距离（厘米）"},
			{Name: "keep_following", Type: "bool", Description: "目标移动后是否继续跟随"},
		},
	},
	{
		Cmd:                  protocol.CmdTurnTo,
		Kind:                 "atomic",
		Description:          "转向目标 Agent 或指定方向",
		EstimatedDurationSec: 5,
		Params: []protocol.CapabilityParam{
			{Name: "target_agent_id", Type: "string", Description: "目标 Agent ID（与 direction 二选一）"},
			{Name: "direction", Type: "vector", Description: "目标朝向向量 [dx,dy,dz]（与 target_agent_id 二选一）"},
		},
	},
	{
		Cmd:                  protocol.CmdPlayMontage,
		Kind:                 "atomic",
		Description:          "播放已注册的蒙太奇",
		EstimatedDurationSec: 10,
		Params: []protocol.CapabilityParam{
			{Name: "montage_id", Type: "string", Description: "蒙太奇名称", Required: true},
			{Name: "wait_finish", Type: "bool", Description: "是否等待播放完成", DefaultValue: "true"},
		},
	},
	{
		Cmd:                  protocol.CmdSpeak,
		Kind:                 "atomic",
		Description:          "对目标说话",
		UsageHint:            "target_agent_id 为空表示公开表达",
		EstimatedDurationSec: 10,
		Params: []protocol.CapabilityParam{
			{Name: "content", Type: "string", Description: "说话内容", Required: true},
			{Name: "target_agent_id", Type: "string", Description: "对话目标 ID（可空）"},
			{Name: "audio_url", Type: "string", Description: "TTS 音频 URL（可空，UE 降级为纯字幕）"},
		},
	},
	{
		Cmd:                  protocol.CmdEmote,
		Kind:                 "atomic",
		Description:          "表现情绪表情",
		EstimatedDurationSec: 5,
		Params: []protocol.CapabilityParam{
			{Name: "emotion", Type: "string", Description: "情绪类型", Required: true},
			{Name: "mode", Type: "enum", Description: "oneshot 一次性；sustained 持续到下次覆盖", DefaultValue: "oneshot", EnumValues: []string{"oneshot", "sustained"}},
		},
	},
	{
		Cmd:                  protocol.CmdWait,
		Kind:                 "atomic",
		Description:          "原地等待",
		EstimatedDurationSec: 60,
		Params: []protocol.CapabilityParam{
			{Name: "duration_sec", Type: "number", Description: "等待秒数", Required: true},
		},
	},
	{
		Cmd:                  protocol.CmdInteractSmartObject,
		Kind:                 "atomic",
		Description:          "与 Smart Object 进行一次指定交互",
		EstimatedDurationSec: 15,
		Params: []protocol.CapabilityParam{
			{Name: "target_object_id", Type: "string", Description: "目标 Smart Object ID", Required: true},
			{Name: "interaction", Type: "string", Description: "交互动作", Required: true},
		},
	},
	// ─── Composite cmds (6) ──────────────────────────────────────
	{
		Cmd:                  protocol.CmdWorkAtWorkbench,
		Kind:                 "composite",
		Description:          "去指定工作台并完成工作流程",
		UsageHint:            "日常生产工作时使用",
		EstimatedDurationSec: 7200,
		Params: []protocol.CapabilityParam{
			{Name: "target_object_id", Type: "string", Description: "目标工作台的 Smart Object ID", Required: true},
			{Name: "duration_sec", Type: "number", Description: "持续秒数（可选）"},
		},
	},
	{
		Cmd:                  protocol.CmdWorkAtWorkshop,
		Kind:                 "composite",
		Description:          "去车间、选择可用工作台并执行例行工作",
		UsageHint:            "无具体目标工作台时使用",
		EstimatedDurationSec: 7200,
		Params: []protocol.CapabilityParam{},
	},
	{
		Cmd:                  protocol.CmdChatWith,
		Kind:                 "composite",
		Description:          "接近目标、面对目标、对话并结束交流",
		UsageHint:            "社交场景使用",
		EstimatedDurationSec: 300,
		Params: []protocol.CapabilityParam{
			{Name: "target_agent_id", Type: "string", Description: "对话目标 Agent ID", Required: true},
			{Name: "topic", Type: "string", Description: "话题（可选）"},
		},
	},
	{
		Cmd:                  protocol.CmdRepairTarget,
		Kind:                 "composite",
		Description:          "接近、检查并修理指定机器人",
		UsageHint:            "维修场景使用",
		EstimatedDurationSec: 1800,
		Params: []protocol.CapabilityParam{
			{Name: "target_agent_id", Type: "string", Description: "待修理 Agent ID", Required: true},
			{Name: "tool_id", Type: "string", Description: "工具 ID（可选）"},
		},
	},
	{
		Cmd:                  protocol.CmdChargeAtStation,
		Kind:                 "composite",
		Description:          "选择或使用指定充电位，持续到满足结束条件",
		UsageHint:            "电量低时使用",
		EstimatedDurationSec: 3600,
		Params: []protocol.CapabilityParam{
			{Name: "target_object_id", Type: "string", Description: "充电桩 ID（可空，空则自动选择）"},
		},
	},
	{
		Cmd:                  protocol.CmdPatrolZone,
		Kind:                 "composite",
		Description:          "进入区域并按区域策略巡逻",
		UsageHint:            "巡检场景使用",
		EstimatedDurationSec: 1800,
		Params: []protocol.CapabilityParam{
			{Name: "target_zone", Type: "string", Description: "目标区域 ID", Required: true},
			{Name: "duration_sec", Type: "number", Description: "持续秒数（可选）"},
		},
	},
}
