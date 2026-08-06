"""
Mock UE Bridge (async) — simulates the UE5 process per the AgentTown
communication protocol (docs/AgentTown_CommProtocol_Values.md).

Mock UE is the WebSocket CLIENT; agenttown-mcp is the server. All messages
use the 7-field envelope. Mock UE owns physical + spatial state (UE side)
and reports it via state_report (authoritative) + perception_update delta.

Message flow:
  - On connect: agent_registered
  - Every 5s: heartbeat (agent_id="system")
  - Periodically: perception_update (spatial + environment)
  - On physical change >threshold / every 15s: state_report
  - Inbound action_command → action_started (ACK ≤2s) → action_completed
  - Inbound stop_action → validate action_id → interrupted / STOP_ID_MISMATCH
  - Inbound scan_area → immediate perception_update
"""

import asyncio
import json
import logging
import math
import os
import time as _time
import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Dict, List, Optional, Set, Tuple

import websockets
import yaml

logger = logging.getLogger("agenttown.mock_ue")

# ─── Protocol constants (mirror pkg/protocol) ──────────────────
PROTOCOL_VERSION = "1.0"
SYSTEM_AGENT_ID = "system"

TYPE_PERCEPTION_UPDATE = "perception_update"
TYPE_ACTION_COMMAND = "action_command"
TYPE_ACTION_STARTED = "action_started"
TYPE_ACTION_COMPLETED = "action_completed"
TYPE_STOP_ACTION = "stop_action"
TYPE_STATE_REPORT = "state_report"
TYPE_AGENT_REGISTERED = "agent_registered"
TYPE_AGENT_UNREGISTERED = "agent_unregistered"
TYPE_HEARTBEAT = "heartbeat"
TYPE_ERROR = "error"
TYPE_SCAN_AREA = "scan_area"
TYPE_NARRATIVE = "narrative"  # MCP → Mock UE, display only
TYPE_RESYNC = "resync"        # reconnect: exchange last_received_seq (约定11)
TYPE_EVENT_LOST = "event_lost"  # reconnect: buffer rollover warning
TYPE_EVENT_NOTIFICATION = "event_notification"  # director-injected event (P1 实时通道)
TYPE_CAPABILITY_REGISTRY = "capability_registry"  # UE → MCP: declare NPC cmds
TYPE_WORLD_KB = "world_kb"  # UE → MCP: push full world KB (generated + authored) on connect

# cmd constants (14: 8 atomic + 6 composite, per protocol §2.3)
CMD_MOVE_TO_LOCATION = "MoveToLocation"
CMD_MOVE_TO_AGENT = "MoveToAgent"
CMD_TURN_TO = "TurnTo"
CMD_PLAY_MONTAGE = "PlayMontage"
CMD_SPEAK = "Speak"
CMD_EMOTE = "Emote"
CMD_WAIT = "Wait"
CMD_INTERACT = "InteractSmartObject"
CMD_WORK_AT_WORKBENCH = "WorkAtWorkbench"
CMD_WORK_AT_WORKSHOP = "WorkAtWorkshop"
CMD_CHAT_WITH = "ChatWith"
CMD_REPAIR_TARGET = "RepairTarget"
CMD_CHARGE_AT_STATION = "ChargeAtStation"
CMD_PATROL_ZONE = "PatrolZone"

# Default capability actions advertised to MCP on connect. Each entry
# mirrors the MCP-side CapabilityAction struct (pkg/protocol/messages.go):
# {cmd, kind, description, usage_hint, estimated_duration_sec, params}.
#
# This is the authoritative global default (agent_id="system"). Tests or
# alternate worlds can override by passing `capability_actions=` to
# MockUE.__init__. If a future mock_ue drops support for a cmd (e.g.
# stops honoring PlayMontage), removing it here makes MCP stop
# advertising tools that depend on it.
DEFAULT_CAPABILITY_ACTIONS: List[Dict[str, Any]] = [
    {
        "cmd": CMD_MOVE_TO_LOCATION,
        "kind": "atomic",
        "description": "移动到静态坐标",
        "usage_hint": "需要到达某个位置时使用；dest 由 MCP 解析为 [x,y,z] 坐标",
        "estimated_duration_sec": 10,
        "params": [
            {"name": "dest", "type": "vector",
             "description": "目标世界坐标 [x,y,z]，单位为厘米", "required": True},
            {"name": "speed", "type": "enum",
             "description": "移动速度档位", "required": False,
             "default_value": "walk", "enum_values": ["walk", "run"]},
        ],
    },
    {
        "cmd": CMD_MOVE_TO_AGENT,
        "kind": "atomic",
        "description": "移动到动态 agent 身边",
        "usage_hint": "需要靠近或跟随其他 agent 时使用",
        "estimated_duration_sec": 10,
        "params": [
            {"name": "target_agent_id", "type": "string",
             "description": "目标 agent ID", "required": True},
            {"name": "speed", "type": "enum",
             "description": "移动速度档位", "required": False,
             "default_value": "walk", "enum_values": ["walk", "run"]},
            {"name": "stop_distance", "type": "number",
             "description": "停止距离（厘米）", "required": False,
             "default_value": "150"},
            {"name": "keep_following", "type": "bool",
             "description": "true=持续跟随；false=到达后停止", "required": False,
             "default_value": "false"},
        ],
    },
    {
        "cmd": CMD_TURN_TO,
        "kind": "atomic",
        "description": "转身面向目标",
        "usage_hint": "需要转向某个 agent 或方向时使用",
        "estimated_duration_sec": 5,
        "params": [
            {"name": "target_agent_id", "type": "string",
             "description": "目标 agent ID（与 direction 二选一）", "required": False},
            {"name": "direction", "type": "vector",
             "description": "方向向量 [dx,dy,dz]（与 target_agent_id 二选一）", "required": False},
        ],
    },
    {
        "cmd": CMD_PLAY_MONTAGE,
        "kind": "atomic",
        "description": "播放蒙太奇动画",
        "usage_hint": "需要播放特定动画时使用；空闲情绪表达优先用 Emote",
        "estimated_duration_sec": 10,
        "params": [
            {"name": "montage_id", "type": "string",
             "description": "蒙太奇动画 ID", "required": True},
            {"name": "wait_finish", "type": "bool",
             "description": "是否等待动画播放完成", "required": False,
             "default_value": "true"},
        ],
    },
    {
        "cmd": CMD_SPEAK,
        "kind": "atomic",
        "description": "对目标说话",
        "usage_hint": "target_agent_id 可空表示自言自语；content 控制话语长度",
        "estimated_duration_sec": 10,
        "params": [
            {"name": "content", "type": "string",
             "description": "说话内容", "required": True},
            {"name": "target_agent_id", "type": "string",
             "description": "对话目标 agent ID（可空）", "required": False},
            {"name": "audio_url", "type": "string",
             "description": "可选音频 URL", "required": False},
        ],
    },
    {
        "cmd": CMD_EMOTE,
        "kind": "atomic",
        "description": "表现情绪表情",
        "usage_hint": "mode=oneshot 一次性表情；mode=sustained 持续到下次覆盖",
        "estimated_duration_sec": 5,
        "params": [
            {"name": "emotion", "type": "string",
             "description": "情绪类型", "required": True},
            {"name": "mode", "type": "enum",
             "description": "oneshot 或 sustained", "required": False,
             "default_value": "oneshot", "enum_values": ["oneshot", "sustained"]},
        ],
    },
    {
        "cmd": CMD_WAIT,
        "kind": "atomic",
        "description": "原地等待",
        "usage_hint": "duration_sec 上限 600；更长等待应使用复合行为",
        "estimated_duration_sec": 60,
        "params": [
            {"name": "duration_sec", "type": "number",
             "description": "等待秒数", "required": True},
        ],
    },
    {
        "cmd": CMD_INTERACT,
        "kind": "atomic",
        "description": "与智能对象交互",
        "usage_hint": "target_object_id 必须存在于 world_kb.objects；interaction 取值见该对象的 available_interactions",
        "estimated_duration_sec": 15,
        "params": [
            {"name": "target_object_id", "type": "string",
             "description": "智能对象 ID", "required": True},
            {"name": "interaction", "type": "string",
             "description": "交互动作", "required": True},
        ],
    },
    {
        "cmd": CMD_WORK_AT_WORKBENCH,
        "kind": "composite",
        "description": "在工作台装配",
        "usage_hint": "target_object_id 为工作台 ID；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
            {"name": "target_object_id", "type": "string",
             "description": "工作台 ID", "required": True},
            {"name": "duration_sec", "type": "number",
             "description": "持续秒数（可选）", "required": False},
        ],
    },
    {
        "cmd": CMD_WORK_AT_WORKSHOP,
        "kind": "composite",
        "description": "车间例行工作",
        "usage_hint": "无需特定目标；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
            {"name": "duration_sec", "type": "number",
             "description": "持续秒数（可选）", "required": False},
        ],
    },
    {
        "cmd": CMD_CHAT_WITH,
        "kind": "composite",
        "description": "与其他 agent 聊天",
        "usage_hint": "target_agent_id 必填；topic 可选",
        "estimated_duration_sec": 300,
        "params": [
            {"name": "target_agent_id", "type": "string",
             "description": "聊天目标 agent ID", "required": True},
            {"name": "topic", "type": "string",
             "description": "聊天话题（可选）", "required": False},
        ],
    },
    {
        "cmd": CMD_REPAIR_TARGET,
        "kind": "composite",
        "description": "修理目标 agent",
        "usage_hint": "target_agent_id 必填；tool_id 可选",
        "estimated_duration_sec": 600,
        "params": [
            {"name": "target_agent_id", "type": "string",
             "description": "要修理的 agent ID", "required": True},
            {"name": "tool_id", "type": "string",
             "description": "工具 ID（可选）", "required": False},
        ],
    },
    {
        "cmd": CMD_CHARGE_AT_STATION,
        "kind": "composite",
        "description": "充电",
        "usage_hint": "target_object_id 可空（自动选最近充电站）；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
            {"name": "target_object_id", "type": "string",
             "description": "充电站 ID（可空）", "required": False},
            {"name": "duration_sec", "type": "number",
             "description": "充电秒数（可选）", "required": False},
        ],
    },
    {
        "cmd": CMD_PATROL_ZONE,
        "kind": "composite",
        "description": "巡逻区域",
        "usage_hint": "target_zone 必填；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
            {"name": "target_zone", "type": "string",
             "description": "巡逻区域 ID", "required": True},
            {"name": "duration_sec", "type": "number",
             "description": "巡逻秒数（可选）", "required": False},
        ],
    },
]

# result constants
RESULT_SUCCESS = "success"
RESULT_FAILED = "failed"
RESULT_INTERRUPTED = "interrupted"

# error codes
ERR_STOP_ID_MISMATCH = "STOP_ID_MISMATCH"
ERR_ACTION_FAILED = "ACTION_FAILED"
ERR_UNKNOWN_AGENT = "UNKNOWN_AGENT"

# ─── World geometry (UE5 centimeters; small sim coords ×100) ────
SCALE = 100.0  # convert legacy small coords to cm

# World geometry now loaded from assets/world_kb.yaml via load_world_kb().
# See WorldKB dataclass below and MockUE.__init__.


@dataclass
class ZoneInfo:
    """One zone entry from world_kb.yaml."""
    id: str
    display_name: str
    entry_point: List[float]
    bounds: Tuple[float, float, float, float]  # (x_min, x_max, y_min, y_max)


@dataclass
class ObjectInfo:
    """One object entry from world_kb.yaml.

    New schema (2026-07) merges the former `locations[]` segment into
    `objects[]` — each object now carries both narrative (display_name,
    description) and spatial fields (actor_position, interaction_point,
    interaction_radius) together. The interaction_radius is used by
    which_object() to reverse-lookup the NPC's current object by
    position; it defaults to 1500cm when not authored.
    """
    id: str
    display_name: str
    description: str = ""
    category: str = ""
    zone_id: str = ""
    actor_class: str = ""
    actor_position: List[float] = field(default_factory=lambda: [0, 0, 0])
    interaction_point: List[float] = field(default_factory=lambda: [0, 0, 0])
    interaction_facing: List[float] = field(default_factory=lambda: [0, 0, 0])
    interaction_radius: float = 0.0
    available_interactions: List[str] = field(default_factory=list)
    default_state: str = ""
    tags: List[str] = field(default_factory=list)


@dataclass
class AgentInfo:
    """One agent entry from world_kb.yaml. Fields align with the MCP
    Go-side worldkb.Agent struct (pkg/worldkb/types.go)."""
    id: str
    display_name: str = ""
    description: str = ""
    type: str = ""
    profession: str = ""
    personality: Dict[str, Any] = field(default_factory=dict)
    initial_zone: str = ""
    initial_position: List[float] = field(default_factory=lambda: [0, 0, 0])
    actor_class: str = ""
    action_table: str = ""
    main_behavior_tree: str = ""


@dataclass
class WorldKB:
    """In-memory World Knowledge Base loaded from world_kb.yaml.

    Replaces the previous module-level hardcoded dicts (ZONE_BOUNDS,
    ZONE_ENTRIES, LOCATION_POINTS, OBJECT_META, ZONE_OBJECTS). The YAML
    file is the single source of truth — agenttown-mcp loads the same
    file, so UE-side and MCP-side semantic→coordinate mappings cannot
    drift.
    """
    zones: Dict[str, ZoneInfo] = field(default_factory=dict)
    objects: Dict[str, ObjectInfo] = field(default_factory=dict)
    agents: List[AgentInfo] = field(default_factory=list)
    narrative: Dict[str, str] = field(default_factory=dict)
    version: str = ""

    def zone_entry(self, zone_id: str) -> Optional[List[float]]:
        z = self.zones.get(zone_id)
        return list(z.entry_point) if z else None

    def object_point(self, object_id: str) -> Optional[List[float]]:
        o = self.objects.get(object_id)
        return list(o.interaction_point) if o else None

    def resolve_position(self, target: str) -> Optional[List[float]]:
        """Resolve a semantic ID to a coordinate. Zones use entry_point,
        objects use interaction_point. Returns None if unknown."""
        return self.zone_entry(target) or self.object_point(target)

    def is_target(self, target: str) -> bool:
        return target in self.zones or target in self.objects

    def which_zone(self, pos: List[float]) -> str:
        """Reverse-lookup zone by position via AABB (X/Y only)."""
        x, y = pos[0], pos[1]
        for zid, z in self.zones.items():
            xmin, xmax, ymin, ymax = z.bounds
            if xmin <= x <= xmax and ymin <= y <= ymax:
                return zid
        return ""

    def which_object(self, pos: List[float]) -> Optional[str]:
        """Reverse-lookup object by position via interaction_radius (X/Y only).

        Uses the actor_position + interaction_radius pair — NPC is
        considered "at" an object when within its interaction radius.
        """
        x, y = pos[0], pos[1]
        for oid, obj in self.objects.items():
            if obj.interaction_radius <= 0:
                continue
            dx, dy = x - obj.actor_position[0], y - obj.actor_position[1]
            if (dx * dx + dy * dy) <= obj.interaction_radius * obj.interaction_radius:
                return oid
        return None

    def objects_in_zone(self, zone_id: str) -> List[str]:
        """Return object IDs registered under a zone (by zone_id field)."""
        return [oid for oid, obj in self.objects.items() if obj.zone_id == zone_id]


# Default values for generated-document header fields. editor_label and
# actor_path are UE-editor metadata that the merged yaml does not keep
# (they are generated-only); _send_world_kb fills them with empty strings
# so the MCP merge pipeline receives a structurally-complete payload.
_GENERATOR_DEFAULT = {"name": "MockUE", "version": "0.1.0"}
_SOURCE_DEFAULT = {"map_package": "", "map_name": ""}
_COORD_SYSTEM_DEFAULT = {
    "space": "UE5_world", "distance_unit": "centimeter",
    "rotation_unit": "degree", "rotation_order": "pitch_yaw_roll",
}


def split_yaml_to_generated_authored(yaml_data: Dict[str, Any]) -> Tuple[Dict[str, Any], Dict[str, Any]]:
    """Split a merged world_kb.yaml dict back into generated/authored halves.

    The MCP merge pipeline (MergeAndWriteBytes) expects the two blobs
    separately, but the on-disk yaml is the merged product. This function
    reverses the merge by applying the field-ownership rules from
    agenttown-mcp/pkg/worldkb/schema.go (protectedZoneFields /
    protectedObjectFields / protectedAgentFields):

      - Spatial/fact fields (bounds, entry_point, actor_position, ...) go
        to `generated`.
      - Narrative fields (display_name, description, aliases, tags,
        profession, personality, relationships, connections) go to
        `authored`.
      - generated-only editor metadata (editor_label, actor_path) is not
        preserved in the merged yaml; we fill empty strings here. MCP
        does not consume these fields downstream.

    `relationships` is per-agent in authored; the merged yaml stores it
    at top-level `relationships`, so we read from there when per-agent
    entries are absent.
    """
    # ─── generated half ──────────────────────────────────────────
    gen_zones = []
    for z in yaml_data.get("zones", []) or []:
        bounds = z.get("bounds", {}) or {}
        gen_zones.append({
            "id": z.get("id", ""),
            "editor_label": "",
            "actor_path": "",
            "bounds": {
                "center": bounds.get("center", [0, 0, 0]),
                "extent": bounds.get("extent", [0, 0, 0]),
                "rotation": bounds.get("rotation", [0, 0, 0]),
            },
            "entry_point": z.get("entry_point", [0, 0, 0]),
            "entry_facing": z.get("entry_facing", [0, 0, 0]),
        })

    gen_objects = []
    for o in yaml_data.get("objects", []) or []:
        gen_objects.append({
            "id": o.get("id", ""),
            "category": o.get("category", ""),
            "zone_id": o.get("zone_id", ""),
            "editor_label": "",
            "actor_class": o.get("actor_class", ""),
            "actor_position": o.get("actor_position", [0, 0, 0]),
            "interaction_point": o.get("interaction_point", [0, 0, 0]),
            "interaction_facing": o.get("interaction_facing", [0, 0, 0]),
            "available_interactions": o.get("available_interactions", []) or [],
            "default_state": o.get("default_state", ""),
        })

    gen_agents = []
    for a in yaml_data.get("agents", []) or []:
        gen_agents.append({
            "id": a.get("id", ""),
            "type": a.get("type", ""),
            "initial_zone": a.get("initial_zone", ""),
            "editor_label": "",
            "actor_class": a.get("actor_class", ""),
            "initial_position": a.get("initial_position", [0, 0, 0]),
            "action_table": a.get("action_table", ""),
            "main_behavior_tree": a.get("main_behavior_tree", ""),
        })

    generated = {
        "$schema": "agenttown-world-generated/v1",
        "schema_version": yaml_data.get("version", "1.0"),
        "generated_at": _time.strftime("%Y-%m-%dT%H:%M:%SZ", _time.gmtime()),
        "generator": dict(_GENERATOR_DEFAULT),
        "source": dict(_SOURCE_DEFAULT),
        "coordinate_system": dict(_COORD_SYSTEM_DEFAULT),
        "zones": gen_zones,
        "objects": gen_objects,
        "agents": gen_agents,
        "validation_summary": {"errors": 0, "warnings": 0},
    }

    # ─── authored half ───────────────────────────────────────────
    auth_zones: Dict[str, Any] = {}
    for z in yaml_data.get("zones", []) or []:
        zid = z.get("id", "")
        if not zid:
            continue
        auth_zones[zid] = {
            "display_name": z.get("display_name", zid),
            "description": z.get("description", ""),
            "aliases": z.get("aliases", []) or [],
            "connections": z.get("connections", []) or [],
        }

    auth_objects: Dict[str, Any] = {}
    for o in yaml_data.get("objects", []) or []:
        oid = o.get("id", "")
        if not oid:
            continue
        auth_objects[oid] = {
            "display_name": o.get("display_name", oid),
            "description": o.get("description", ""),
            "tags": o.get("tags", []) or [],
        }

    auth_agents: Dict[str, Any] = {}
    for a in yaml_data.get("agents", []) or []:
        aid = a.get("id", "")
        if not aid:
            continue
        auth_agents[aid] = {
            "display_name": a.get("display_name", aid),
            "description": a.get("description", ""),
            "profession": a.get("profession", ""),
            "personality": a.get("personality", {}) or {},
            "initial_zone": a.get("initial_zone", ""),
            "relationships": a.get("relationships", []) or [],
        }

    authored = {
        "version": yaml_data.get("version", "1.0"),
        "narrative": yaml_data.get("narrative", {}) or {},
        "zones": auth_zones,
        "objects": auth_objects,
        "agents": auth_agents,
    }

    return generated, authored


def load_world_kb(path: str) -> Tuple[WorldKB, Dict[str, Any]]:
    """Load world_kb.yaml into a WorldKB instance + the raw yaml dict.

    The raw dict is kept so _send_world_kb can split it back into
    generated/authored halves for the MCP merge pipeline (which needs
    the two blobs separately).

    Raises FileNotFoundError / ValueError on structural errors. Callers
    should treat any exception as fatal — Mock UE cannot resolve semantic
    targets or generate nearby_objects without a valid KB.
    """
    with open(path, "r", encoding="utf-8") as f:
        data = yaml.safe_load(f)

    kb = WorldKB()
    for raw_zone in data.get("zones", []) or []:
        zid = raw_zone.get("id", "")
        if not zid:
            raise ValueError(f"zone missing id in {path}: {raw_zone}")
        if zid in kb.zones:
            raise ValueError(f"duplicate zone id {zid!r} in {path}")
        bounds_raw = raw_zone.get("bounds", {}) or {}
        center = bounds_raw.get("center", [0, 0, 0])
        extent = bounds_raw.get("extent", [0, 0, 0])
        # AABB: (x_min, x_max, y_min, y_max) — Z ignored for flat worlds.
        bounds = (
            center[0] - extent[0], center[0] + extent[0],
            center[1] - extent[1], center[1] + extent[1],
        )
        kb.zones[zid] = ZoneInfo(
            id=zid,
            display_name=raw_zone.get("display_name", zid),
            entry_point=list(raw_zone.get("entry_point", [0, 0, 0])),
            bounds=bounds,
        )

    for raw_obj in data.get("objects", []) or []:
        oid = raw_obj.get("id", "")
        if not oid:
            raise ValueError(f"object missing id in {path}: {raw_obj}")
        if oid in kb.objects:
            raise ValueError(f"duplicate object id {oid!r} in {path}")
        radius = float(raw_obj.get("interaction_radius", 0))
        if radius == 0:
            radius = 1500.0  # default 1500cm, matches Go side defaultInteractionRadius
        kb.objects[oid] = ObjectInfo(
            id=oid,
            display_name=raw_obj.get("display_name", oid),
            description=raw_obj.get("description", ""),
            category=raw_obj.get("category", ""),
            zone_id=raw_obj.get("zone_id", ""),
            actor_class=raw_obj.get("actor_class", ""),
            actor_position=list(raw_obj.get("actor_position", [0, 0, 0])),
            interaction_point=list(raw_obj.get("interaction_point", [0, 0, 0])),
            interaction_facing=list(raw_obj.get("interaction_facing", [0, 0, 0])),
            interaction_radius=radius,
            available_interactions=list(raw_obj.get("available_interactions", []) or []),
            default_state=raw_obj.get("default_state", ""),
            tags=list(raw_obj.get("tags", []) or []),
        )

    for raw_agent in data.get("agents", []) or []:
        aid = raw_agent.get("id", "")
        if not aid:
            raise ValueError(f"agent missing id in {path}: {raw_agent}")
        kb.agents.append(AgentInfo(
            id=aid,
            display_name=raw_agent.get("display_name", aid),
            description=raw_agent.get("description", ""),
            type=raw_agent.get("type", ""),
            profession=raw_agent.get("profession", ""),
            personality=raw_agent.get("personality", {}) or {},
            initial_zone=raw_agent.get("initial_zone", ""),
            initial_position=list(raw_agent.get("initial_position", [0, 0, 0])),
            actor_class=raw_agent.get("actor_class", ""),
            action_table=raw_agent.get("action_table", ""),
            main_behavior_tree=raw_agent.get("main_behavior_tree", ""),
        ))

    kb.narrative = data.get("narrative", {}) or {}
    kb.version = data.get("version", "")

    return kb, data

# Composite action nominal durations (seconds) when not given explicitly
COMPOSITE_DEFAULT_SEC = 1800.0  # 30 min

# Physical evolution rates per game-minute, keyed by activity class.
# `interval` in _evolve_physical is in game-minutes (perception_interval=30),
# so multiply these by `interval` to get the per-tick delta.
# ChargeAtStation actively restores energy and relieves fatigue (no joint
# wear — the robot is docked, not moving). The default composite rate
# (WorkAtWorkbench / WorkAtWorkshop / PatrolZone / ChatWith /
# RepairTarget) keeps the work-like drain. Keyed by cmd constant so the
# removal of busy_composite_name (post-14-cmd migration) still lets
# _evolve_physical pick the right rate from busy_cmd alone.
# Fatigue rates tuned for single-day sim (06:00-22:00, ~16 game-hours):
# at default rate +0.10/min × ~9 work-hours ≈ 54 fatigue by 17:00, leaving
# headroom for the raised alert threshold (80) to trigger mid-afternoon
# rather than at 11:00 as the old +0.20/min rate did.
PHYS_RATES = {
    CMD_CHARGE_AT_STATION: {"energy": +0.10, "fatigue": -0.15, "joint_wear": 0.0},
    "_default":            {"energy": -0.05, "fatigue": +0.10, "joint_wear": +0.05},
}
# Passive (non-busy) drain — applied when no composite action is running.
PHYS_RATES_PASSIVE = {"energy": -0.02, "fatigue": +0.03, "joint_wear": 0.0}

# Composite cmds — the 6 long-running composite actions. Used by
# _estimate_duration (all busy) and _evolve_physical (PHYS_RATES lookup).
# Atomic busy cmds (e.g. Wait) use PASSIVE rate, not the work-like default.
COMPOSITE_CMDS = frozenset({
    CMD_WORK_AT_WORKBENCH, CMD_WORK_AT_WORKSHOP, CMD_CHAT_WITH,
    CMD_REPAIR_TARGET, CMD_CHARGE_AT_STATION, CMD_PATROL_ZONE,
})

# Physical-state delta thresholds (约定5)
DELTA_THRESHOLD = {"energy": 5.0, "fatigue": 5.0, "health": 5.0, "joint_wear": 1.0}

# state_report fallback interval (seconds, real)
STATE_REPORT_INTERVAL_SEC = 15.0

# heartbeat interval (seconds, real)
HEARTBEAT_INTERVAL_SEC = 5.0

# Reconnect backoff: 3s interval, exponential to 30s cap (§5.2).
RECONNECT_BASE_SEC = 3.0
RECONNECT_MAX_SEC = 30.0

# Send buffer retention for reconnect replay (约定11).
SEND_BUFFER_MAX_LEN = 200
SEND_BUFFER_MAX_AGE_SEC = 60.0

# Discrete outbound message types eligible for seq-replay after reconnect.
# Continuous state (perception_update/state_report) and heartbeat are NOT
# replayed — the peer uses the latest snapshot (约定11).
DISCRETE_REPLAY_TYPES = {
    TYPE_ACTION_STARTED,
    TYPE_ACTION_COMPLETED,
    TYPE_ERROR,
}


@dataclass
class PhysicalState:
    """UE-owned physical state (四项)."""
    energy: float = 100.0
    fatigue: float = 0.0
    joint_wear: float = 0.0
    health: float = 100.0

    def as_dict(self) -> Dict[str, float]:
        return {
            "energy": round(self.energy, 1),
            "fatigue": round(self.fatigue, 1),
            "joint_wear": round(self.joint_wear, 1),
            "health": round(self.health, 1),
        }


@dataclass
class NPCState:
    """UE-owned spatial + physical state for one NPC."""
    agent_id: str = "H-01"
    name: str = "老陈"
    agent_type: str = "humanoid"
    ue5_ref: str = "BP_HumanoidRobot_H01"
    position: List[float] = field(default_factory=lambda: [20000.0, 10000.0, 0.0])  # cm
    rotation: List[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])  # [Pitch,Yaw,Roll]
    current_zone: str = "main_workshop"
    current_location: Optional[str] = None
    physical: PhysicalState = field(default_factory=PhysicalState)
    current_animation: str = "idle"
    current_emote: Optional[str] = None
    # Busy state for long-running actions
    busy_action_id: Optional[str] = None
    busy_cmd: Optional[str] = None
    # busy_cmd alone distinguishes all composite actions after the 14-cmd
    # migration (ChargeAtStation vs WorkAtWorkbench vs ...). The old
    # busy_composite_name field (needed when all composites shared
    # CmdExecuteComposite) has been removed.
    busy_until_min: Optional[int] = None  # absolute game-minute
    busy_started_ms: Optional[int] = None


@dataclass
class GameTime:
    """Simulated game time with acceleration."""
    speed: float = 60.0
    day: int = 1
    hour: int = 6
    minute: int = 0

    @property
    def total_minutes(self) -> int:
        return self.hour * 60 + self.minute

    @property
    def display(self) -> str:
        return f"Day{self.day} {self.hour:02d}:{self.minute:02d}"

    @property
    def time_of_day(self) -> str:
        return f"{self.hour:02d}:{self.minute:02d}"

    # ── 约定 19：游戏时间权威字段（UE DS 推送给 Agent） ─────────────
    @property
    def time_of_day_sec(self) -> float:
        """当天秒数 0-86400。"""
        return self.hour * 3600.0 + self.minute * 60.0

    @property
    def day_count(self) -> int:
        """第几天（从 0 开始；Mock UE 内部 day 从 1 开始，对外暴露 day-1）。"""
        return self.day - 1

    @property
    def game_time_sec(self) -> float:
        """权威游戏时间（累计秒）= day_count*86400 + time_of_day_sec。"""
        return self.day_count * 86400.0 + self.time_of_day_sec

    @property
    def time_scale(self) -> float:
        """时间倍速（游戏秒/现实秒）。"""
        return self.speed

    def advance(self, game_minutes: float):
        total = int(self.total_minutes + game_minutes)
        self.hour = (total // 60) % 24
        self.minute = total % 60
        if self.hour == 0 and self.minute == 0 and total > 0:
            self.day += 1


class MockUE:
    """Async Mock UE client speaking the AgentTown WebSocket protocol."""

    def __init__(
        self,
        mcp_ws_url: str = "ws://localhost:9091/ws",
        mode: str = "normal",
        time_speed: float = 300.0,
        perception_interval: int = 30,   # game-minutes between perception pushes
        scenario_file: Optional[str] = None,
        log_dir: str = "logs",
        world_kb_path: str = "assets/world_kb.yaml",
        capability_actions: Optional[List[Dict[str, Any]]] = None,
    ):
        self.mcp_ws_url = mcp_ws_url
        self.mode = mode
        self.perception_interval = perception_interval
        self.log_dir = log_dir

        # Capability registry — the cmd set this Mock UE advertises to MCP
        # on connect. Defaults to the module-level 9-cmd constant; tests or
        # alternate worlds can pass a reduced set (e.g. drop PlayAnimation
        # for an NPC that doesn't support it) to drive MCP-side tool
        # reconciliation without touching the constant.
        self._capability_actions = (
            capability_actions if capability_actions is not None
            else DEFAULT_CAPABILITY_ACTIONS
        )

        # World KB — single source of truth for zone/location/object data.
        # Fail-fast: Mock UE cannot resolve semantic targets without it.
        # `kb` is the typed view (queries, NPC init); `kb_yaml` is the raw
        # dict kept for _send_world_kb, which splits it back into
        # generated/authored halves for the MCP merge pipeline.
        try:
            self.kb, self.kb_yaml = load_world_kb(world_kb_path)
        except Exception as e:
            raise SystemExit(f"[FATAL] failed to load world_kb from {world_kb_path!r}: {e}")
        logger.info(f"[KB] loaded {len(self.kb.zones)} zones, "
                    f"{len(self.kb.objects)} objects, {len(self.kb.agents)} agents "
                    f"from {world_kb_path}")

        # Seed NPC from the first agent entry in the KB. First phase is
        # single-agent; agents[0] is the active NPC. ue5_ref has no yaml
        # counterpart (actor_class is the UE path, not the blueprint short
        # name), so it keeps the NPCState default.
        if not self.kb.agents:
            raise SystemExit(f"[FATAL] no agents in world_kb {world_kb_path!r}")
        agent0 = self.kb.agents[0]
        self.npc = NPCState(
            agent_id=agent0.id,
            name=agent0.display_name or agent0.id,
            agent_type=agent0.type or "humanoid",
            current_zone=agent0.initial_zone,
            position=list(agent0.initial_position),
        )
        self.time = GameTime(speed=time_speed)

        self._ws = None
        self._seq = 0
        self._started_ms = int(_time.time() * 1000)

        # Reconnect / replay state (约定11).
        self._last_received_seq = 0
        self._send_buffer: List[Dict[str, Any]] = []  # {seq, frame, at}
        self._connected = False
        self._stop = False
        self._ws_ready = asyncio.Event()  # signals first WS connection established

        # Physical values last reported via state_report (for delta calc)
        self._last_reported = PhysicalState()

        # Scenario injection. Crossed events wait here until exactly one
        # perception consumes them; indices prevent reinjection.
        self.scenarios: List[Dict[str, Any]] = []
        self._injected_scenario_events: Set[int] = set()
        self._pending_audible_events: List[Dict[str, Any]] = []
        if scenario_file:
            self._load_scenarios(scenario_file)

        self.action_log: List[Dict] = []

        os.makedirs(log_dir, exist_ok=True)
        self._setup_logging()

    # ─── Envelope send helpers ────────────────────────────────

    def _next_seq(self) -> int:
        self._seq += 1
        return self._seq

    async def _send(self, msg_type: str, agent_id: str, payload: Dict[str, Any]):
        """Wrap payload in the 7-field envelope and send.

        Discrete message types are retained in the send buffer for reconnect
        replay (约定11).
        """
        if self._ws is None:
            logger.warning("send with no WS connection")
            return
        seq = self._next_seq()
        env = {
            "version": PROTOCOL_VERSION,
            "msg_id": str(uuid.uuid4()),
            "seq": seq,
            "timestamp": int(_time.time() * 1000),
            "type": msg_type,
            "agent_id": agent_id,
            "payload": payload,
        }
        frame = json.dumps(env, ensure_ascii=False)
        if msg_type in DISCRETE_REPLAY_TYPES:
            self._buffer_outbound(seq, frame)
        await self._ws.send(frame)
        # MCP 侧 wsserver/server.go:229 已记录 [UE→MCP] 入站日志，
        # Mock UE 不再重复记录出站消息（避免 ~90% 日志重复）。
        # 控制台仍可通过 [PERCEPTION]/[STATE] 摘要观察仿真状态。

    def _buffer_outbound(self, seq: int, frame: str):
        """Append a discrete message to the rolling send buffer and evict old."""
        self._send_buffer.append({"seq": seq, "frame": frame, "at": _time.monotonic()})
        self._evict_buffer()

    def _evict_buffer(self):
        """Trim send buffer to the retention window (length + age)."""
        cutoff = _time.monotonic() - SEND_BUFFER_MAX_AGE_SEC
        i = 0
        while i < len(self._send_buffer) and self._send_buffer[i]["at"] < cutoff:
            i += 1
        overflow = len(self._send_buffer) - i - SEND_BUFFER_MAX_LEN
        if overflow > 0:
            i += overflow
        if i > 0:
            self._send_buffer = self._send_buffer[i:]

    async def _replay_from(self, peer_last_seq: int):
        """Re-send buffered discrete messages with seq > peer_last_seq.

        If the oldest buffered seq is already beyond peer_last_seq+1, some
        messages were lost to rollover — emit an event_lost warning (约定11).
        """
        self._evict_buffer()
        to_replay = [m for m in self._send_buffer if m["seq"] > peer_last_seq]
        oldest_seq = self._send_buffer[0]["seq"] if self._send_buffer else None

        if oldest_seq is not None and oldest_seq > peer_last_seq + 1:
            lost = oldest_seq - (peer_last_seq + 1)
            logger.warning(
                f"[EVENT_LOST] send buffer rolled past resume point: "
                f"peer_last_seq={peer_last_seq} oldest={oldest_seq} lost={lost}")
            await self._send(TYPE_EVENT_LOST, SYSTEM_AGENT_ID, {
                "from_seq": peer_last_seq + 1,
                "to_seq": oldest_seq,
                "count": lost,
                "reason": "send buffer rollover",
            })

        if not to_replay:
            return
        logger.info(f"[REPLAY] resending {len(to_replay)} discrete messages "
                    f"after reconnect (peer_last_seq={peer_last_seq})")
        for m in to_replay:
            if self._ws is None:
                return
            await self._ws.send(m["frame"])

    # ─── Lifecycle messages ───────────────────────────────────

    async def _send_agent_registered(self):
        await self._send(TYPE_AGENT_REGISTERED, self.npc.agent_id, {
            "agent_type": self.npc.agent_type,
            "ue5_ref": self.npc.ue5_ref,
            "initial_position": list(self.npc.position),
            "initial_zone": self.npc.current_zone,
        })

    async def _send_capability_registry(self):
        """Declare the cmds Mock UE actually implements.

        Sent on connect (and reconnect) right after agent_registered,
        with agent_id="system" so it overwrites MCP's built-in default
        and becomes the authoritative global capability set. MCP uses
        this to drive tactical-layer prompt generation and dynamic
        MCP tool registration (AddTool/RemoveTools).

        The action list comes from self._capability_actions, which
        defaults to DEFAULT_CAPABILITY_ACTIONS (9 cmd constant) and can
        be overridden via the `capability_actions=` constructor param.
        """
        await self._send(TYPE_CAPABILITY_REGISTRY, SYSTEM_AGENT_ID, {
            "actions": self._capability_actions,
        })

    async def _send_world_kb(self):
        """Push the full world KB (generated + authored) to MCP on connect.

        Sent FIRST in the connection sequence (before agent_registered)
        so MCP can merge + persist + swap its in-memory KB before any
        agent starts running. MCP only accepts this before the first
        agent_registered (startup window); later pushes are rejected.

        The payload is built dynamically from self.kb_yaml (the raw dict
        loaded by load_world_kb) via split_yaml_to_generated_authored,
        so swapping assets/world_kb.yaml is enough to drive a new world
        — no hardcoded JSON to keep in sync.
        """
        generated, authored = split_yaml_to_generated_authored(self.kb_yaml)
        await self._send(TYPE_WORLD_KB, SYSTEM_AGENT_ID, {
            "pushed_at": _time.strftime("%Y-%m-%dT%H:%M:%SZ", _time.gmtime()),
            "generated": generated,
            "authored": authored,
        })

    async def _send_agent_unregistered(self, reason: str):
        await self._send(TYPE_AGENT_UNREGISTERED, self.npc.agent_id, {"reason": reason})

    async def _send_heartbeat(self):
        uptime = (int(_time.time() * 1000) - self._started_ms) // 1000
        # 心跳发送打 debug 日志：与 MCP 端 heartbeat received 配对，
        # 双向都能看到心跳链路状态。INFO 级别会被 perception_update 刷屏，
        # 所以放 debug；联调时设 LOG_LEVEL=DEBUG 即可见。
        logger.debug(f"[HEARTBEAT] send uptime_sec={uptime}s")
        await self._send(TYPE_HEARTBEAT, SYSTEM_AGENT_ID, {"uptime_sec": uptime})

    # ─── perception_update ────────────────────────────────────

    async def _send_perception(self, scan_id: Optional[str] = None):
        payload = self._build_perception()
        if scan_id:
            payload["scan_id"] = scan_id
        await self._send(TYPE_PERCEPTION_UPDATE, self.npc.agent_id, payload)
        logger.info(f"[PERCEPTION] {self.time.display} | zone={self.npc.current_zone} | energy={self.npc.physical.energy:.0f}%")

    def _build_perception(self) -> Dict[str, Any]:
        """Build a perception_update payload per §2.3."""
        # Physical delta: only include values that changed over threshold.
        delta = {}
        for key, thr in DELTA_THRESHOLD.items():
            cur = getattr(self.npc.physical, key)
            last = getattr(self._last_reported, key)
            if abs(cur - last) >= thr:
                delta[key] = round(cur, 1)

        audible_events = self._pending_audible_events
        self._pending_audible_events = []

        return {
            "location": {
                "position": list(self.npc.position),
                "rotation": list(self.npc.rotation),
                "current_zone": self.npc.current_zone,
                "current_location": self.npc.current_location,
            },
            "physical_state_delta": delta if delta else None,
            "visible_agents": [],   # Phase 1: single NPC
            "nearby_objects": self._nearby_objects(),
            "audible_events": audible_events,
            "current_animation": self.npc.current_animation,
            "current_emote": self.npc.current_emote,
            "environment": {
                "game_time_sec": self.time.game_time_sec,
                "time_of_day_sec": self.time.time_of_day_sec,
                "day_count": self.time.day_count,
                "time_scale": self.time.time_scale,
                "weather": "clear",
            },
        }

    def _nearby_objects(self) -> List[Dict[str, Any]]:
        objs = []
        for oid in self.kb.objects_in_zone(self.npc.current_zone):
            obj = self.kb.objects.get(oid)
            if obj is None:
                continue
            name = obj.display_name or oid
            # Euclidean distance on the XY plane (UE5 cm). Z is ignored —
            # multi-floor navigation is out of scope for phase 1.
            dx = self.npc.position[0] - obj.actor_position[0]
            dy = self.npc.position[1] - obj.actor_position[1]
            distance = math.sqrt(dx * dx + dy * dy)
            objs.append({
                "id": oid,
                "name": name,
                "distance": round(distance, 1),
                "state": obj.default_state or "idle",
                "available_interactions": list(obj.available_interactions),
            })
        return objs

    # ─── state_report (authoritative physical channel) ────────

    async def _send_state_report(self):
        payload: Dict[str, Any] = {"physical_state": self.npc.physical.as_dict()}
        if self.npc.busy_action_id is not None:
            payload["current_task_progress"] = {
                "action_id": self.npc.busy_action_id,
                "progress": self._busy_progress(),
            }
        await self._send(TYPE_STATE_REPORT, self.npc.agent_id, payload)
        # Snapshot the reported values for delta calc.
        self._last_reported = PhysicalState(**self.npc.physical.as_dict())
        logger.info(f"[STATE] energy={self.npc.physical.energy:.0f} fatigue={self.npc.physical.fatigue:.0f} "
                    f"wear={self.npc.physical.joint_wear:.0f} health={self.npc.physical.health:.0f}")

    async def _send_event_notification(self, event: Dict[str, Any]):
        """Send an event_notification envelope immediately (P1 实时通道).

        与 _pending_audible_events 互补：事件到达时既走独立通道即时送达反应层，
        又折入下一个 perception_update 的 audible_events 供战术层参考。
        EventNotificationPayload 结构与 protocol/messages.go 对齐：
        {event_id, event: {...}, perception_level}。
        """
        import uuid
        payload = {
            "event_id": "evt_" + uuid.uuid4().hex[:12],
            "event": event,
            "perception_level": "direct",
        }
        await self._send(TYPE_EVENT_NOTIFICATION, self.npc.agent_id, payload)
        logger.info(f"[EVENT_NOTIFY] {payload['event_id']} type={event.get('type','?')} content={str(event.get('content',''))[:80]}")

    def _physical_changed_over_threshold(self) -> bool:
        for key, thr in DELTA_THRESHOLD.items():
            if abs(getattr(self.npc.physical, key) - getattr(self._last_reported, key)) >= thr:
                return True
        return False

    def _busy_progress(self) -> float:
        if self.npc.busy_until_min is None or self.npc.busy_started_ms is None:
            return 0.0
        # Progress by game-time elapsed toward completion.
        # Approximate: 1 - remaining/total is hard without total; use time.
        remaining = max(0, self.npc.busy_until_min - self.time.total_minutes)
        return 0.0 if remaining > 0 else 1.0

    # ─── Inbound message handling ─────────────────────────────

    async def _handle_envelope(self, env: Dict[str, Any]):
        msg_type = env.get("type", "")
        payload = env.get("payload", {}) or {}
        seq = env.get("seq", 0)
        target_agent = env.get("agent_id", "")

        agent_commands = {TYPE_ACTION_COMMAND, TYPE_STOP_ACTION, TYPE_SCAN_AREA, TYPE_NARRATIVE}
        if msg_type in agent_commands and target_agent != self.npc.agent_id:
            logger.warning(f"[MCP→UE] rejected {msg_type} for unknown agent={target_agent}")
            await self._send_error(
                ERR_UNKNOWN_AGENT,
                "message agent_id does not match this actor",
                context={"requested": target_agent, "current": self.npc.agent_id},
            )
            return

        # Track highest inbound seq for reconnect replay (约定11); resync/
        # event_lost are control messages and don't advance it.
        if msg_type not in (TYPE_RESYNC, TYPE_EVENT_LOST):
            if isinstance(seq, int) and seq > self._last_received_seq:
                self._last_received_seq = seq

        # MCP 侧 wsserver/server.go:358 已记录 [MCP→UE] 出站日志，
        # Mock UE 不再重复记录入站消息。仅 narrative 保留一条单行
        # 预览（换行转义）供控制台实时观察 LLM 输出。
        if msg_type == TYPE_NARRATIVE:
            text = payload.get("text", "").replace("\n", "\\n")
            logger.info(f"[MCP→UE/NARRATIVE] seq={seq} text={text[:100]}")

        if msg_type == TYPE_ACTION_COMMAND:
            await self._handle_action_command(payload)
        elif msg_type == TYPE_STOP_ACTION:
            await self._handle_stop_action(payload)
        elif msg_type == TYPE_SCAN_AREA:
            await self._send_perception(payload.get("scan_id"))
        elif msg_type == TYPE_RESYNC:
            peer_last = int(payload.get("last_received_seq", 0))
            await self._replay_from(peer_last)
        elif msg_type == TYPE_EVENT_LOST:
            logger.warning(f"[EVENT_LOST] peer reported: {payload}")
        elif msg_type == TYPE_NARRATIVE:
            text = payload.get("text", "")
            if text:
                print(f"\n  [{self.time.display}] {text[:500]}")
        else:
            logger.debug(f"[WS←] unhandled type: {msg_type}")

    async def _handle_action_command(self, payload: Dict[str, Any]):
        action_id = payload.get("action_id", "")
        cmd = payload.get("cmd", "")
        params = payload.get("params", {}) or {}

        # MCP 侧 wsserver/server.go:485 已记录 [MCP→UE/CMD]，不再重复。
        # 仅在拒绝时记录原因（validate 失败的场景）。

        # Validate that the command references known world entities.
        invalid_reason = self._validate_target(cmd, params)
        if invalid_reason:
            await self._send_action_started(
                action_id, accepted=False, estimated_sec=None,
                reject_reason=invalid_reason,
            )
            await self._send_error(ERR_ACTION_FAILED, invalid_reason,
                                   action_id=action_id, context={"cmd": cmd, "params": params})
            return

        # Busy guard: reject disruptive commands while busy.
        DISRUPTIVE = {
            CMD_MOVE_TO_LOCATION, CMD_MOVE_TO_AGENT, CMD_TURN_TO, CMD_INTERACT, CMD_WAIT,
            CMD_WORK_AT_WORKBENCH, CMD_WORK_AT_WORKSHOP, CMD_CHAT_WITH,
            CMD_REPAIR_TARGET, CMD_CHARGE_AT_STATION, CMD_PATROL_ZONE,
        }
        if self.npc.busy_action_id is not None and cmd in DISRUPTIVE:
            remaining = max(0, (self.npc.busy_until_min or 0) - self.time.total_minutes)
            await self._send_action_started(
                action_id, accepted=False, estimated_sec=None,
                reject_reason=f"busy with {self.npc.busy_cmd} ({remaining} game-min remaining)",
            )
            return

        # Determine estimated duration and whether it's a long (busy) action.
        est_sec, is_busy = self._estimate_duration(cmd, params)

        # ACK within 2s (约定8).
        await self._send_action_started(action_id, accepted=True, estimated_sec=est_sec)

        if is_busy:
            # Set busy state; completion happens when game time advances past
            # busy_until_min (checked in the perception loop).
            game_min = est_sec / 60.0 * self.time.speed / 60.0  # est_sec is real→game
            # Simpler: treat est_sec as game-seconds for the sim; convert to game-min.
            busy_game_min = max(1, int(est_sec / 60.0))
            self.npc.busy_action_id = action_id
            self.npc.busy_cmd = cmd
            # After the 14-cmd migration, busy_cmd alone distinguishes all
            # composite actions (ChargeAtStation / WorkAtWorkbench / ...),
            # so the old busy_composite_name field is no longer needed.
            self.npc.busy_until_min = self.time.total_minutes + busy_game_min
            self.npc.busy_started_ms = int(_time.time() * 1000)
            self.npc.current_animation = "work"
            self._apply_command_effects(cmd, params, starting=True)
        else:
            # Short action: apply effects and complete after a small delay.
            # Real UE has frame-level execution latency even for instant actions;
            # completing in the same tick as the ACK exposed a race in MCP's
            # armActionTimeout (completion arrived before the timer was armed,
            # but the already-started timer was not stopped, leading to
            # spurious stop_action after 5s). A 100ms delay mirrors real UE
            # timing and lets the armActionTimeout race path settle.
            details = self._apply_command_effects(cmd, params, starting=False)
            await asyncio.sleep(0.1)
            await self._send_action_completed(action_id, RESULT_SUCCESS, 0, 1.0, details=details)

    def _validate_target(self, cmd: str, params: Dict[str, Any]) -> str:
        """Return a non-empty rejection reason when targeting a non‑existent
        zone, location, object, or route.  An empty return means valid."""
        if cmd == CMD_MOVE_TO_LOCATION:
            # MoveToLocation: MCP 层已解析坐标，UE 只校验 dest 是 3 元数组。
            dest = params.get("dest")
            if not isinstance(dest, list) or len(dest) != 3:
                return f"MoveToLocation requires dest:[x,y,z], got: {dest!r}"
            for v in dest:
                if not isinstance(v, (int, float)):
                    return f"MoveToLocation dest entries must be numeric, got: {dest!r}"
        elif cmd == CMD_MOVE_TO_AGENT:
            # MoveToAgent: target_agent_id 必填，UE 端不校验 agent 存在性
            # （多 agent 场景由 MCP 注册表管理，UE 仅按 ID 寻路）
            tid = params.get("target_agent_id", "")
            if not tid:
                return "MoveToAgent requires target_agent_id"
        elif cmd == CMD_INTERACT:
            obj = params.get("target_object_id", "")
            if obj and obj not in self.kb.objects:
                return f"unknown object: {obj} (available: {list(self.kb.objects)})"
        elif cmd == CMD_WORK_AT_WORKBENCH:
            target = params.get("target_object_id", "")
            if target and target not in self.kb.objects:
                return f"unknown workbench: {target}"
        elif cmd == CMD_CHARGE_AT_STATION:
            station = params.get("target_object_id", "")
            if station and not self.kb.is_target(station):
                return f"unknown charging station: {station}"
        elif cmd == CMD_PATROL_ZONE:
            zone = params.get("target_zone", "")
            if zone and zone not in self.kb.zones:
                return f"unknown patrol zone: {zone}"
        return ""

    async def _handle_stop_action(self, payload: Dict[str, Any]):
        req_id = payload.get("action_id", "")
        cur = self.npc.busy_action_id
        logger.info(f"[MCP→UE/STOP] action_id={req_id} current_busy={cur}")
        if cur is not None and cur == req_id:
            # Match → interrupt.
            progress = self._busy_progress()
            self._clear_busy()
            await self._send_action_completed(req_id, RESULT_INTERRUPTED, 0, progress,
                                              details={"reason": "stop_action received"})
        else:
            # Mismatch → ignore + error (约定9).
            await self._send_error(ERR_STOP_ID_MISMATCH,
                                   "stop_action id does not match current action",
                                   action_id=req_id,
                                   context={"requested": req_id, "current": cur})

    # ─── Action effects ───────────────────────────────────────

    def _estimate_duration(self, cmd: str, params: Dict[str, Any]) -> Tuple[float, bool]:
        """Return (estimated_duration_sec, is_long_running)."""
        # Composite cmds (6): all long-running, duration from params or default.
        if cmd in COMPOSITE_CMDS:
            dur = float(params.get("duration_sec", COMPOSITE_DEFAULT_SEC))
            return dur, True
        if cmd == CMD_WAIT:
            # Wait 是长动作：占用 NPC 直到游戏时间推进过 busy_until_min，
            # 避免被当作短动作立即完成导致 MCP 侧忙循环（sendIdleWait 路径）。
            return float(params.get("duration_sec", 5)), True
        if cmd == CMD_MOVE_TO_LOCATION:
            return 120.0, False   # ~2 min walk
        if cmd == CMD_MOVE_TO_AGENT:
            return 120.0, False   # ~2 min walk
        if cmd == CMD_INTERACT:
            return 300.0, False   # ~5 min
        if cmd == CMD_PLAY_MONTAGE:
            return 10.0, False
        # TurnTo/Speak/Emote: near-instant
        return 1.0, False

    def _apply_command_effects(self, cmd: str, params: Dict[str, Any], starting: bool) -> Optional[Dict[str, Any]]:
        """Mutate NPC state for a command. Does NOT advance game time.

        Returns an optional ``details`` dict for short-running actions — the
        caller passes it to ``_send_action_completed`` so the result reaches
        the agent. Currently only ``interact`` with ``interaction=inspect``
        returns meaningful content; other commands return None (empty details).
        """
        if cmd == CMD_MOVE_TO_LOCATION:
            # MCP 已解析坐标，UE 直接用 dest。
            dest = params.get("dest")
            if isinstance(dest, list) and len(dest) == 3:
                self._move_to(list(dest))
        elif cmd == CMD_MOVE_TO_AGENT:
            # MoveToAgent: 无多 agent 模拟，记录意图即可。
            # 真实 UE 会寻路到 target_agent_id 当前位置；Mock UE 单 agent
            # 场景下退化为原地等待目标出现。
            tid = params.get("target_agent_id", "")
            if tid:
                logger.info(f"[MOVE_TO_AGENT] target={tid} (mock: no multi-agent sim)")
        elif cmd == CMD_TURN_TO:
            self.npc.rotation[1] = (self.npc.rotation[1] + 90.0) % 360.0
        elif cmd == CMD_PLAY_MONTAGE:
            montage = params.get("montage_id", "")
            if montage:
                logger.info(f"[MONTAGE] {montage}")
        elif cmd == CMD_SPEAK:
            content = params.get("content", "")
            logger.info(f"[SPEAK] {content[:60]}")
        elif cmd == CMD_EMOTE:
            emotion = params.get("emotion", "")
            mode = params.get("mode", "oneshot")
            if mode == "sustained":
                self.npc.current_emote = emotion
            else:
                self.npc.current_emote = None
        elif cmd in (CMD_WORK_AT_WORKBENCH, CMD_WORK_AT_WORKSHOP,
                     CMD_CHAT_WITH, CMD_REPAIR_TARGET, CMD_PATROL_ZONE):
            # 工作类复合行为：疲劳在 _evolve_physical 循环中累积，这里不产生 details。
            pass
        elif cmd == CMD_CHARGE_AT_STATION:
            # 充电：能量在 _evolve_physical 循环中恢复（PHYS_RATES 已按 cmd 区分）。
            pass
        elif cmd == CMD_INTERACT:
            # inspect 是只读查询：根据 target_object_id 生成设备检查报告，通过
            # action_completed.details 返回，流经 MCP "动作完成" 行进入下一轮
            # 决策上下文。其他 interact 动作（assemble/charge 等）由对应
            # 复合动作处理，这里不产生额外 details。
            interaction = params.get("interaction", "")
            target_object_id = params.get("target_object_id", "")
            if interaction == "inspect" and target_object_id:
                return self._inspect_object(target_object_id)
        # Physical drain applied gradually in perception loop.
        return None

    def _inspect_object(self, object_id: str) -> Dict[str, Any]:
        """Generate a readable inspection report for a smart object.

        Returns a ``details`` dict with an ``inspection`` text field describing
        the object's current state, recent activity, and any anomalies. The
        text is what the NPC sees in the next decision context under
        "动作完成 ... details={...}". Kept intentionally short (one paragraph)
        so it stays inside the tool-result injection without bloating context.

        Text is assembled from the KB's ``description`` + ``category`` fields
        rather than hardcoded object IDs, so any object in any test world_kb
        yields a sensible report. The ``charging_station`` category gets one
        extra line with the NPC's current battery reading, since that's the
        only category-specific signal Mock UE can produce.
        """
        obj = self.kb.objects.get(object_id)
        name = (obj.display_name if obj else "") or object_id
        actions = (obj.available_interactions if obj else [])
        description = (obj.description if obj else "") or "外观正常，未见明显异常。"
        busy = self.npc.busy_action_id is not None

        lines: List[str] = []
        # Lead with the authored description (already a full sentence).
        if not description.endswith(("。", ".", "！", "!", "？", "?")):
            description = description + "。"
        lines.append(f"{name}：{description}")
        # Category-specific extra signal. charging_station is the only
        # category where Mock UE has live telemetry (battery reading);
        # other categories just report busy/idle state.
        if obj and obj.category == "charging_station":
            p = self.npc.physical
            lines.append(
                f"当前状态：{'充电中' if busy else '空闲'}，"
                f"可执行 {('/'.join(actions)) if actions else '无'}，"
                f"本机电池读数 {p.energy:.0f}%，建议电量低于 30% 时及时补能。"
            )
        else:
            lines.append(
                f"当前状态：{'作业中' if busy else '待机'}，"
                f"可执行 {('/'.join(actions)) if actions else '无'}。"
            )
        text = "".join(lines)
        logger.info(f"[INSPECT] {object_id} -> {text[:60]}...")
        return {"inspection": text, "object_id": object_id}

    def _move_to(self, dest: List[float], target: Optional[str] = None, kind: Optional[str] = None) -> bool:
        """Move NPC to dest coordinate.

        方案 A: dest 是权威坐标（MCP 层解析自 world_kb.yaml）。
        target+kind 是 metadata：
          - kind=="object" 且 target 非空 → 直接设置 current_location = target
          - 否则用 which_object(pos) 基于坐标距离反推
        """
        if not dest or len(dest) != 3:
            logger.warning(f"[MOVE] invalid dest {dest!r}")
            return False
        old_pos = self.npc.position
        # Face the movement direction (yaw).
        dx, dy = dest[0] - old_pos[0], dest[1] - old_pos[1]
        if dx or dy:
            self.npc.rotation[1] = (math.degrees(math.atan2(dy, dx))) % 360.0
        self.npc.position = list(dest)

        # Resolve current_location: prefer explicit metadata, else reverse-lookup.
        if kind == "object" and target:
            self.npc.current_location = target
        else:
            self.npc.current_location = self.kb.which_object(self.npc.position)

        # Resolve current_zone via AABB reverse-lookup.
        new_zone = self.kb.which_zone(self.npc.position)
        if new_zone and new_zone != self.npc.current_zone:
            logger.info(f"[ZONE] {self.npc.current_zone} → {new_zone}")
            self.npc.current_zone = new_zone
        return True

    def _resolve_zone(self, pos: List[float]) -> str:
        """Deprecated wrapper kept for backward compat — delegates to KB."""
        return self.kb.which_zone(pos) or self.npc.current_zone

    def _clear_busy(self):
        self.npc.busy_action_id = None
        self.npc.busy_cmd = None
        self.npc.busy_until_min = None
        self.npc.busy_started_ms = None
        self.npc.current_animation = "idle"

    # ─── ACK / completed / error senders ──────────────────────

    async def _send_action_started(self, action_id: str, accepted: bool,
                                   estimated_sec: Optional[float], reject_reason: str = ""):
        payload: Dict[str, Any] = {
            "action_id": action_id,
            "accepted": accepted,
            "estimated_duration_sec": estimated_sec,
        }
        if reject_reason:
            payload["reject_reason"] = reject_reason
        await self._send(TYPE_ACTION_STARTED, self.npc.agent_id, payload)

    async def _send_action_completed(self, action_id: str, result: str,
                                    duration_ms: int, progress: float,
                                    details: Optional[Dict] = None):
        await self._send(TYPE_ACTION_COMPLETED, self.npc.agent_id, {
            "action_id": action_id,
            "result": result,
            "duration_ms": duration_ms,
            "progress": progress,
            "details": details or {},
        })
        self.action_log.append({"time": self.time.display, "action_id": action_id, "result": result})

    async def _send_error(self, code: str, message: str, action_id: str = "",
                        context: Optional[Dict] = None):
        payload: Dict[str, Any] = {"error_code": code, "message": message}
        if action_id:
            payload["action_id"] = action_id
        if context:
            payload["context"] = context
        await self._send(TYPE_ERROR, self.npc.agent_id, payload)

    # ─── Scenario injection ───────────────────────────────────

    def _load_scenarios(self, filepath: str):
        with open(filepath, "r", encoding="utf-8") as f:
            data = yaml.safe_load(f) or {}
        self.scenarios = data.get("events", [])
        logger.info(f"Loaded {len(self.scenarios)} scenarios from {filepath}")

    async def _queue_crossed_scenario_events(self, previous_min: int, current_min: int):
        """Queue scenario events crossed by the sole game-time driver.

        The interval may skip over an event's exact minute, so events are
        selected from (previous_min, current_min]. Each event is:
          - sent immediately as an event_notification envelope (P1 实时通道，
            供反应层即时评估)
          - also appended to _pending_audible_events (供战术层下一个 perception)
        """
        for index, event in enumerate(self.scenarios):
            if index in self._injected_scenario_events:
                continue
            event_min = int(event.get("hour", 0)) * 60 + int(event.get("minute", 0))
            if previous_min < event_min <= current_min:
                description = str(event.get("description", "")).strip()
                event_dict = {
                    "type": "scenario",
                    "source": "world_director",
                    "content": description,
                }
                # 即时通道：立即发 event_notification 给反应层
                await self._send_event_notification(event_dict)
                # 保留折入下一个 perception，供战术层参考
                self._pending_audible_events.append(event_dict)
                self._injected_scenario_events.add(index)
                logger.info(f"[SCENARIO] {event_min // 60:02d}:{event_min % 60:02d} {description}")

    # ─── Main loop ─────────────────────────────────────────────

    async def run_day(self, start_hour: int = 6, end_hour: int = 22):
        self.time.hour = start_hour
        self.time.minute = 0

        logger.info(f"=== Day {self.time.day} start ===")
        print(f"\n{'='*60}")
        print(f"  UE — Day {self.time.day} ({start_hour:02d}:00 - {end_hour:02d}:00)")
        print(f"  NPC: {self.npc.name} ({self.npc.agent_id})")
        print(f"  MCP WS: {self.mcp_ws_url}")
        print(f"  Mode: {self.mode}")
        print(f"  Time speed: {self.time.speed}x | interval: {self.perception_interval} game-min")
        print(f"{'='*60}\n")

        # Connection manager reconnects on drop; simulation runs independently
        # and drives game time. Sends buffer/no-op while disconnected (约定11).
        conn_task = asyncio.create_task(self._connection_manager())
        hb_task = asyncio.create_task(self._heartbeat_loop())
        try:
            await self._perception_loop(end_hour)
        finally:
            self._stop = True
            # Graceful unregister if currently connected.
            if self._ws is not None:
                try:
                    await self._send_agent_unregistered("actor_destroyed")
                    await self._ws.close()
                except Exception:
                    pass
            conn_task.cancel()
            hb_task.cancel()
            for t in (conn_task, hb_task):
                try:
                    await t
                except (asyncio.CancelledError, Exception):
                    pass
            logger.info("disconnected from MCP")

        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} complete | {len(self.action_log)} actions")
        print(f"  Final zone: {self.npc.current_zone} | energy: {self.npc.physical.energy:.0f}%")
        print(f"{'='*60}\n")

    async def _connection_manager(self):
        """Maintain the WS connection, reconnecting with exponential backoff.

        On each (re)connect: re-register the agent, exchange resync so both
        sides replay missed discrete messages, then read until the link drops
        (§4.2, 约定11).
        """
        backoff = RECONNECT_BASE_SEC
        first = True
        while not self._stop:
            try:
                self._ws = await websockets.connect(self.mcp_ws_url, max_size=1 << 20)
                self._connected = True
                self._ws_ready.set()
                backoff = RECONNECT_BASE_SEC
                if first:
                    logger.info(f"connected to MCP at {self.mcp_ws_url}")
                    first = False
                else:
                    logger.info(f"[RECONNECT] reconnected to MCP at {self.mcp_ws_url}")

                # Push the full world KB FIRST so MCP can merge + persist
                # + swap its in-memory KB before any agent_registered
                # starts workers. MCP only accepts this during the startup
                # window (before first agent_registered).
                await self._send_world_kb()
                # Declare NPC capabilities (cmds Mock UE implements) so MCP
                # can drive tactical-layer prompt generation and dynamic
                # tool registration. Sent every connect to refresh MCP's
                # registry (idempotent on MCP side — Register replaces).
                # Per §4.1 both system messages (world_kb + capability_registry)
                # must arrive before the first agent_registered so MCP has
                # the capability set and world model in place when workers start.
                await self._send_capability_registry()
                # Re-register all agents (§4.2). agent_registered triggers the
                # MCP Hermes session reset on the initial connect; on reconnect
                # the MCP matches by agent_id.
                await self._send_agent_registered()
                # Announce our last received seq so MCP replays what we missed.
                await self._send(TYPE_RESYNC, SYSTEM_AGENT_ID,
                                 {"last_received_seq": self._last_received_seq})
                # Push a fresh authoritative snapshot (continuous state isn't
                # replayed — latest snapshot wins, 约定11).
                await self._send_state_report()
                await self._send_perception()

                # Read until the connection drops.
                await self._read_loop()
            except asyncio.CancelledError:
                raise
            except Exception as e:
                logger.warning(f"[CONN] connection error: {e}")
            finally:
                self._connected = False
                self._ws = None

            if self._stop:
                return
            logger.info(f"[RECONNECT] retrying in {backoff:.0f}s")
            try:
                await asyncio.sleep(backoff)
            except asyncio.CancelledError:
                return
            backoff = min(backoff * 2, RECONNECT_MAX_SEC)

    async def _perception_loop(self, end_hour: int):
        last_state_report = _time.monotonic()

        # Wait for first WS connection so the initial 06:00 perception (sent
        # by _connection_manager on connect) reaches MCP. We do NOT send a
        # separate perception here — _connection_manager already pushed one
        # as part of the (re)connect handshake, and a duplicate 06:00 frame
        # wastes an LLM turn.
        await self._ws_ready.wait()

        # 固定节奏调度：以绝对时间点对齐，确保游戏时间均匀流动。
        # 初始 06:00 perception 已由 _connection_manager 在连接握手时发送，
        # 所以这里从当前时间起算，先 sleep 一个完整间隔再推进到 06:30，
        # 避免 06:00/06:30 首次双发（间隔几乎为 0）。
        #
        # next_tick 固定累加，不受单次 perception 发送耗时或 Agent 侧
        # scan 请求的影响 —— UE 端绝对掌控时间节奏。即使某次发送耗时
        # 略长（网络抖动），下一次 tick 仍会对齐到预定时间点，长期
        # 平均节奏恒定。
        interval_real_sec = self.perception_interval / max(self.time.speed, 1) * 60
        next_tick = _time.monotonic() + interval_real_sec

        while self.time.hour < end_hour:
            # Sleep 到下一个固定时间点，确保节奏均匀。
            now = _time.monotonic()
            delay = max(0, next_tick - now)
            await asyncio.sleep(delay)

            # Advance game time — SOLE time driver.
            previous_min = self.time.total_minutes
            self.time.advance(self.perception_interval)
            await self._queue_crossed_scenario_events(previous_min, self.time.total_minutes)

            # Physical evolution — MUST run before busy completion check.
            # The completion tick (game time crosses busy_until_min) is still
            # part of the busy period: the action was running for this entire
            # interval, so its composite rate (e.g. charge_at restoring
            # energy) must apply. If we _clear_busy() first, _evolve_physical
            # would see no busy action and use the passive rate, causing
            # charge_at's last tick to drain energy instead of restoring it.
            self._evolve_physical()

            # Busy completion check.
            if self.npc.busy_action_id is not None and self.time.total_minutes >= (self.npc.busy_until_min or 0):
                action_id = self.npc.busy_action_id
                started = self.npc.busy_started_ms or int(_time.time() * 1000)
                self._clear_busy()
                await self._send_action_completed(
                    action_id, RESULT_SUCCESS,
                    int(_time.time() * 1000) - started, 1.0,
                )

            # state_report: on threshold change or every 15s fallback.
            now = _time.monotonic()
            if self._physical_changed_over_threshold() or (now - last_state_report) >= STATE_REPORT_INTERVAL_SEC:
                await self._send_state_report()
                last_state_report = now

            # Perception push.
            await self._send_perception()

            # 安排下一个 tick（固定累加，不受本次处理耗时影响）。
            next_tick += interval_real_sec

    def _evolve_physical(self):
        """Gradually change physical state based on current activity.

        Rates are per game-minute (``interval`` is in game-minutes). After
        the 14-cmd migration, composite actions are keyed directly by
        busy_cmd (ChargeAtStation restores energy, other composites use the
        work-like default). Atomic busy cmds (e.g. Wait) and non-busy
        periods use the passive drain — same as pre-migration behavior.
        """
        p = self.npc.physical
        interval = self.perception_interval
        if self.npc.busy_cmd in COMPOSITE_CMDS and self.npc.busy_action_id:
            rate = PHYS_RATES.get(self.npc.busy_cmd) or PHYS_RATES["_default"]
        else:
            rate = PHYS_RATES_PASSIVE
        p.energy = max(0, min(100, p.energy + interval * rate["energy"]))
        p.fatigue = max(0, min(100, p.fatigue + interval * rate["fatigue"]))
        p.joint_wear = max(0, min(100, p.joint_wear + interval * rate["joint_wear"]))

    async def _heartbeat_loop(self):
        """Send heartbeats across the whole day, tolerating reconnects."""
        logger.info(f"[HEARTBEAT] loop started, interval={HEARTBEAT_INTERVAL_SEC}s")
        while not self._stop:
            await asyncio.sleep(HEARTBEAT_INTERVAL_SEC)
            if self._ws is None:
                continue  # disconnected; connection_manager is reconnecting
            try:
                await self._send_heartbeat()
            except Exception as e:
                # Link dropped mid-send; connection_manager handles reconnect.
                # 打 warn 日志：心跳发送失败通常是连接断开，能看到失败次数
                # 和时间点有助于诊断"UE 端是否真的在发心跳"。
                logger.warning(f"[HEARTBEAT] send failed: {e}")
                continue
        logger.info("[HEARTBEAT] loop stopped")

    async def _read_loop(self):
        """Read until the connection drops. Returns so the connection
        manager can reconnect (raises nothing on normal close)."""
        if self._ws is None:
            return
        try:
            async for raw in self._ws:
                try:
                    env = json.loads(raw)
                except json.JSONDecodeError:
                    logger.warning(f"[WS←] non-JSON: {raw[:100]}")
                    continue
                asyncio.create_task(self._handle_envelope(env))
        except websockets.ConnectionClosed:
            logger.info("WS connection closed")
        # Other exceptions propagate to connection_manager for backoff.

    # ─── Logging ──────────────────────────────────────────────

    def _setup_logging(self):
        # Mock UE no longer writes its own log file — the MCP process is
        # the single source of truth for the unified sim.log (avoids
        # drvfs O_APPEND issues and eliminates the ~90% duplication
        # between day1_*.log and sim.log for [UE→MCP]/[MCP→UE] events).
        # We keep a console handler so the operator can still watch
        # [PERCEPTION]/[STATE]/[SPEAK] summaries in real time.
        logger.setLevel(logging.INFO)
        console = logging.StreamHandler()
        console.setFormatter(logging.Formatter("[MockUE] [%(levelname)s] %(message)s"))
        logger.addHandler(console)

    def print_report(self):
        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} — Action Report")
        print(f"{'='*60}")
        for e in self.action_log:
            print(f"  {e['time']:<12} {e['action_id']:<20} {e['result']}")
        print(f"  Total: {len(self.action_log)}")
        print(f"{'='*60}\n")
