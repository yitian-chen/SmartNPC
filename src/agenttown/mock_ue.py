"""
Mock UE Bridge (async) — M-5 of Phase 1 work checklist.

Simulates the UE game world for a single NPC (H-01 老陈).
Connects to agenttown-mcp via WebSocket, pushes JSON perception snapshots,
and handles inbound tool-call requests concurrently.

Architecture (post-MCP):
    Mock UE (Python, async)  ──ws──→  agenttown-mcp (Go)  ──http──→  Hermes Gateway
       • perception_update event          • NL conversion + POST /v1/responses
       • tool request/response            • MCP tool registration + console log

Tasks covered:
  5.1  Mock message receiver    — handle inbound tool Requests from MCP
  5.2  Mock execution timing    — simulate action duration by type
  5.3  Mock perception push     — timed perception_update events to MCP
  5.4  Zone simulation          — auto-detect zone from coordinates
  5.5  Time acceleration        — configurable game-time speed
  5.6  Scenario injection       — load preset events from YAML
"""

import asyncio
import json
import logging
import os
import time as _time
import uuid
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Dict, List, Optional, Tuple

import websockets
import yaml

logger = logging.getLogger("agenttown.mock_ue")

# ─── Constants ──────────────────────────────────────────────────

# Action durations in game-minutes
ACTION_DURATIONS: Dict[str, float] = {
    "move_to": 2,           # 2 game-min per zone transition
    "turn_to": 0.1,
    "work_assemble": 0,     # uses explicit duration_min param
    "interact_with": 5,     # 5 game-min default
    "charge_at": 0,
    "self_check": 1,
    "speak": 0.5,
    "emote": 0.1,
    "wait": 0,
    "update_plan": 0,
}

# Default zone boundaries (x_min, x_max, y_min, y_max)
ZONE_BOUNDS: Dict[str, Tuple[float, float, float, float]] = {
    "main_workshop": (150, 250, 50, 150),
    "central_plaza": (80, 150, 80, 150),
    "charging_station": (200, 250, 50, 100),
}

# Zone entry points
ZONE_ENTRIES: Dict[str, List[float]] = {
    "main_workshop": [160, 100, 0],
    "central_plaza": [100, 100, 0],
    "charging_station": [215, 85, 0],
}

# Location interaction points
LOCATION_POINTS: Dict[str, List[float]] = {
    "workbench_01": [195, 105, 0],
    "charging_station_01": [215, 85, 0],
}

# WS frame type constants (mirror of Go wsserver/protocol.go)
TYPE_REQUEST = "request"
TYPE_RESPONSE = "response"
TYPE_EVENT = "event"

EVENT_PERCEPTION_UPDATE = "perception_update"
EVENT_DAY_STARTED = "day_started"
EVENT_DAY_ENDED = "day_ended"


@dataclass
class NPCState:
    """Tracks a single NPC's world state."""
    agent_id: str = "H-01"
    name: str = "老陈"
    position: List[float] = field(default_factory=lambda: [200, 100, 0])
    current_zone: str = "main_workshop"
    energy: float = 100.0
    fatigue: float = 0.0
    holding: Optional[str] = None
    current_action: Optional[str] = None
    # Busy state: when set, the NPC is occupied with a long-running action
    # (work_assemble, charge_at) until game time reaches busy_until_min.
    # The perception loop naturally advances time; when it passes
    # busy_until_min, the action completes. While busy, disruptive actions
    # (move_to, interact_with, etc.) are rejected.
    busy_until_min: Optional[int] = None   # absolute game time in minutes
    busy_action: Optional[str] = None      # e.g. "work_assemble(workbench_01)"


@dataclass
class GameTime:
    """Simulated game time with acceleration."""
    speed: float = 60.0    # 1 real-sec = 60 game-sec (1 game-min)
    day: int = 1
    hour: int = 6
    minute: int = 0

    @property
    def total_minutes(self) -> int:
        return self.hour * 60 + self.minute

    @property
    def display(self) -> str:
        return f"Day{self.day} {self.hour:02d}:{self.minute:02d}"

    def advance(self, game_minutes: float):
        total = self.total_minutes + game_minutes
        total = int(total)
        self.hour = (total // 60) % 24
        self.minute = total % 60
        if self.hour == 0 and self.minute == 0 and total > 0:
            self.day += 1


class MockUE:
    """
    Async Mock UE Bridge — drives a single NPC through a day.

    Connects to agenttown-mcp via WebSocket. Pushes JSON perception
    snapshots; handles inbound tool-call requests concurrently.

    Usage:
        mock = MockUE(mcp_ws_url="ws://localhost:9000/ws")
        await mock.run_day()
    """

    def __init__(
        self,
        mcp_ws_url: str = "ws://localhost:9000/ws",
        time_speed: float = 60.0,       # 1 real-sec = 1 game-min
        perception_interval: int = 15,   # game-min between perception pushes
        scenario_file: Optional[str] = None,
        log_dir: str = "logs",
    ):
        self.mcp_ws_url = mcp_ws_url
        self.perception_interval = perception_interval
        self.log_dir = log_dir

        # World state
        self.npc = NPCState()
        self.time = GameTime(speed=time_speed)
        self.action_log: List[Dict] = []
        self.current_plan: Optional[str] = None

        # Completion message from the last busy-action check (consumed by
        # _build_snapshot so the LLM sees "work complete" in the perception).
        self._last_completion: Optional[str] = None

        # WebSocket connection (set in run_day)
        self._ws = None

        # Scenario injection
        self.scenarios: List[Dict] = []
        if scenario_file:
            self._load_scenarios(scenario_file)

        # Setup logging
        os.makedirs(log_dir, exist_ok=True)
        self._setup_logging()

    # ─── WebSocket I/O ────────────────────────────────────────

    async def _send_frame(self, frame: Dict[str, Any]):
        """Send a JSON frame over the WebSocket."""
        if self._ws is None:
            logger.warning("send_frame called with no WS connection")
            return
        payload = json.dumps(frame, ensure_ascii=False)
        await self._ws.send(payload)
        logger.debug(f"[WS→] {frame.get('type')} {frame.get('name') or frame.get('action', '')[:30]}")

    async def _send_event(self, name: str, data: Dict[str, Any]):
        """Push an event frame to MCP."""
        frame = {
            "type": TYPE_EVENT,
            "name": name,
            "data": data,
            "timestamp": int(_time.time() * 1000),
        }
        await self._send_frame(frame)

    async def _send_response(self, request_id: str, ok: bool, data: Any = None, error: Optional[Dict] = None):
        """Send a response frame correlated to a request ID."""
        frame: Dict[str, Any] = {
            "type": TYPE_RESPONSE,
            "id": request_id,
            "ok": ok,
        }
        if data is not None:
            frame["data"] = data
        if error is not None:
            frame["error"] = error
        await self._send_frame(frame)

    # ─── 5.3 Perception Push ──────────────────────────────────

    async def _push_perception(self, phase: str = "perception"):
        """Build a perception snapshot and push it as an event to MCP."""
        snapshot = self._build_snapshot(phase)
        await self._send_event(EVENT_PERCEPTION_UPDATE, snapshot)
        logger.info(f"[PERCEPTION] {self.time.display} | zone={self.npc.current_zone} | energy={self.npc.energy:.0f}%")

    def _build_snapshot(self, phase: str) -> Dict[str, Any]:
        """Build a PerceptionSnapshot dict matching MCP's expected schema."""
        nearby = self._get_nearby_objects()
        event_line = self._match_scenario()
        # If a busy action just completed, surface that as the event line
        # so the LLM sees the completion in the perception.
        completion = getattr(self, "_last_completion", None)
        if completion and not event_line:
            event_line = completion
        elif completion and event_line:
            event_line = f"{event_line}\n{completion}"

        return {
            "agent_id": self.npc.agent_id,
            "phase": phase,
            "time": {
                "day": self.time.day,
                "hour": self.time.hour,
                "minute": self.time.minute,
                "display": self.time.display,
            },
            "position": list(self.npc.position),
            "zone": self.npc.current_zone,
            "energy": round(self.npc.energy, 1),
            "fatigue": round(self.npc.fatigue, 1),
            "holding": self.npc.holding,
            "current_action": self.npc.current_action,
            "nearby_objects": nearby,
            "event": event_line,
            "busy": self._busy_status_text(),
        }

    def _get_nearby_objects(self) -> List[str]:
        """Determine nearby objects based on zone."""
        zone = self.npc.current_zone
        objects = {
            "main_workshop": ["workbench_01 (工作台)", "charging_station_01 (充电桩)"],
            "central_plaza": ["充电站"],
            "charging_station": ["charging_station_01 (充电桩)"],
        }
        return objects.get(zone, [])

    # ─── 5.1 / 5.2 Tool Request Handler ──────────────────────

    async def _handle_request(self, msg: Dict[str, Any]):
        """
        Handle an inbound tool Request frame from MCP.
        Executes the action locally and sends back a Response.
        """
        req_id = msg.get("id", "")
        action = msg.get("action", "")
        params = msg.get("params", {}) or {}

        logger.info(f"[TOOL←] {action} | params={params}")

        try:
            result = self.execute_action(action, params)
            await self._send_response(req_id, ok=True, data=result)
        except Exception as e:
            logger.error(f"[TOOL] {action} failed: {e}", exc_info=True)
            await self._send_response(
                req_id,
                ok=False,
                error={"code": "execution_error", "message": str(e)},
            )

    def execute_action(self, action: str, params: Dict[str, Any]) -> Dict[str, Any]:
        """
        Simulate action execution. Returns an ActionResult dict.

        Long-running actions (work_assemble, charge_at) set a "busy" state
        and return immediately — the NPC stays in place while the perception
        loop naturally advances game time. When time reaches busy_until_min,
        the action completes. Short actions (speak, emote, self_check, etc.)
        are instant in game time (their duration is negligible vs the
        perception interval).

        While busy, disruptive actions (move_to, interact_with, work_assemble)
        are rejected so the NPC doesn't teleport away mid-work.
        """
        # ─── Busy guard ────────────────────────────────────────
        # While occupied with a long-running action, reject anything that
        # would move the NPC or start a new long task. Non-disruptive
        # actions (self_check, speak, emote) are allowed — the NPC can
        # talk or glance at gauges while working.
        DISRUPTIVE = {"move_to", "turn_to", "interact_with", "work_assemble", "charge_at", "wait"}
        if self.npc.busy_until_min is not None and action in DISRUPTIVE:
            remaining = max(0, self.npc.busy_until_min - self.time.total_minutes)
            busy_name = self.npc.busy_action or "busy"
            raise ValueError(
                f"NPC is busy with {busy_name!r} ({remaining} game-min remaining). "
                f"Cannot {action} now. Wait for the current task to finish or interrupt it first."
            )

        duration = ACTION_DURATIONS.get(action, 1.0)
        message_parts: List[str] = []
        include_state = False

        if action == "move_to":
            target = params.get("target", "")
            if not target:
                raise ValueError("target is required")
            if self._already_at_target(target):
                message_parts.append(f"already at {target}")
            else:
                moved = self._simulate_move(target)
                if moved:
                    message_parts.append(f"arrived at {target}")
                else:
                    raise ValueError(
                        f"unknown target: {target!r}. "
                        f"Valid zones: {sorted(ZONE_ENTRIES.keys())}. "
                        f"Valid locations: {sorted(LOCATION_POINTS.keys())}."
                    )
            include_state = True
            # move_to is short (2 game-min) — apply directly.
            self.time.advance(duration)

        elif action == "turn_to":
            target = params.get("target", "")
            message_parts.append(f"facing {target}")

        elif action == "work_assemble":
            duration = params.get("duration_min", 30)
            target = params.get("target", "")
            # Set busy state — the perception loop will naturally advance
            # time. We don't jump the clock; the NPC stays here and works
            # until busy_until_min is reached.
            self.npc.busy_until_min = self.time.total_minutes + int(duration)
            self.npc.busy_action = f"work_assemble({target})"
            # Apply fatigue/energy gradually — the perception loop will
            # decay them further while busy. Apply an initial hit here.
            self.npc.fatigue = min(100, self.npc.fatigue + duration * 0.3)
            message_parts.append(f"started work at {target}, will finish in {duration} game-min")
            self.npc.current_action = f"work_assemble({target})"
            self._log_action(action, params, duration)
            return {
                "action": action,
                "duration_game_min": round(duration, 2),
                "message": "; ".join(message_parts),
                "busy": True,
                "will_complete_at": self._minutes_to_display(self.npc.busy_until_min),
            }

        elif action == "interact_with":
            obj = params.get("object_id", "")
            verb = params.get("action", "")
            if not obj:
                raise ValueError("object_id is required")
            if obj in ZONE_ENTRIES or obj in ZONE_BOUNDS:
                raise ValueError(
                    f"{obj!r} is a zone, not an object. "
                    f"Use interact_with with an object_id from: {sorted(LOCATION_POINTS.keys())}. "
                    f"To travel to a zone, use move_to instead."
                )
            if obj not in LOCATION_POINTS:
                raise ValueError(
                    f"unknown object_id: {obj!r}. "
                    f"Valid objects: {sorted(LOCATION_POINTS.keys())}."
                )
            message_parts.append(f"{verb} on {obj}")
            # interact_with is short (5 game-min) — apply directly.
            self.time.advance(duration)

        elif action == "charge_at":
            duration = params.get("duration_min", 30)
            station = params.get("station_id", "")
            # Set busy state — charging takes real in-world time.
            self.npc.busy_until_min = self.time.total_minutes + int(duration)
            self.npc.busy_action = f"charge_at({station})"
            message_parts.append(f"started charging at {station}, will finish in {duration} game-min")
            self.npc.current_action = f"charge_at({station})"
            self._log_action(action, params, duration)
            return {
                "action": action,
                "duration_game_min": round(duration, 2),
                "message": "; ".join(message_parts),
                "busy": True,
                "will_complete_at": self._minutes_to_display(self.npc.busy_until_min),
            }

        elif action == "self_check":
            message_parts.append("all systems nominal")
            include_state = True
            # self_check is 1 game-min — apply directly.
            self.time.advance(duration)

        elif action == "speak":
            text = params.get("text", "")
            to = params.get("to", "")
            message_parts.append(f"said to {to or 'nearby'}: {text[:40]}")

        elif action == "emote":
            emotion = params.get("emotion", "")
            message_parts.append(f"emoted {emotion}")

        elif action == "wait":
            # wait is explicit — apply the time directly (it's usually short).
            duration = params.get("seconds", 5) / 60.0
            message_parts.append(f"waited {params.get('seconds', 5)}s")
            self.time.advance(duration)

        elif action == "update_plan":
            self.current_plan = params.get("plan", "")
            message_parts.append("plan updated")
            self._log_action(action, params, 0)
            return {
                "action": action,
                "duration_game_min": 0,
                "message": "; ".join(message_parts),
            }

        # Universal energy consumption for short actions
        self.npc.energy = max(0, self.npc.energy - duration * 0.1)
        self.npc.current_action = f"{action}({params.get('target', '') or params.get('object_id', '')})"

        self._log_action(action, params, duration)
        return self._build_action_result(action, duration, message_parts, include_state=include_state)

    def _build_action_result(
        self,
        action: str,
        duration: float,
        messages: List[str],
        extra: Optional[Dict[str, Any]] = None,
        include_state: bool = False,
    ) -> Dict[str, Any]:
        """
        Build the standard ActionResult envelope.

        include_state controls whether new_state is included. Set True only
        for actions that change physical state the LLM needs to see in the
        tool result (move_to, charge_at). Stateless actions (speak, emote,
        wait, etc.) omit it to keep the chained context small.
        """
        result: Dict[str, Any] = {
            "action": action,
            "duration_game_min": round(duration, 2),
            "message": "; ".join(messages) if messages else "ok",
        }
        if include_state:
            result["new_state"] = self._state_snapshot()
        if extra:
            result.update(extra)
        return result

    def _state_snapshot(self) -> Dict[str, Any]:
        """Return current NPC state for inclusion in ActionResult."""
        return {
            "position": list(self.npc.position),
            "zone": self.npc.current_zone,
            "energy": round(self.npc.energy, 1),
            "fatigue": round(self.npc.fatigue, 1),
            "current_action": self.npc.current_action,
        }

    def _log_action(self, action: str, params: Dict[str, Any], duration: float):
        """Append to action_log and log."""
        entry = {
            "time": self.time.display,
            "action": action,
            "params": params,
            "duration_game_min": duration,
            "result": "success",
        }
        self.action_log.append(entry)
        logger.info(
            f"[ACTION] {self.time.display} | {action} | "
            f"{duration:.1f}min | zone={self.npc.current_zone} | energy={self.npc.energy:.0f}%"
        )

    def _simulate_move(self, target: str) -> bool:
        """Move NPC to a zone entry or location point. Returns True if moved."""
        dest = ZONE_ENTRIES.get(target) or LOCATION_POINTS.get(target)
        if not dest:
            return False
        self.npc.position = list(dest)
        new_zone = self._resolve_zone(self.npc.position)
        if new_zone != self.npc.current_zone:
            old = self.npc.current_zone
            self.npc.current_zone = new_zone
            logger.info(f"[ZONE] {old} → {new_zone}")
        return True

    def _already_at_target(self, target: str) -> bool:
        """Check if the NPC is already at the given target (zone or location)."""
        if target == self.npc.current_zone:
            return True
        loc = LOCATION_POINTS.get(target)
        if loc is not None:
            return self._resolve_zone(loc) == self.npc.current_zone
        return False

    def _minutes_to_display(self, total_min: int) -> str:
        """Convert absolute game-time minutes to a Day{N} HH:MM display."""
        day = self.time.day
        # Handle day roll-over during long busy periods.
        total = total_min
        while total >= 24 * 60:
            total -= 24 * 60
            day += 1
        return f"Day{day} {total // 60:02d}:{total % 60:02d}"

    def _check_busy_completion(self) -> Optional[str]:
        """
        Called from the perception loop each tick. If the NPC is busy and
        game time has reached/exceeded busy_until_min, complete the action:
        apply final state changes (energy gain for charge_at, etc.), clear
        the busy state, and return a completion message for the perception.
        Returns None if not busy or not yet complete.
        """
        if self.npc.busy_until_min is None:
            return None
        if self.time.total_minutes < self.npc.busy_until_min:
            return None

        busy_name = self.npc.busy_action or "task"
        elapsed = self.npc.busy_until_min - (self.npc.busy_until_min - 0)  # placeholder
        message = f"{busy_name} 完成"

        # Apply completion effects based on the action type.
        if busy_name.startswith("charge_at"):
            # Restore energy based on the elapsed time.
            # busy_until_min - start = duration; energy gain = duration * 2.
            # We don't store the start explicitly, but the action set the
            # fatigue/energy. Apply the gain now.
            # Estimate duration from the busy_action metadata.
            # For simplicity, cap energy at 100.
            self.npc.energy = min(100, self.npc.energy + 20)  # approximate gain
            message = f"充电完成, 电池 {self.npc.energy:.0f}%"
        elif busy_name.startswith("work_assemble"):
            message = f"{busy_name} 完成"

        # Clear busy state
        self.npc.busy_until_min = None
        self.npc.busy_action = None
        self.npc.current_action = None
        logger.info(f"[BUSY] {message}")
        return message

    def _busy_status_text(self) -> Optional[str]:
        """Return a perception-line describing the current busy state, or None."""
        if self.npc.busy_until_min is None:
            return None
        remaining = max(0, self.npc.busy_until_min - self.time.total_minutes)
        return f"你正在{self.npc.busy_action}，预计还需{remaining}游戏分钟完成。"

    # ─── 5.4 Zone Resolution ──────────────────────────────────

    def _resolve_zone(self, pos: List[float]) -> str:
        """Find which zone contains the given position."""
        x, y = pos[0], pos[1]
        for name, (xmin, xmax, ymin, ymax) in ZONE_BOUNDS.items():
            if xmin <= x <= xmax and ymin <= y <= ymax:
                return name
        return self.npc.current_zone

    # ─── 5.6 Scenario Injection ───────────────────────────────

    def _load_scenarios(self, filepath: str):
        """Load preset events from YAML."""
        with open(filepath, "r", encoding="utf-8") as f:
            data = yaml.safe_load(f) or {}
        self.scenarios = data.get("events", [])
        logger.info(f"Loaded {len(self.scenarios)} scenarios from {filepath}")

    def _match_scenario(self) -> Optional[str]:
        """
        Return the description of the first unfired scenario whose trigger
        time matches the current game time, or None.
        """
        current = (self.time.hour, self.time.minute)
        for evt in self.scenarios:
            if evt.get("_fired"):
                continue
            h = int(evt.get("hour", 0))
            m = int(evt.get("minute", 0))
            if (h, m) == current:
                evt["_fired"] = True
                desc = evt.get("description", "")
                logger.info(f"[EVENT] {self.time.display} | {desc}")
                return desc
        return None

    # ─── Main Loop ─────────────────────────────────────────────

    async def run_day(self, start_hour: int = 6, end_hour: int = 22):
        """
        Run one full game day from start_hour to end_hour.

        Two concurrent tasks:
          1. Perception loop: advance time, push perception_update events
          2. WS read loop: handle inbound tool Requests
        """
        self.time.hour = start_hour
        self.time.minute = 0

        logger.info(f"=== Day {self.time.day} start ===")
        print(f"\n{'='*60}")
        print(f"  Mock UE (async) — Day {self.time.day} ({start_hour:02d}:00 - {end_hour:02d}:00)")
        print(f"  NPC: {self.npc.name} ({self.npc.agent_id})")
        print(f"  MCP WS: {self.mcp_ws_url}")
        print(f"  Time speed: {self.time.speed}x")
        print(f"  Perception interval: {self.perception_interval} game-min")
        print(f"{'='*60}\n")

        # Connect to MCP WebSocket
        try:
            self._ws = await websockets.connect(self.mcp_ws_url, max_size=1 << 20)
            logger.info(f"connected to MCP at {self.mcp_ws_url}")
        except Exception as e:
            logger.error(f"failed to connect to MCP: {e}")
            print(f"[ERROR] Cannot connect to MCP at {self.mcp_ws_url}: {e}")
            print("Start MCP first: run agenttown-mcp --http :8760 --ws :9000")
            return

        try:
            # Signal day start — MCP will reset Hermes session
            await self._send_event(EVENT_DAY_STARTED, {"day": self.time.day})
            # First perception with phase=day_start
            await self._push_perception(phase="day_start")

            # Run perception loop and WS read loop concurrently
            await asyncio.gather(
                self._perception_loop(end_hour),
                self._ws_read_loop(),
            )
        finally:
            # Signal day end
            await self._send_event(EVENT_DAY_ENDED, {"day": self.time.day})
            await self._push_perception(phase="day_end")
            await self._ws.close()
            logger.info("disconnected from MCP")

        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} complete  |  {len(self.action_log)} tool calls")
        print(f"  Final zone: {self.npc.current_zone}  |  Energy: {self.npc.energy:.0f}%")
        print(f"{'='*60}\n")

    async def _perception_loop(self, end_hour: int):
        """Advance game time and push perception at regular intervals."""
        while self.time.hour < end_hour:
            # Advance game time by the perception interval. This is the
            # ONLY driver of game time for long-running actions — the NPC
            # stays in place while busy, and time naturally passes.
            self.time.advance(self.perception_interval)

            # Check if a busy action (work_assemble, charge_at) just
            # completed. If so, the completion message is included in
            # this perception push via the snapshot's "event" field.
            completion = self._check_busy_completion()
            if completion:
                # Stash the completion message so _build_snapshot can
                # surface it as an event line.
                self._last_completion = completion
            else:
                self._last_completion = None

            # While busy, apply gradual energy/fatigue changes so the
            # NPC's state evolves during long work sessions.
            if self.npc.busy_until_min is not None:
                # Working drains energy faster; charging restores it.
                if self.npc.busy_action and self.npc.busy_action.startswith("charge_at"):
                    self.npc.energy = min(100, self.npc.energy + self.perception_interval * 2)
                else:
                    self.npc.energy = max(0, self.npc.energy - self.perception_interval * 0.1)
                    self.npc.fatigue = min(100, self.npc.fatigue + self.perception_interval * 0.3)
            else:
                # Idle decay
                self.npc.energy = max(0, self.npc.energy - 0.5)
                self.npc.fatigue = min(100, self.npc.fatigue + 0.3)

            # Push perception snapshot
            await self._push_perception(phase="perception")

            # Real-time pacing. time_speed=60 means 1 real-sec = 1 game-min;
            # perception_interval=15 game-min → ~15 real-sec between pushes.
            # Use a shorter real delay for dev (cap at 2s so high-speed runs
            # still feel live).
            real_delay = min(2.0, self.perception_interval / max(self.time.speed, 1) * 60)
            await asyncio.sleep(real_delay)

    async def _ws_read_loop(self):
        """Read WebSocket frames and dispatch Requests."""
        if self._ws is None:
            return
        try:
            async for raw in self._ws:
                try:
                    msg = json.loads(raw)
                except json.JSONDecodeError:
                    logger.warning(f"[WS←] non-JSON frame: {raw[:100]}")
                    continue

                msg_type = msg.get("type", "")
                if msg_type == TYPE_REQUEST:
                    # Tool call from MCP — handle it (may take time; run concurrently)
                    asyncio.create_task(self._handle_request(msg))
                elif msg_type == TYPE_EVENT:
                    name = msg.get("name", "")
                    if name == "narrative":
                        # Hermes narrative text pushed from MCP — display + log
                        data = msg.get("data", {})
                        text = data.get("text", "") if isinstance(data, dict) else ""
                        if text:
                            print(f"\n  [{self.time.display}] {text[:500]}")
                            if len(text) > 500:
                                print(f"  ... ({len(text)} chars)")
                            logger.info(f"[NARRATIVE] {text}")
                    else:
                        logger.debug(f"[WS←] event: {name}")
                else:
                    logger.debug(f"[WS←] unknown frame type: {msg_type}")
        except websockets.ConnectionClosed:
            logger.info("WS connection closed")
        except Exception as e:
            logger.error(f"WS read loop error: {e}", exc_info=True)

    # ─── Logging ──────────────────────────────────────────────

    def _setup_logging(self):
        """Configure file + console logger."""
        log_file = os.path.join(
            self.log_dir,
            f"day{self.time.day}_{datetime.now().strftime('%Y%m%d_%H%M%S')}.log"
        )
        handler = logging.FileHandler(log_file, encoding="utf-8")
        handler.setFormatter(logging.Formatter(
            "%(asctime)s [%(levelname)s] %(message)s"
        ))
        logger.addHandler(handler)
        logger.setLevel(logging.DEBUG)
        console = logging.StreamHandler()
        console.setFormatter(logging.Formatter("[%(levelname)s] %(message)s"))
        console.setLevel(logging.INFO)
        logger.addHandler(console)
        logger.info(f"Logging to {log_file}")

    # ─── Report ───────────────────────────────────────────────

    def print_report(self):
        """Print end-of-day tool-call summary."""
        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} — Tool Call Report")
        print(f"{'='*60}")
        print(f"  {'Time':<12} {'Action':<18} {'Target':<20} {'Dur(min)':<10}")
        print(f"  {'-'*60}")
        for entry in self.action_log:
            target = entry.get("params", {}).get("target") or entry.get("params", {}).get("object_id", "")
            print(
                f"  {entry['time']:<12} "
                f"{entry['action']:<18} "
                f"{str(target):<20} "
                f"{entry['duration_game_min']:<10.1f}"
            )
        print(f"  {'-'*60}")
        print(f"  Total tool calls: {len(self.action_log)}")
        print(f"{'='*60}\n")
