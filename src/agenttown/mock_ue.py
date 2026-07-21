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

# cmd constants
CMD_MOVE_TO = "MoveTo"
CMD_TURN_TO = "TurnTo"
CMD_PLAY_ANIMATION = "PlayAnimation"
CMD_SPEAK = "Speak"
CMD_EMOTE = "Emote"
CMD_WAIT = "Wait"
CMD_INTERACT = "InteractSmartObject"
CMD_EXECUTE_COMPOSITE = "ExecuteComposite"
CMD_STOP = "Stop"

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
    name: str
    entry_point: List[float]
    bounds: Tuple[float, float, float, float]  # (x_min, x_max, y_min, y_max)
    locations: List[str] = field(default_factory=list)


@dataclass
class LocationInfo:
    """One location entry from world_kb.yaml."""
    id: str
    name: str
    zone: str
    position: List[float]
    interaction_point: List[float]
    interaction_radius: float
    available_actions: List[str] = field(default_factory=list)


@dataclass
class ObjectInfo:
    """One object entry from world_kb.yaml (mirrors a location for now)."""
    id: str
    available_actions: List[str] = field(default_factory=list)


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
    locations: Dict[str, LocationInfo] = field(default_factory=dict)
    objects: Dict[str, ObjectInfo] = field(default_factory=dict)

    def zone_entry(self, zone_id: str) -> Optional[List[float]]:
        z = self.zones.get(zone_id)
        return list(z.entry_point) if z else None

    def location_point(self, loc_id: str) -> Optional[List[float]]:
        loc = self.locations.get(loc_id)
        return list(loc.interaction_point) if loc else None

    def resolve_position(self, target: str) -> Optional[List[float]]:
        """Resolve a semantic ID to a coordinate. Zones use entry_point,
        locations use interaction_point. Returns None if unknown."""
        return self.zone_entry(target) or self.location_point(target)

    def is_target(self, target: str) -> bool:
        return target in self.zones or target in self.locations

    def which_zone(self, pos: List[float]) -> str:
        """Reverse-lookup zone by position via AABB (X/Y only)."""
        x, y = pos[0], pos[1]
        for zid, z in self.zones.items():
            xmin, xmax, ymin, ymax = z.bounds
            if xmin <= x <= xmax and ymin <= y <= ymax:
                return zid
        return ""

    def which_location(self, pos: List[float]) -> Optional[str]:
        """Reverse-lookup location by position via interaction_radius (X/Y only)."""
        x, y = pos[0], pos[1]
        for lid, loc in self.locations.items():
            if loc.interaction_radius <= 0:
                continue
            dx, dy = x - loc.position[0], y - loc.position[1]
            if (dx * dx + dy * dy) <= loc.interaction_radius * loc.interaction_radius:
                return lid
        return None

    def objects_in_zone(self, zone_id: str) -> List[str]:
        """Return location/object IDs registered under a zone."""
        return list(self.zones.get(zone_id, ZoneInfo(id="", name="", entry_point=[], bounds=(0, 0, 0, 0))).locations)


def load_world_kb(path: str) -> WorldKB:
    """Load world_kb.yaml into a WorldKB instance.

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
        bounds_raw = raw_zone.get("ue5_bounds", {}) or {}
        center = bounds_raw.get("center", [0, 0, 0])
        half = bounds_raw.get("half_size", [0, 0, 0])
        # AABB: (x_min, x_max, y_min, y_max) — Z ignored for flat worlds.
        bounds = (
            center[0] - half[0], center[0] + half[0],
            center[1] - half[1], center[1] + half[1],
        )
        kb.zones[zid] = ZoneInfo(
            id=zid,
            name=raw_zone.get("name", zid),
            entry_point=list(raw_zone.get("entry_point", [0, 0, 0])),
            bounds=bounds,
            locations=list(raw_zone.get("locations", []) or []),
        )

    for raw_loc in data.get("locations", []) or []:
        lid = raw_loc.get("id", "")
        if not lid:
            raise ValueError(f"location missing id in {path}: {raw_loc}")
        if lid in kb.locations:
            raise ValueError(f"duplicate location id {lid!r} in {path}")
        kb.locations[lid] = LocationInfo(
            id=lid,
            name=raw_loc.get("name", lid),
            zone=raw_loc.get("zone", ""),
            position=list(raw_loc.get("position", [0, 0, 0])),
            interaction_point=list(raw_loc.get("interaction_point", [0, 0, 0])),
            interaction_radius=float(raw_loc.get("interaction_radius", 0)),
            available_actions=list(raw_loc.get("available_actions", []) or []),
        )

    for raw_obj in data.get("objects", []) or []:
        oid = raw_obj.get("id", "")
        if not oid:
            raise ValueError(f"object missing id in {path}: {raw_obj}")
        if oid in kb.objects:
            raise ValueError(f"duplicate object id {oid!r} in {path}")
        kb.objects[oid] = ObjectInfo(
            id=oid,
            available_actions=list(raw_obj.get("available_actions", []) or []),
        )

    return kb

# Composite action nominal durations (seconds) when not given explicitly
COMPOSITE_DEFAULT_SEC = 1800.0  # 30 min

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
        mcp_ws_url: str = "ws://localhost:9090/ws",
        mode: str = "normal",
        time_speed: float = 300.0,
        perception_interval: int = 30,   # game-minutes between perception pushes
        scenario_file: Optional[str] = None,
        log_dir: str = "logs",
        world_kb_path: str = "assets/world_kb.yaml",
    ):
        self.mcp_ws_url = mcp_ws_url
        self.mode = mode
        self.perception_interval = perception_interval
        self.log_dir = log_dir

        # World KB — single source of truth for zone/location/object data.
        # Fail-fast: Mock UE cannot resolve semantic targets without it.
        try:
            self.kb = load_world_kb(world_kb_path)
        except Exception as e:
            raise SystemExit(f"[FATAL] failed to load world_kb from {world_kb_path!r}: {e}")
        logger.info(f"[KB] loaded {len(self.kb.zones)} zones, {len(self.kb.locations)} locations, "
                    f"{len(self.kb.objects)} objects from {world_kb_path}")

        # Seed NPC defaults. KB does not yet expose agent defaults, so we
        # use the long-standing defaults (H-01 in main_workshop). When KB
        # gains agent entries, this can be driven from there.
        self.npc = NPCState(current_zone="main_workshop", position=[20000.0, 10000.0, 0.0])
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

    async def _send_agent_unregistered(self, reason: str):
        await self._send(TYPE_AGENT_UNREGISTERED, self.npc.agent_id, {"reason": reason})

    async def _send_heartbeat(self):
        uptime = (int(_time.time() * 1000) - self._started_ms) // 1000
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
                "time_of_day": self.time.time_of_day,
                "weather": "clear",
            },
        }

    def _nearby_objects(self) -> List[Dict[str, Any]]:
        objs = []
        for oid in self.kb.objects_in_zone(self.npc.current_zone):
            loc = self.kb.locations.get(oid)
            obj = self.kb.objects.get(oid)
            # Prefer location name (中文), fall back to ID.
            name = (loc.name if loc else "") or oid
            actions = (loc.available_actions if loc else None) or (obj.available_actions if obj else [])
            objs.append({
                "id": oid,
                "name": name,
                "distance": 8.0,
                "state": "idle",
                "available_actions": list(actions),
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
        DISRUPTIVE = {CMD_MOVE_TO, CMD_TURN_TO, CMD_INTERACT, CMD_EXECUTE_COMPOSITE, CMD_WAIT}
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
            self.npc.busy_until_min = self.time.total_minutes + busy_game_min
            self.npc.busy_started_ms = int(_time.time() * 1000)
            self.npc.current_animation = "work"
            self._apply_command_effects(cmd, params, starting=True)
        else:
            # Short action: apply effects immediately and complete now.
            self._apply_command_effects(cmd, params, starting=False)
            await self._send_action_completed(action_id, RESULT_SUCCESS, 0, 1.0)

    def _validate_target(self, cmd: str, params: Dict[str, Any]) -> str:
        """Return a non-empty rejection reason when targeting a non‑existent
        zone, location, object, or route.  An empty return means valid."""
        if cmd == CMD_MOVE_TO:
            # move_to (方案 A): MCP 层已解析坐标，UE 只校验 dest 是 3 元数组。
            dest = params.get("dest")
            if not isinstance(dest, list) or len(dest) != 3:
                return f"move_to requires dest:[x,y,z], got: {dest!r}"
            for v in dest:
                if not isinstance(v, (int, float)):
                    return f"move_to dest entries must be numeric, got: {dest!r}"
        elif cmd == CMD_INTERACT:
            obj = params.get("object_id", "")
            if obj and obj not in self.kb.objects:
                return f"unknown object: {obj} (available: {list(self.kb.objects)})"
        elif cmd == CMD_EXECUTE_COMPOSITE:
            name = params.get("name", "")
            if name == "patrol_route":
                route = params.get("route_id", "")
                if route and not self.kb.is_target(route):
                    return f"unknown patrol route: {route}"
            elif name == "work_assemble":
                target = params.get("target", "")
                if target and target not in self.kb.objects:
                    return f"unknown workbench: {target}"
            elif name == "charge_at":
                station = params.get("station_id", "")
                if station and not self.kb.is_target(station):
                    return f"unknown charging station: {station}"
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
        if cmd == CMD_EXECUTE_COMPOSITE:
            dur = float(params.get("duration_sec", COMPOSITE_DEFAULT_SEC))
            return dur, True
        if cmd == CMD_WAIT:
            return float(params.get("duration_sec", 5)), False
        if cmd == CMD_MOVE_TO:
            return 120.0, False   # ~2 min walk
        if cmd == CMD_INTERACT:
            return 300.0, False   # ~5 min
        # TurnTo/Speak/Emote/Stop: near-instant
        return 1.0, False

    def _apply_command_effects(self, cmd: str, params: Dict[str, Any], starting: bool):
        """Mutate NPC state for a command. Does NOT advance game time."""
        if cmd == CMD_MOVE_TO:
            # 方案 A: MCP 已解析坐标，UE 直接用 dest。
            # target+kind 是 metadata，用于精确设置 current_location。
            dest = params.get("dest")
            target = params.get("target", "")
            kind = params.get("kind", "")
            if isinstance(dest, list) and len(dest) == 3:
                self._move_to(list(dest), target=target or None, kind=kind or None)
        elif cmd == CMD_TURN_TO:
            self.npc.rotation[1] = (self.npc.rotation[1] + 90.0) % 360.0
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
        elif cmd == CMD_EXECUTE_COMPOSITE:
            name = params.get("name", "")
            if name == "charge_at":
                pass  # energy restored gradually in loop
            elif name in ("work_assemble", "archive_research", "repair_target"):
                pass  # fatigue accrues in loop
        # Physical drain applied gradually in perception loop.

    def _move_to(self, dest: List[float], target: Optional[str] = None, kind: Optional[str] = None) -> bool:
        """Move NPC to dest coordinate.

        方案 A: dest 是权威坐标（MCP 层解析自 world_kb.yaml）。
        target+kind 是 metadata：
          - kind=="location" 且 target 非空 → 直接设置 current_location = target
          - 否则用 _resolve_location(pos) 基于坐标距离反推
        """
        if not dest or len(dest) != 3:
            logger.warning(f"[MOVE] invalid dest {dest!r}")
            return False
        old_pos = self.npc.position
        # Face the movement direction (yaw).
        dx, dy = dest[0] - old_pos[0], dest[1] - old_pos[1]
        if dx or dy:
            import math
            self.npc.rotation[1] = (math.degrees(math.atan2(dy, dx))) % 360.0
        self.npc.position = list(dest)

        # Resolve current_location: prefer explicit metadata, else reverse-lookup.
        if kind == "location" and target:
            self.npc.current_location = target
        else:
            self.npc.current_location = self.kb.which_location(self.npc.position)

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

    def _queue_crossed_scenario_events(self, previous_min: int, current_min: int):
        """Queue scenario events crossed by the sole game-time driver.

        The interval may skip over an event's exact minute, so events are
        selected from (previous_min, current_min]. Each event is injected once
        and consumed by the next perception_update as an audible event.
        """
        for index, event in enumerate(self.scenarios):
            if index in self._injected_scenario_events:
                continue
            event_min = int(event.get("hour", 0)) * 60 + int(event.get("minute", 0))
            if previous_min < event_min <= current_min:
                description = str(event.get("description", "")).strip()
                self._pending_audible_events.append({
                    "type": "scenario",
                    "source": "world_director",
                    "content": description,
                })
                self._injected_scenario_events.add(index)
                logger.info(f"[SCENARIO] {event_min // 60:02d}:{event_min % 60:02d} {description}")

    # ─── Main loop ─────────────────────────────────────────────

    async def run_day(self, start_hour: int = 6, end_hour: int = 22):
        self.time.hour = start_hour
        self.time.minute = 0

        logger.info(f"=== Day {self.time.day} start ===")
        print(f"\n{'='*60}")
        print(f"  Mock UE — Day {self.time.day} ({start_hour:02d}:00 - {end_hour:02d}:00)")
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

        while self.time.hour < end_hour:
            # Advance game time — SOLE time driver.
            previous_min = self.time.total_minutes
            self.time.advance(self.perception_interval)
            self._queue_crossed_scenario_events(previous_min, self.time.total_minutes)

            # Busy completion check.
            if self.npc.busy_action_id is not None and self.time.total_minutes >= (self.npc.busy_until_min or 0):
                action_id = self.npc.busy_action_id
                started = self.npc.busy_started_ms or int(_time.time() * 1000)
                self._clear_busy()
                await self._send_action_completed(
                    action_id, RESULT_SUCCESS,
                    int(_time.time() * 1000) - started, 1.0,
                )

            # Physical evolution.
            self._evolve_physical()

            # state_report: on threshold change or every 15s fallback.
            now = _time.monotonic()
            if self._physical_changed_over_threshold() or (now - last_state_report) >= STATE_REPORT_INTERVAL_SEC:
                await self._send_state_report()
                last_state_report = now

            # Perception push.
            await self._send_perception()

            # Real-time pacing (speed controls the pace, no artificial cap).
            real_delay = self.perception_interval / max(self.time.speed, 1) * 60
            await asyncio.sleep(real_delay)

    def _evolve_physical(self):
        """Gradually change physical state based on current activity."""
        p = self.npc.physical
        interval = self.perception_interval
        if self.npc.busy_cmd == CMD_EXECUTE_COMPOSITE and self.npc.busy_action_id:
            # Charging vs working determined loosely; charge_at busy raises energy.
            # We don't track composite name post-start here; approximate by wear.
            p.energy = max(0, p.energy - interval * 0.05)
            p.fatigue = min(100, p.fatigue + interval * 0.2)
            p.joint_wear = min(100, p.joint_wear + interval * 0.05)
        else:
            p.energy = max(0, p.energy - interval * 0.02)
            p.fatigue = min(100, p.fatigue + interval * 0.05)

    async def _heartbeat_loop(self):
        """Send heartbeats across the whole day, tolerating reconnects."""
        while not self._stop:
            await asyncio.sleep(HEARTBEAT_INTERVAL_SEC)
            if self._ws is None:
                continue  # disconnected; connection_manager is reconnecting
            try:
                await self._send_heartbeat()
            except Exception:
                # Link dropped mid-send; connection_manager handles reconnect.
                continue

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
