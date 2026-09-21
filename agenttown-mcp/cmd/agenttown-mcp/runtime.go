package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/contract"
	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/weeklyschedule"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// Runtime is the agent-side inbound message dispatcher. It owns the
// protocol→business fan-out that used to live inline in main()'s
// SetMessageHandler closure, plus the disconnect handling, so the transport
// (wsserver) only sees two callbacks: HandleMessage and OnDisconnect.
//
// Mutable state that must stay shared with the assembly site (main) — the
// current world KB and the first-agent-registered gate — is held via
// pointers so both sides observe the same value.
type Runtime struct {
	logger *slog.Logger

	capabilityRegistry *CapabilityRegistry
	server             *mcp.Server
	executor           tools.Executor
	ws                 contract.Transport
	profiles           map[string]*profile.Profile
	weeklySched        *weeklyschedule.Schedule

	registerAgent func(string) (*agentContext, bool)
	lookupAgent   func(string) *agentContext

	agents   map[string]*agentContext
	agentsMu *sync.Mutex

	autoPlanEnabled bool
	worldKBPath     string
	worldKBManifest string
	ctx             context.Context

	// eventRouter judges non-force world events (P2-5): interrupt or
	// enqueue. nil (router disabled / no LLM backend) means every non-force
	// event just enqueues.
	eventRouter *eventRouter

	// Pointers into main's mutable locals, shared so both sides stay in sync.
	kbPtr                **worldkb.KB
	firstAgentRegistered *bool
}

// HandleMessage satisfies contract.MessageHandler. It mirrors the former
// inline switch in main(), fanning out each inbound message type to the
// corresponding agent-side handling.
func (rt *Runtime) HandleMessage(_ context.Context, msgType, agentID string, payload json.RawMessage) {
	switch msgType {
	case protocol.TypeCapabilityRegistry:
		var cr protocol.CapabilityRegistryPayload
		if err := json.Unmarshal(payload, &cr); err != nil {
			rt.logger.Warn("capability_registry parse failed", "err", err, "agent_id", agentID)
			return
		}
		rt.capabilityRegistry.Register(agentID, cr.Actions)
		rt.logger.Info("capability_registry registered",
			"agent_id", agentID, "actions", len(cr.Actions))
		tools.ReconcileTools(rt.server, rt.executor, *rt.kbPtr, rt.logger,
			rt.capabilityRegistry.EffectiveActions(protocol.SystemAgentID))

	case protocol.TypeWorldKB:
		rt.agentsMu.Lock()
		newKB, normalizeChanges, err := worldKBSwap(*rt.firstAgentRegistered, payload, rt.worldKBPath, rt.worldKBManifest)
		if err != nil {
			rt.agentsMu.Unlock()
			if errors.Is(err, errAgentWindowClosed) {
				rt.logger.Warn("world_kb rejected: agents already registered, startup window closed",
					"agent_id", agentID)
			} else {
				rt.logger.Error("world_kb merge failed, keeping existing KB",
					"err", err, "path", rt.worldKBPath)
			}
			return
		}
		if len(normalizeChanges) > 0 {
			rt.logger.Info("world_kb entity ids normalized to lowercase",
				"changes", normalizeChanges)
		}
		*rt.kbPtr = newKB
		kbRef = newKB // sync /debug/kb handler
		tools.RegisterAll(rt.server, rt.executor, newKB, rt.logger)
		rt.agentsMu.Unlock()
		rt.logger.Info("world_kb merged and persisted",
			"path", rt.worldKBPath,
			"manifest", rt.worldKBManifest,
			"zones", len(newKB.Zones),
			"objects", len(newKB.Objects),
			"agents", len(newKB.Agents),
		)

	case protocol.TypeAgentRegistered:
		ac, isNew := rt.registerAgent(agentID)
		if isNew {
			rt.logger.Info("agent_registered (new day)", "agent_id", agentID,
				"agent_epoch", ac.agentEpoch, "payload", string(payload))
		} else {
			rt.logger.Info("agent_registered (reconnect, session kept)", "agent_id", agentID,
				"agent_epoch", ac.agentEpoch)
			ac.signal()
		}

	case protocol.TypeAgentUnregistered:
		rt.agentsMu.Lock()
		ac := rt.agents[agentID]
		delete(rt.agents, agentID)
		rt.agentsMu.Unlock()
		if ac != nil {
			ac.stop()
		}
		rt.logger.Info("agent_unregistered", "agent_id", agentID, "perception_queue_cleared", true)

	case protocol.TypeHeartbeat:
		rt.logger.Debug("heartbeat", "agent_id", agentID)

	case protocol.TypeStateReport:
		var sr protocol.StateReportPayload
		if err := json.Unmarshal(payload, &sr); err != nil {
			rt.logger.Warn("state_report parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil {
			rt.logger.Warn("state_report dropped for unregistered agent", "agent_id", agentID)
			return
		}
		detail := ac.updateState(sr)
		rt.logger.Info("state_report", "agent_id", agentID,
			"energy", sr.PhysicalState.Energy, "fatigue", sr.PhysicalState.Fatigue,
			"joint_wear", sr.PhysicalState.JointWear)
		// P4-12：物理警戒带突破 → 合成 physical_threshold world_event 入队
		// （路由器裁决 interrupt/入队）。
		if detail != "" && rt.autoPlanEnabled {
			rt.dispatchSynthesizedEvent(ac, agentID, detail)
		}

	case protocol.TypeActionCompleted:
		var completed protocol.ActionCompletedPayload
		if err := json.Unmarshal(payload, &completed); err != nil {
			rt.logger.Warn("action_completed parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil {
			rt.logger.Warn("action_completed dropped for unregistered agent", "agent_id", agentID)
			return
		}
		queued, detail := ac.recordActionCompletion(completed)
		rt.logger.Info("action_completed", "agent_id", agentID,
			"action_id", completed.ActionID, "result", completed.Result,
			"reason", completed.Reason,
			"decision_queued", queued)
		// P4-12：异常完成（failed/interrupted/error）→ 合成 action_anomaly
		// world_event 入队。成功完成不合成——是常态。
		if detail != "" && completed.Result != protocol.ResultSuccess && rt.autoPlanEnabled {
			rt.dispatchSynthesizedEvent(ac, agentID, detail)
		}

	case protocol.TypeActionQueued:
		var aq protocol.ActionQueuedPayload
		if err := json.Unmarshal(payload, &aq); err != nil {
			rt.logger.Warn("action_queued parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil {
			rt.logger.Warn("action_queued dropped for unregistered agent", "agent_id", agentID)
			return
		}
		ac.as.RecordQueueStatus(aq)
		rt.logger.Info("action_queued", "agent_id", agentID,
			"action_id", aq.ActionID, "status", aq.Status,
			"group", aq.Group, "position", aq.Position)

	case protocol.TypeWorldEvent:
		var ev protocol.WorldEventPayload
		if err := json.Unmarshal(payload, &ev); err != nil {
			rt.logger.Warn("world_event parse failed", "err", err, "agent_id", agentID)
			return
		}
		rt.handleWorldEvent(agentID, ev)

	case protocol.TypeEventNotification:
		var event protocol.EventNotificationPayload
		if err := json.Unmarshal(payload, &event); err != nil {
			rt.logger.Warn("event_notification parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil {
			rt.logger.Warn("event_notification dropped for unregistered agent", "agent_id", agentID)
			return
		}
		detail := ac.recordEventNotification(event)
		rt.logger.Info("event_notification", "agent_id", agentID,
			"event_id", event.EventID, "perception_level", event.PerceptionLevel)
		// P4-12：event_notification（Director 注入）→ 合成 world world_event
		// 入队，走事件系统。旧反应层不复存在。
		if rt.autoPlanEnabled {
			rt.dispatchSynthesizedEvent(ac, agentID, "event_notification "+detail)
		}

	case protocol.TypeError:
		var ep protocol.ErrorPayload
		if err := json.Unmarshal(payload, &ep); err != nil {
			rt.logger.Warn("error from ue (payload parse failed)",
				"agent_id", agentID, "raw", string(payload), "err", err)
		} else {
			rt.logger.Warn("error from ue",
				"agent_id", agentID,
				"error_code", ep.ErrorCode, "message", ep.Message,
				"action_id", ep.ActionID)
			recordUEError(agentID, ep)
		}

	case protocol.TypePerceptionUpdate:
		ac := rt.lookupAgent(agentID)
		if ac == nil {
			rt.logger.Warn("perception_update dropped for unregistered agent", "agent_id", agentID)
			return
		}
		ac.coordMu.Lock()
		if ac.stopped {
			ac.stopped = false
			ac.online = true
			workerCtx, cancel := context.WithCancel(rt.ctx)
			ac.cancel = cancel
			ac.coordMu.Unlock()
			rt.logger.Info("agent hot-reconnected via perception, restarting worker", "agent_id", agentID)
			go runPerceptionWorker(workerCtx, agentID, ac, rt.ws, *rt.kbPtr, rt.profiles, rt.weeklySched, rt.logger)
		} else {
			ac.coordMu.Unlock()
		}
		detail, err := ac.observePerception(payload)
		if err != nil {
			rt.logger.Warn("perception_update parse failed", "agent_id", agentID, "err", err)
			return
		}
		// P4-12：物理警戒带突破 → 合成 physical_threshold world_event 入队。
		if detail != "" && rt.autoPlanEnabled {
			rt.dispatchSynthesizedEvent(ac, agentID, detail)
		}

	case protocol.TypeChatInvite:
		// P4-14：UE 不再发送独立的 chat_invite——对话邀请统一按
		// world_event（social.chat_invite_incoming）推送，本分支仅为
		// 兼容旧 UE 保留（UE 切换后删除）。收到时转为 world_event 再分发，
		// 与新路径同一管道。
		var invite protocol.ChatInvitePayload
		if err := json.Unmarshal(payload, &invite); err != nil {
			rt.logger.Warn("chat_invite parse failed (legacy path)", "err", err)
			return
		}
		ev := protocol.WorldEventPayload{
			EventID:   "legacy_invite_" + invite.ConvID,
			Category:  protocol.CategorySocial,
			EventType: protocol.EventTypeChatInviteIncoming,
			Force:     false,
			Severity:  5,
			Subject:   invite.FromAgentID,
			Data: mustMarshal(map[string]any{
				"conv_id": invite.ConvID,
				"from":    invite.FromAgentID,
				"content": invite.Content,
			}),
		}
		rt.handleWorldEvent(agentID, ev)

	case protocol.TypeChatInviteRsp:
		var rsp protocol.ChatInviteRspPayload
		if err := json.Unmarshal(payload, &rsp); err != nil {
			rt.logger.Warn("chat_invite_rsp parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil || ac.dialogue == nil {
			rt.logger.Debug("chat_invite_rsp dropped (agent unregistered or dialogue disabled)", "agent_id", agentID)
			return
		}
		go ac.dialogue.handleInviteRsp(rt.ctx, rsp)

	case protocol.TypeChatTurn:
		var turn protocol.ChatTurnPayload
		if err := json.Unmarshal(payload, &turn); err != nil {
			rt.logger.Warn("chat_turn parse failed", "err", err)
			return
		}
		ac := rt.lookupAgent(agentID)
		if ac == nil || ac.dialogue == nil {
			rt.logger.Debug("chat_turn dropped (agent unregistered or dialogue disabled)", "agent_id", agentID)
			return
		}
		go ac.dialogue.handleTurn(rt.ctx, turn)

	default:
		rt.logger.Debug("unhandled message type", "type", msgType, "agent_id", agentID)
	}
}

// OnDisconnect satisfies contract.DisconnectHandler. On the active connection
// ending, stop all agents but keep them registered for hot-reconnect.
func (rt *Runtime) OnDisconnect() {
	rt.agentsMu.Lock()
	stopped := make([]*agentContext, 0, len(rt.agents))
	for _, ac := range rt.agents {
		stopped = append(stopped, ac)
	}
	rt.agentsMu.Unlock()
	for _, ac := range stopped {
		ac.stop()
	}
	if len(stopped) > 0 {
		rt.logger.Info("ws disconnected, stopped all agents (agents kept for hot-reconnect)", "agent_count", len(stopped))
	}
}
