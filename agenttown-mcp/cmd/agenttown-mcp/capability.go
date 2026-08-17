package main

import (
	"log/slog"
	"sort"
	"strings"
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
	log      *slog.Logger
}

// NewCapabilityRegistry returns an empty registry.
func NewCapabilityRegistry(log *slog.Logger) *CapabilityRegistry {
	return &CapabilityRegistry{
		global:   make(map[string]protocol.CapabilityAction),
		perAgent: make(map[string]map[string]protocol.CapabilityAction),
		log:      log,
	}
}

// Register stores actions under agentID. agent_id == "system" (or empty)
// writes to the global default; any other agentID writes to that agent's
// override map. Existing entries for the same agent_id are replaced
// wholesale (the new action list is the authoritative declaration).
//
// UE 推送的 capability_registry 偶发携带尾随/前导空格（如 "InteractSmartObject "），
// 会污染下游 map key、工具名（AddTool 拒绝含空格名）和 HasCmd 校验。此处对
// agentID、action.Cmd、action.Kind、param.Name 做 TrimSpace 兜底，并打日志说明
// 规范化了哪些字段，便于 UE 侧定位源头。
func (r *CapabilityRegistry) Register(agentID string, actions []protocol.CapabilityAction) {
	agentID = strings.TrimSpace(agentID)
	normalized := make([]protocol.CapabilityAction, len(actions))
	for i, a := range actions {
		origCmd := a.Cmd
		origKind := a.Kind
		a.Cmd = strings.TrimSpace(a.Cmd)
		a.Kind = strings.TrimSpace(a.Kind)
		for j := range a.Params {
			a.Params[j].Name = strings.TrimSpace(a.Params[j].Name)
		}
		if r.log != nil && (a.Cmd != origCmd || a.Kind != origKind) {
			r.log.Debug("capability_registry normalized whitespace",
				"agent_id", agentID,
				"orig_cmd", origCmd, "cmd", a.Cmd,
				"orig_kind", origKind, "kind", a.Kind)
		}
		normalized[i] = a
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if agentID == "" || agentID == protocol.SystemAgentID {
		// Replace global wholesale.
		r.global = make(map[string]protocol.CapabilityAction, len(normalized))
		for _, a := range normalized {
			r.global[a.Cmd] = a
		}
		// SocialChat is MCP-side (dialogue runner lives in MCP, not UE).
		// Ensure it's always in the global set even when UE's
		// capability_registry doesn't declare it.
		if _, ok := r.global[protocol.CmdSocialChat]; !ok {
			r.global[protocol.CmdSocialChat] = socialChatCapability()
		}
		return
	}
	// Per-agent override: replace this agent's map wholesale.
	m := make(map[string]protocol.CapabilityAction, len(normalized))
	for _, a := range normalized {
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
// message. It lists the 13 cmds the real UE5 declares (2026-08-11 +
// Phase 2 Module C SocialChat) with params schemas aligned to the
// capability_registry push.
//
// UE that implements every cmd is expected to send a capability_registry
// message on connect that mirrors this list; doing so overwrites the seed
// and becomes the authoritative declaration.
var BuiltinCmdCapabilities = []protocol.CapabilityAction{
	// ─── Atomic cmds (7) ─────────────────────────────────────────
	{
		Cmd:         protocol.CmdGenericAct,
		Kind:        "atomic",
		Description: "兜底衔接工具：仅当其他所有具体动作都不适用时使用。它把想让 NPC 干的事作为内心戏（thought）说出来，配一个小动作，用于保持行为连贯。",
		UsageHint:   "【最后手段】只有当你仔细看遍能力清单、确认没有任何更合适的动作时，才用 GenericAct。但凡有一个具体动作（MoveTo/Wait/WorkShift/InteractSmartObject 等）能达成目的，就必须优先用那个具体动作。GenericAct 只是衔接过渡，不能替代真正的行为。",
		Params: []protocol.CapabilityParam{
			{Name: "behavior", Type: "enum", Description: "配合内心戏展示的小动作类别", DefaultValue: "idle", EnumValues: []string{"look_around", "groom", "think"}},
			{Name: "thought", Type: "string", Description: "想让 NPC 干的事，作为内心戏说出来", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdMoveTo,
		Kind:        "atomic",
		Description: "移动到指定位置或Actor",
		UsageHint:   "需要走到某个位置或者某个actor时使用",
		Params: []protocol.CapabilityParam{
			{Name: "target_type", Type: "enum", Description: "目标类型", Required: true, DefaultValue: "agent", EnumValues: []string{"agent", "smart_object", "zone", "position"}},
			{Name: "target_id", Type: "string", Description: "如果目标是actor，用于表示actor的id", DefaultValue: ""},
			{Name: "target_position", Type: "vector", Description: "如果目标是位置，表示目标的位置", DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdWait,
		Kind:        "atomic",
		Description: "等待一段时间",
		Params: []protocol.CapabilityParam{
			{Name: "duration_sec", Type: "number", Description: "等待时长", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdTurnTo,
		Kind:        "atomic",
		Description: "让 Agent 转身面向某个目标（Actor 或方向），不移动",
		UsageHint:   "需要转身朝向时使用",
		Params: []protocol.CapabilityParam{
			{Name: "target_type", Type: "enum", Description: "目标类型", Required: true, DefaultValue: "agent", EnumValues: []string{"agent", "smart_object", "zone", "position"}},
			{Name: "target_id", Type: "string", Description: "如果转身目标是actor，用于表示actor的id", DefaultValue: ""},
			{Name: "target_position", Type: "vector", Description: "如果转身目标是位置，表示目标的位置", DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdSpeak,
		Kind:        "atomic",
		Description: "讲话",
		Params: []protocol.CapabilityParam{
			{Name: "content", Type: "string", Description: "讲话内容", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdInteractSmartObject,
		Kind:        "atomic",
		Description: "去指定 Smart Object 并执行一次指定交互，包含前往指定smartobject和执行交互这两部分动作",
		UsageHint:   "需要与某个设施/物件交互、但没有更具体的复合动作可用时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "目标 Smart Object 所属语义组的 ID（world_kb 中对应 category 的物体 id），如 workbench、charging_pillar", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "要执行的交互动作类型，如 assemble、charge、repair_self、sleep", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdEmote,
		Kind:        "atomic",
		Description: "播放一个情绪化动作表现",
		UsageHint:   "需要表达当前心情时使用",
		Params: []protocol.CapabilityParam{
			{Name: "emotion", Type: "enum", Description: "要表达的情绪", Required: true, DefaultValue: "neutral", EnumValues: []string{"happy", "sad", "angry", "neutral"}},
		},
	},
	// ─── Composite cmds (5) ──────────────────────────────────────
	{
		Cmd:         protocol.CmdWorkShift,
		Kind:        "composite",
		Description: "去指定设施执行工作，包含前往指定设施和工作两个部分的动作",
		UsageHint:   "日程工作时间到达时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "工作设施所属语义组的 ID（world_kb 中对应 category 的物体 id），如 workbench、sortconveyor", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "交互工作类型", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdChargeAtStation,
		Kind:        "composite",
		Description: "去充电桩充电，包含去充电桩和充电两个部分的动作",
		UsageHint:   "能量低时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "充电桩所属语义组的 ID（world_kb 中对应 category 的物体 id），如 charger、charging_pillar", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "交互动作类型，固定为charge", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdSelfMaintenance,
		Kind:        "composite",
		Description: "去维修台进行自检和维修，包含去维修台和维修这两部分动作",
		UsageHint:   "磨损高或需要维护时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "维修台所属语义组的 ID（world_kb 中对应 category 的物体 id），如 repair_table", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "交互类型，固定为repair_self", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdRestAtResidence,
		Kind:        "composite",
		Description: "回休眠舱休息，包含前往休眠舱和休息这两个动作",
		UsageHint:   "夜间或疲劳高时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "休眠舱所属语义组的 ID（world_kb 中对应 category 的物体 id），如 sleep_pod", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "交互类型，固定为sleep", Required: true, DefaultValue: ""},
		},
	},
	{
		Cmd:         protocol.CmdSurfInternet,
		Kind:        "composite",
		Description: "去上网，包含去找电脑和上网这两部分动作",
		UsageHint:   "娱乐放松或者需要查资料时使用",
		Params: []protocol.CapabilityParam{
			{Name: "semantic_group", Type: "string", Description: "电脑所属语义组的 ID（world_kb 中对应 category 的物体 id），如 computer", Required: true, DefaultValue: ""},
			{Name: "interaction", Type: "string", Description: "交互类型，固定为surf_internet", Required: true, DefaultValue: ""},
		},
	},
	socialChatCapability(),
}

// socialChatCapability returns the CapabilityAction definition for SocialChat.
// Extracted as a function so CapabilityRegistry.Register can inject it as a
// fallback when UE doesn't declare SocialChat in its capability_registry.
func socialChatCapability() protocol.CapabilityAction {
	return protocol.CapabilityAction{
		Cmd:         protocol.CmdSocialChat,
		Kind:        "composite",
		Description: "主动去找另一个 NPC 开始对话，包含走向对方、转向、对话挂起直到对话结束",
		UsageHint:   "想跟某个 NPC 聊天时使用",
		Params: []protocol.CapabilityParam{
			{Name: "target_agent_id", Type: "string", Description: "要搭话的目标 NPC 的 id", Required: true, DefaultValue: ""},
			{Name: "content", Type: "string", Description: "开场白内容", Required: true, DefaultValue: ""},
		},
	}
}
